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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
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

type daemonMetadata = daemon.Metadata

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type connectConfig struct {
	BaseURL      string
	Token        string
	MetadataPath string
	SessionID    string
	Prompt       string
	ClientID     string
	Model        string
	Role         string
	MaxTurns     int
}

type startRunPayload struct {
	Text     string `json:"text"`
	ClientID string `json:"client_id,omitempty"`
	Role     string `json:"role,omitempty"`
	Model    string `json:"model,omitempty"`
	MaxTurns int    `json:"max_turns,omitempty"`
}

type startRunResult struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	EventsURL string `json:"events_url"`
}

type daemonToolCatalog struct {
	Tools []daemonToolCatalogEntry `json:"tools"`
}

type daemonToolCatalogEntry struct {
	Name                    string          `json:"name"`
	Description             string          `json:"description,omitempty"`
	Parameters              json.RawMessage `json:"parameters,omitempty"`
	RequiresPermission      bool            `json:"requires_permission"`
	PermissionAction        string          `json:"permission_action,omitempty"`
	PermissionTargetPreview string          `json:"permission_target_preview,omitempty"`
}

type connectPermissionPayload struct {
	PermissionID string                 `json:"permission_id"`
	Request      glue.PermissionRequest `json:"request"`
	ExpiresAt    time.Time              `json:"expires_at"`
}

type connectPermissionDecision struct {
	Allow       bool   `json:"allow"`
	Reason      string `json:"reason,omitempty"`
	RememberFor string `json:"remember_for,omitempty"`
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
	return runCLIWithDeps(ctx, args, os.Stdin, stdout, stderr, newProvider, serveDaemon, http.DefaultClient)
}

func runCLIWithServe(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory, serve serveFunc) int {
	return runCLIWithDeps(ctx, args, os.Stdin, stdout, stderr, newProvider, serve, http.DefaultClient)
}

func runCLIWithDeps(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, newProvider providerFactory, serve serveFunc, client httpDoer) int {
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
	case "connect":
		if err := connectCommand(ctx, args[1:], stdin, stdout, stderr, client); err != nil {
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

func connectCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, client httpDoer) error {
	flags := flag.NewFlagSet("glue connect", flag.ContinueOnError)
	flags.SetOutput(stderr)

	sessionID := flags.String("id", "default", "session id")
	prompt := flags.String("prompt", "", "prompt text")
	baseURL := flags.String("base-url", "", "daemon base URL; defaults to metadata file")
	tokenFlag := flags.String("token", "", "daemon bearer token; defaults to metadata file or GLUE_DAEMON_TOKEN")
	metadataPath := flags.String("metadata", defaultMetadataPath(), "connection metadata JSON path; empty disables metadata file")
	clientID := flags.String("client-id", defaultClientID(), "daemon client id")
	model := flags.String("model", "", "per-run model override")
	role := flags.String("role", "", "per-run role override")
	maxTurns := flags.Int("max-turns", 0, "per-run loop turn budget")
	showTools := flags.Bool("tools", false, "list daemon tools and exit without starting a run")
	toolsJSON := flags.Bool("tools-json", false, "print --tools output as JSON")
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if *toolsJSON {
		*showTools = true
	}
	if !*showTools && strings.TrimSpace(*prompt) == "" {
		return errors.New("missing required --prompt")
	}
	if err := loadEnvFiles(envs); err != nil {
		return err
	}
	cfg, err := resolveConnectConfig(connectConfig{
		BaseURL:      *baseURL,
		Token:        *tokenFlag,
		MetadataPath: *metadataPath,
		SessionID:    *sessionID,
		Prompt:       *prompt,
		ClientID:     *clientID,
		Model:        *model,
		Role:         *role,
		MaxTurns:     *maxTurns,
	})
	if err != nil {
		return err
	}
	if *showTools {
		return runConnectTools(ctx, cfg, *toolsJSON, stdout, client)
	}
	return runConnect(ctx, cfg, stdin, stdout, stderr, client)
}

func resolveConnectConfig(cfg connectConfig) (connectConfig, error) {
	var meta daemonMetadata
	var metadataErr error
	if strings.TrimSpace(cfg.MetadataPath) != "" {
		loaded, err := readDaemonMetadata(cfg.MetadataPath)
		if err != nil {
			metadataErr = err
		} else {
			meta = loaded
		}
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = meta.BaseURL
	}
	if strings.TrimSpace(cfg.Token) == "" {
		cfg.Token = meta.Token
	}
	if strings.TrimSpace(cfg.Token) == "" {
		cfg.Token = strings.TrimSpace(os.Getenv("GLUE_DAEMON_TOKEN"))
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.Token = strings.TrimSpace(cfg.Token)
	cfg.SessionID = strings.TrimSpace(cfg.SessionID)
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	if cfg.SessionID == "" {
		cfg.SessionID = "default"
	}
	if cfg.ClientID == "" {
		cfg.ClientID = defaultClientID()
	}
	if cfg.BaseURL == "" {
		if metadataErr != nil {
			return cfg, metadataErr
		}
		return cfg, errors.New("daemon base URL is required (metadata missing or pass --base-url)")
	}
	if cfg.Token == "" {
		if metadataErr != nil {
			return cfg, metadataErr
		}
		return cfg, errors.New("daemon token is required (metadata missing, pass --token, or set GLUE_DAEMON_TOKEN)")
	}
	return cfg, nil
}

func runConnect(ctx context.Context, cfg connectConfig, stdin io.Reader, stdout io.Writer, stderr io.Writer, client httpDoer) error {
	start, err := startDaemonRun(ctx, cfg, client)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = cancelDaemonRun(context.Background(), cfg, start.RunID, client)
		case <-done:
		}
	}()
	return streamDaemonRun(ctx, cfg, start, bufio.NewReader(stdin), stdout, stderr, client)
}

func runConnectTools(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonTools(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonToolCatalog(stdout, catalog.Tools)
	return nil
}

func fetchDaemonTools(ctx context.Context, cfg connectConfig, client httpDoer) (daemonToolCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/tools", nil)
	if err != nil {
		return daemonToolCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonToolCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonToolCatalog{}, fmt.Errorf("daemon tools: %s", httpStatusError(resp))
	}
	var catalog daemonToolCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonToolCatalog{}, err
	}
	if catalog.Tools == nil {
		catalog.Tools = []daemonToolCatalogEntry{}
	}
	return catalog, nil
}

func writeDaemonToolCatalog(w io.Writer, tools []daemonToolCatalogEntry) {
	if len(tools) == 0 {
		fmt.Fprintln(w, "No daemon tools reported.")
		return
	}
	for i, tool := range tools {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, tool.Name)
		if tool.Description != "" {
			fmt.Fprintf(w, "  description: %s\n", oneLine(tool.Description))
		}
		if tool.RequiresPermission || tool.PermissionAction != "" || tool.PermissionTargetPreview != "" {
			fmt.Fprintf(w, "  permission: %s %s\n", tool.PermissionAction, tool.PermissionTargetPreview)
		}
		if len(tool.Parameters) > 0 {
			fmt.Fprintf(w, "  parameters: %s\n", compactJSON(tool.Parameters))
		}
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return oneLine(string(raw))
	}
	return buf.String()
}

func startDaemonRun(ctx context.Context, cfg connectConfig, client httpDoer) (startRunResult, error) {
	payload := startRunPayload{
		Text:     cfg.Prompt,
		ClientID: cfg.ClientID,
		Role:     strings.TrimSpace(cfg.Role),
		Model:    strings.TrimSpace(cfg.Model),
		MaxTurns: cfg.MaxTurns,
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return startRunResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/v1/sessions/"+url.PathEscape(cfg.SessionID)+"/runs", &body)
	if err != nil {
		return startRunResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return startRunResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return startRunResult{}, fmt.Errorf("daemon start run: %s", httpStatusError(resp))
	}
	var out startRunResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return startRunResult{}, err
	}
	if out.RunID == "" || out.EventsURL == "" {
		return startRunResult{}, errors.New("daemon start run: missing run id or events URL")
	}
	return out, nil
}

func streamDaemonRun(ctx context.Context, cfg connectConfig, start startRunResult, input *bufio.Reader, stdout io.Writer, stderr io.Writer, client httpDoer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+start.EventsURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon event stream: %s", httpStatusError(resp))
	}

	wroteDelta := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event daemon.EventEnvelope
		dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(line, "data: ")))
		dec.UseNumber()
		if err := dec.Decode(&event); err != nil {
			return err
		}
		switch event.Type {
		case string(glue.EventTextDelta):
			if delta := payloadString(event.Payload, "delta"); delta != "" {
				fmt.Fprint(stdout, delta)
				wroteDelta = true
			}
		case "permission_request":
			if wroteDelta {
				fmt.Fprintln(stdout)
				wroteDelta = false
			}
			perm, err := decodePayload[connectPermissionPayload](event.Payload)
			if err != nil {
				return err
			}
			decision, err := promptPermission(input, stderr, perm)
			if err != nil {
				return err
			}
			if err := postPermissionDecision(ctx, cfg, start.RunID, perm.PermissionID, decision, client); err != nil {
				return err
			}
		case "run_done":
			if wroteDelta {
				fmt.Fprintln(stdout)
				return nil
			}
			if text := payloadString(event.Payload, "text"); text != "" {
				fmt.Fprintln(stdout, text)
			}
			return nil
		case "run_error":
			if wroteDelta {
				fmt.Fprintln(stdout)
			}
			if msg := payloadErrorMessage(event.Payload); msg != "" {
				return errors.New(msg)
			}
			return errors.New("daemon run failed")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("daemon event stream closed before terminal event")
}

func promptPermission(input *bufio.Reader, stderr io.Writer, perm connectPermissionPayload) (connectPermissionDecision, error) {
	req := perm.Request
	fmt.Fprintf(stderr, "\nPermission requested: %s", req.Tool)
	if req.Action != "" {
		fmt.Fprintf(stderr, " %s", req.Action)
	}
	if req.Target != "" {
		fmt.Fprintf(stderr, "\nTarget: %s", req.Target)
	}
	fmt.Fprint(stderr, "\n[d]eny, [a]llow once, allow for [s]ession, session [t]arget, [f]orever: ")
	raw, err := input.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return connectPermissionDecision{}, err
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "a", "allow", "allow once", "y", "yes":
		return connectPermissionDecision{Allow: true}, nil
	case "s", "session":
		return connectPermissionDecision{Allow: true, RememberFor: "session"}, nil
	case "t", "target", "session_target", "session target":
		return connectPermissionDecision{Allow: true, RememberFor: "session_target"}, nil
	case "f", "forever":
		return connectPermissionDecision{Allow: true, RememberFor: "forever"}, nil
	default:
		return connectPermissionDecision{Allow: false, Reason: "permission denied by glue connect"}, nil
	}
}

func postPermissionDecision(ctx context.Context, cfg connectConfig, runID, permissionID string, decision connectPermissionDecision, client httpDoer) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(decision); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/v1/runs/"+url.PathEscape(runID)+"/permissions/"+url.PathEscape(permissionID)+"/decision", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Glue-Client-ID", cfg.ClientID)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon permission decision: %s", httpStatusError(resp))
	}
	return nil
}

func cancelDaemonRun(ctx context.Context, cfg connectConfig, runID string, client httpDoer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, cfg.BaseURL+"/v1/runs/"+url.PathEscape(runID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("daemon cancel run: %s", httpStatusError(resp))
	}
	return nil
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
	return daemon.ServeLocal(ctx, daemon.LocalConfig{
		Name:            "glue daemon",
		ListenAddr:      cfg.ListenAddr,
		Token:           cfg.Token,
		TokenSource:     cfg.TokenSource,
		MetadataPath:    cfg.MetadataPath,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}, handler, stdout)
}

func resolveDaemonToken(flagValue string) (token, source string, err error) {
	return daemon.ResolveToken(flagValue)
}

func defaultMetadataPath() string {
	return daemon.DefaultMetadataPath()
}

func defaultClientID() string {
	return fmt.Sprintf("cli:%d", os.Getpid())
}

func readDaemonMetadata(path string) (daemonMetadata, error) {
	return daemon.ReadMetadata(path)
}

func writeDaemonMetadata(path string, meta daemonMetadata) error {
	return daemon.WriteMetadata(path, meta)
}

func decodePayload[T any](payload any) (T, error) {
	var out T
	data, err := json.Marshal(payload)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(data, &out)
	return out, err
}

func payloadString(payload any, key string) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	value, _ := m[key].(string)
	return value
}

func payloadErrorMessage(payload any) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	errValue, ok := m["error"].(map[string]any)
	if !ok {
		return ""
	}
	msg, _ := errValue["message"].(string)
	return msg
}

func httpStatusError(resp *http.Response) string {
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Error.Message != "" {
		if out.Error.Code != "" {
			return fmt.Sprintf("%s: %s", out.Error.Code, out.Error.Message)
		}
		return out.Error.Message
	}
	return resp.Status
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  glue run [default|gemini] --prompt <text> [--id <id>] [--model <model>] [--store <dir>] [--env <path>]
  glue serve [default|gemini] [--listen 127.0.0.1:0] [--metadata <path>] [--model <model>] [--store <dir>] [--env <path>]
  glue connect --prompt <text> [--id <id>] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --tools [--tools-json] [--metadata <path>] [--base-url <url>] [--token <token>]

Commands:
  run      Run the local Gemini-backed agent.
  serve    Start a local HTTP+SSE daemon for Glue sessions.
  connect  Start a daemon run, or inspect daemon tools with --tools.

Flags:
  --id       Session id. Defaults to "default".
  --prompt   Prompt text. Required unless connect --tools is set.
  --model    Gemini model id or gemini/<model>. Defaults to gemini-2.5-flash.
  --store    File session store directory. Defaults to .glue/sessions.
  --work     Working directory for serve mode. Defaults to ".".
  --listen   Serve listen address. Defaults to 127.0.0.1:0.
  --token    Serve bearer token. Defaults to GLUE_DAEMON_TOKEN or a generated token.
  --metadata Serve connection metadata JSON. Defaults to the user config directory.
  --base-url Connect daemon base URL override.
  --role     Connect role override.
  --max-turns Connect loop turn budget override.
  --tools    Connect mode: list daemon tools and exit without starting a run.
  --tools-json
             Connect --tools mode: print JSON.
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
