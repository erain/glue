// Command glue is the local CLI runner for Glue agents.
//
// Run a single prompt:
//
//	glue run --prompt "Say hi" --id local-dev --store .glue/sessions
//
// The default subcommand uses Glue's Gemini-backed agent. Streaming text
// is written to stdout; provider, store, or flag errors exit non-zero.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	"github.com/erain/glue/providers/gemini"
	filestore "github.com/erain/glue/stores/file"
)

const defaultModel = "gemini-2.5-flash"
const defaultListenAddr = "127.0.0.1:0"
const defaultShutdownTimeout = 5 * time.Second

// providerFactory returns a [glue.Provider] or an error. The error is the
// hook the default factory uses to surface "GEMINI_API_KEY missing"
// before any API call is attempted.
type providerFactory func() (glue.Provider, error)

type envFiles []string

func (e *envFiles) String() string { return strings.Join(*e, ",") }

func (e *envFiles) Set(value string) error {
	*e = append(*e, value)
	return nil
}

type serveFunc func(context.Context, serveConfig, http.Handler, io.Writer) error

type serveConfig struct {
	ListenAddr        string
	Token             string
	TokenSource       string
	MetadataPath      string
	Model             string
	StoreDir          string
	WorkDir           string
	PermissionTimeout time.Duration
	ShutdownTimeout   time.Duration
}

type daemonMetadata struct {
	Version int    `json:"version"`
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
	PID     int    `json:"pid"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultGeminiFactory))
}

func defaultGeminiFactory() (glue.Provider, error) {
	if strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) == "" {
		return nil, errors.New("GEMINI_API_KEY is required (set in shell or pass --env <file>)")
	}
	return gemini.New(gemini.Options{}), nil
}

func runCLI(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory) int {
	return runCLIWithServe(ctx, args, stdout, stderr, newProvider, serveDaemon)
}

func runCLIWithServe(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory, serve serveFunc) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printUsage(stdout)
		return 0
	}

	switch args[0] {
	case "run":
		if err := runCommand(ctx, args[1:], stdout, stderr, newProvider); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "serve":
		if err := serveCommand(ctx, args[1:], stdout, stderr, newProvider, serve); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 1
	}
}

func runCommand(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory) error {
	flags := flag.NewFlagSet("glue run", flag.ContinueOnError)
	flags.SetOutput(stderr)

	id := flags.String("id", "default", "session id")
	prompt := flags.String("prompt", "", "prompt text")
	model := flags.String("model", defaultModel, "model id or gemini/<model>")
	storeDir := flags.String("store", ".glue/sessions", "session store directory")
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")

	if err := flags.Parse(args); err != nil {
		return err
	}

	agentName := "default"
	if flags.NArg() > 0 {
		agentName = flags.Arg(0)
	}
	if agentName != "default" && agentName != "gemini" {
		return fmt.Errorf("unknown agent %q; only 'default' is available in this runner", agentName)
	}
	if strings.TrimSpace(*prompt) == "" {
		return errors.New("missing required --prompt")
	}
	if err := loadEnvFiles(envs); err != nil {
		return err
	}

	agent, err := newAgent(newProvider, *model, *storeDir, ".")
	if err != nil {
		return err
	}
	session, err := agent.Session(ctx, *id)
	if err != nil {
		return err
	}

	wroteDelta := false
	response, err := session.Prompt(ctx, *prompt, glue.WithEvents(func(event glue.Event) {
		if event.Type == glue.EventTextDelta && event.Delta != "" {
			fmt.Fprint(stdout, event.Delta)
			wroteDelta = true
		}
	}))
	if err != nil {
		if wroteDelta {
			fmt.Fprintln(stdout)
		}
		return err
	}
	if wroteDelta {
		fmt.Fprintln(stdout)
		return nil
	}
	if response.Text != "" {
		fmt.Fprintln(stdout, response.Text)
	}
	return nil
}

func serveCommand(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory, serve serveFunc) error {
	flags := flag.NewFlagSet("glue serve", flag.ContinueOnError)
	flags.SetOutput(stderr)

	model := flags.String("model", defaultModel, "model id or gemini/<model>")
	storeDir := flags.String("store", ".glue/sessions", "session store directory")
	workDir := flags.String("work", ".", "working directory for AGENTS.md, skills, and roles")
	listenAddr := flags.String("listen", defaultListenAddr, "local listen address")
	tokenFlag := flags.String("token", "", "bearer token; defaults to GLUE_DAEMON_TOKEN or a generated token")
	metadataPath := flags.String("metadata", defaultMetadataPath(), "connection metadata JSON path; empty disables metadata file")
	permissionTimeout := flags.Duration("permission-timeout", 0, "permission decision timeout; 0 uses daemon default")
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")

	if err := flags.Parse(args); err != nil {
		return err
	}

	agentName := "default"
	if flags.NArg() > 0 {
		agentName = flags.Arg(0)
	}
	if agentName != "default" && agentName != "gemini" {
		return fmt.Errorf("unknown agent %q; only 'default' is available in this runner", agentName)
	}
	if err := loadEnvFiles(envs); err != nil {
		return err
	}
	token, tokenSource, err := resolveDaemonToken(*tokenFlag)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*metadataPath) == "" && tokenSource == "generated" {
		return errors.New("metadata disabled requires --token or GLUE_DAEMON_TOKEN")
	}
	agent, err := newAgent(newProvider, *model, *storeDir, *workDir)
	if err != nil {
		return err
	}
	handler, err := daemon.New(daemon.Options{
		Host:              agent,
		Token:             token,
		PermissionTimeout: *permissionTimeout,
	})
	if err != nil {
		return err
	}
	return serve(ctx, serveConfig{
		ListenAddr:        *listenAddr,
		Token:             token,
		TokenSource:       tokenSource,
		MetadataPath:      *metadataPath,
		Model:             normalizeModel(*model),
		StoreDir:          *storeDir,
		WorkDir:           *workDir,
		PermissionTimeout: *permissionTimeout,
		ShutdownTimeout:   defaultShutdownTimeout,
	}, handler, stdout)
}

func newAgent(newProvider providerFactory, model, storeDir, workDir string) (*glue.Agent, error) {
	provider, err := newProvider()
	if err != nil {
		return nil, err
	}
	return glue.NewAgent(glue.AgentOptions{
		Provider: provider,
		Model:    normalizeModel(model),
		Store:    filestore.New(storeDir),
		WorkDir:  workDir,
	}), nil
}

func normalizeModel(model string) string {
	return strings.TrimPrefix(model, "gemini/")
}

func serveDaemon(ctx context.Context, cfg serveConfig, handler http.Handler, stdout io.Writer) error {
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	baseURL := "http://" + ln.Addr().String()
	if cfg.MetadataPath != "" {
		if err := writeDaemonMetadata(cfg.MetadataPath, daemonMetadata{
			Version: 1,
			BaseURL: baseURL,
			Token:   cfg.Token,
			PID:     os.Getpid(),
		}); err != nil {
			_ = ln.Close()
			return err
		}
	}

	fmt.Fprintln(stdout, "glue daemon listening")
	fmt.Fprintf(stdout, "base_url: %s\n", baseURL)
	if cfg.MetadataPath != "" {
		fmt.Fprintf(stdout, "metadata: %s\n", cfg.MetadataPath)
		fmt.Fprintf(stdout, "token: written to metadata file (%s)\n", cfg.TokenSource)
	} else {
		fmt.Fprintf(stdout, "token: configured (%s); metadata file disabled\n", cfg.TokenSource)
	}

	server := &http.Server{Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		err := server.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return <-errCh
	}
}

func resolveDaemonToken(flagValue string) (token, source string, err error) {
	if token := strings.TrimSpace(flagValue); token != "" {
		return token, "flag", nil
	}
	if token := strings.TrimSpace(os.Getenv("GLUE_DAEMON_TOKEN")); token != "" {
		return token, "GLUE_DAEMON_TOKEN", nil
	}
	token, err = randomToken()
	if err != nil {
		return "", "", err
	}
	return token, "generated", nil
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func defaultMetadataPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return filepath.Join(".glue", "daemon.json")
	}
	return filepath.Join(dir, "glue", "daemon.json")
}

func writeDaemonMetadata(path string, meta daemonMetadata) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  glue run [default|gemini] --prompt <text> [--id <id>] [--model <model>] [--store <dir>] [--env <path>]
  glue serve [default|gemini] [--listen 127.0.0.1:0] [--metadata <path>] [--model <model>] [--store <dir>] [--env <path>]

Commands:
  run    Run the local Gemini-backed agent.
  serve  Start a local HTTP+SSE daemon for Glue sessions.

Flags:
  --id       Session id. Defaults to "default".
  --prompt   Prompt text. Required.
  --model    Gemini model id or gemini/<model>. Defaults to gemini-2.5-flash.
  --store    File session store directory. Defaults to .glue/sessions.
  --work     Working directory for serve mode. Defaults to ".".
  --listen   Serve listen address. Defaults to 127.0.0.1:0.
  --token    Serve bearer token. Defaults to GLUE_DAEMON_TOKEN or a generated token.
  --metadata Serve connection metadata JSON. Defaults to the user config directory.
  --env      Load env vars from a .env file. Repeatable; shell env wins.
`)
}

func loadEnvFiles(files []string) error {
	shellEnv := map[string]struct{}{}
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			shellEnv[key] = struct{}{}
		}
	}

	for _, path := range files {
		if err := loadEnvFile(path, shellEnv); err != nil {
			return err
		}
	}
	return nil
}

func loadEnvFile(path string, shellEnv map[string]struct{}) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s:%d: empty key", path, lineNumber)
		}
		if _, exists := shellEnv[key]; exists {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}
