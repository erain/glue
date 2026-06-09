// The "glue serve" subcommand: the local HTTP+SSE daemon, its bearer
// token resolution, and connection-metadata read/write.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/erain/glue/daemon"
	// Register the shipped providers so they resolve through the
	// providers registry by name (--provider). Importing for side
)

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

func serveCommand(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory, serve serveFunc) error {
	flags := flag.NewFlagSet("glue serve", flag.ContinueOnError)
	flags.SetOutput(stderr)

	provider := flags.String("provider", defaultProvider, "provider name: codex, gemini, nvidia, or openrouter")
	model := flags.String("model", "", "model id (default: the provider's default model); gemini/<model> accepted")
	storeDir := flags.String("store", ".glue/sessions", "session store directory")
	workDir := flags.String("work", ".", "working directory for AGENTS.md, skills, and roles")
	coding := flags.Bool("coding", false, "enable local coding tools for daemon runs")
	codingAllowOverwrite := flags.Bool("coding-allow-overwrite", false, "allow write_file to replace existing files after model and daemon permission approval")
	listenAddr := flags.String("listen", defaultListenAddr, "local listen address")
	tokenFlag := flags.String("token", "", "bearer token; defaults to GLUE_DAEMON_TOKEN or a generated token")
	metadataPath := flags.String("metadata", defaultMetadataPath(), "connection metadata JSON path; empty disables metadata file")
	permissionTimeout := flags.Duration("permission-timeout", 0, "permission decision timeout; 0 uses daemon default")
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")
	var allowedBinaries repeatedStrings
	flags.Var(&allowedBinaries, "allow-binary", "allowed shell_exec binary basename for --coding; repeatable")

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
	providerName, effectiveModel, err := resolveProvider(*provider, *model)
	if err != nil {
		return err
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
	tools, _, err := buildCodingTools(codingFlagConfig{
		Enabled:         *coding,
		WorkDir:         *workDir,
		AllowedBinaries: append([]string(nil), allowedBinaries...),
		AllowOverwrite:  *codingAllowOverwrite,
	})
	if err != nil {
		return err
	}
	if *coding {
		fmt.Fprintf(stderr, "glue serve: coding tools enabled for %s\n", *workDir)
	}
	agent, err := newAgent(newProvider, agentConfig{
		Provider: providerName,
		Model:    effectiveModel,
		StoreDir: *storeDir,
		WorkDir:  *workDir,
		Tools:    tools,
	})
	if err != nil {
		return err
	}
	handler, err := daemon.New(daemon.Options{
		Host:  agent,
		Token: token,
		Diagnostics: daemon.DiagnosticInfo{
			Name:         "glue",
			ListenAddr:   *listenAddr,
			MetadataPath: *metadataPath,
			TokenSource:  tokenSource,
			Provider:     providerName,
			Model:        normalizeModel(effectiveModel),
			StoreType:    "file",
			StorePath:    *storeDir,
		},
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
		Model:             normalizeModel(effectiveModel),
		StoreDir:          *storeDir,
		WorkDir:           *workDir,
		PermissionTimeout: *permissionTimeout,
		ShutdownTimeout:   defaultShutdownTimeout,
	}, handler, stdout)
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
