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
	"path/filepath"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	"github.com/erain/glue/jobs"
	"github.com/erain/glue/providers"
	toolscoding "github.com/erain/glue/tools/coding"
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

func serveCommand(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory, serve serveFunc) (resultErr error) {
	flags := flag.NewFlagSet("glue serve", flag.ContinueOnError)
	flags.SetOutput(stderr)

	provider := flags.String("provider", "", "provider name: codex, deepseek, gemini, nvidia, or openrouter")
	thinking := flags.String("thinking", "", "thinking level for the selected model (see glue models)")
	model := flags.String("model", "", "model id (default: selected activity or explicit provider default); gemini/<model> accepted")
	retainRuns := flags.Duration("retain-runs", 720*time.Hour, "how long to retain terminal daemon runs")
	stateDir := flags.String("state-dir", ".glue/state", "durable daemon state directory")
	storeDir := flags.String("store", ".glue/sessions", "session store directory")
	workDir := flags.String("work", ".", "working directory for AGENTS.md, skills, and roles")
	coding := flags.Bool("coding", false, "enable local coding tools for daemon runs")
	jobsEnabled := flags.Bool("jobs", false, "host durable coding jobs (default on with --coding)")
	jobFromAX := flags.Bool("job-from-ax", false, "create a job from AX task metadata or AX_TASK_YAML on startup (idempotent)")
	policyName := flags.String("policy", "auto", "permission policy profile: auto, workspace-auto, ask-all, or yolo")
	policyFile := flags.String("policy-file", "", "path to custom JSON permission policy file")
	codingAllowOverwrite := flags.Bool("coding-allow-overwrite", false, "allow write_file to replace existing files after model and daemon permission approval")
	listenAddr := flags.String("listen", defaultListenAddr, "local listen address")
	tokenFlag := flags.String("token", "", "bearer token; defaults to GLUE_DAEMON_TOKEN or a generated token")
	tokenFile := flags.String("token-file", "", "durable bearer token file path (created with mode 0600 if missing)")
	metadataPath := flags.String("metadata", defaultMetadataPath(), "connection metadata JSON path; empty disables metadata file")
	permissionTimeout := flags.Duration("permission-timeout", 0, "permission decision timeout; 0 uses daemon default")
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")
	var codingCfg codingFlagConfig
	registerShellFlags(flags, &codingCfg)

	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := codingCfg.validate(); err != nil {
		return err
	}

	jobsSet, policySet := false, false
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "jobs":
			jobsSet = true
		case "policy":
			policySet = true
		}
	})
	if !jobsSet {
		*jobsEnabled = *coding || *jobFromAX
	}
	if *jobFromAX && !*jobsEnabled {
		return errors.New("--job-from-ax requires --jobs")
	}
	if policySet && strings.TrimSpace(*policyFile) != "" {
		return errors.New("cannot combine --policy and --policy-file")
	}
	var policy jobs.Policy
	var err error
	if strings.TrimSpace(*policyFile) != "" {
		policy, err = jobs.LoadPolicyFile(*policyFile)
	} else {
		policy, err = jobs.BuiltInPolicy(*policyName, codingCfg.AllowedBinaries...)
	}
	if err != nil {
		return err
	}
	if policy.Name == "yolo" {
		fmt.Fprintln(stderr, "WARNING: glue serve: --policy yolo enabled; permission prompts are off.")
	}

	agentName := "default"
	if flags.NArg() > 0 {
		agentName = flags.Arg(0)
	}
	if agentName != "default" && agentName != "gemini" {
		return fmt.Errorf("unknown agent %q; only 'default' is available in this runner", agentName)
	}
	providerName, effectiveModel, err := resolveSelection(*coding, *provider, *model)
	if err != nil {
		return err
	}
	providerOptions, err := providers.ThinkingOptions(providerName, normalizeModel(effectiveModel), *thinking)
	if err != nil {
		return err
	}
	if err := loadEnvFiles(envs); err != nil {
		return err
	}
	if *jobFromAX && strings.TrimSpace(*tokenFile) == "" {
		if p := filepath.Join(filepath.Dir(*stateDir), "token"); func() bool { _, err := os.Stat(p); return err == nil }() {
			*tokenFile = p
		}
	}
	token, tokenSource, err := resolveServeToken(*tokenFlag, *tokenFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*metadataPath) == "" && tokenSource == "generated" {
		return errors.New("metadata disabled requires --token or GLUE_DAEMON_TOKEN")
	}
	codingCfg.Enabled, codingCfg.WorkDir, codingCfg.StateDir, codingCfg.AllowOverwrite = *coding, *workDir, *stateDir, *codingAllowOverwrite
	daemonCodingCfg := codingCfg
	if len(daemonCodingCfg.AllowedBinaries) == 0 && policy.Default == jobs.DecisionAllow && len(policy.Rules) == 0 {
		daemonCodingCfg.AllowedBinaries = repeatedStrings{"*"}
	}
	tools, _, err := buildCodingTools(daemonCodingCfg)
	if err != nil {
		return err
	}
	if *coding {
		fmt.Fprintf(stderr, "glue serve: coding tools enabled for %s\n", *workDir)
	}
	hostFactory := newProvider
	if *jobsEnabled {
		hostFactory = func(name string) (glue.Provider, error) { return &lazyProvider{name: name, factory: newProvider}, nil }
	}
	agent, err := newAgent(hostFactory, agentConfig{
		Provider: providerName,
		Options:  providerOptions,
		Model:    effectiveModel,
		StoreDir: *storeDir,
		WorkDir:  *workDir,
		Tools:    tools,
		Coding:   *coding,
	})
	if err != nil {
		return err
	}
	var manager *jobs.Manager
	if *jobsEnabled {
		manager, err = jobs.New(jobs.Options{
			StateDir: *stateDir, ProviderFactory: newProvider, Policy: &policy,
			AllowedBinaries: codingCfg.AllowedBinaries, ShellEnvPass: toolscoding.ParseCSVList(codingCfg.ShellEnvPass),
			ShellEnv: codingCfg.ShellEnv, ShellEnvSecretAllow: toolscoding.ParseCSVList(codingCfg.SecretAllow), ShellTimeout: codingCfg.ShellTimeout,
		})
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, manager.Close()) }()
		if *jobFromAX {
			if _, err := manager.EnsureJobFromAX(ctx, nil); err != nil {
				return err
			}
		}
	}
	handler, err := daemon.New(daemon.Options{
		Jobs:             manager,
		PermissionPolicy: daemon.NewPermissionPolicy(policy, *workDir),
		RetainRuns:       *retainRuns,
		StateDir:         *stateDir,
		Host:             agent,
		Token:            token,
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
	defer func() { resultErr = errors.Join(resultErr, handler.Close()) }()
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

func resolveServeToken(flagValue, tokenFile string) (token, source string, err error) {
	path := strings.TrimSpace(tokenFile)
	if path != "" && strings.TrimSpace(flagValue) == "" && strings.TrimSpace(os.Getenv("GLUE_DAEMON_TOKEN")) == "" {
		if data, err := os.ReadFile(path); err == nil {
			if tok := strings.TrimSpace(string(data)); tok != "" {
				_ = os.Chmod(path, 0o600)
				return tok, "token-file", nil
			}
		} else if !os.IsNotExist(err) {
			return "", "", err
		}
	}
	token, source, err = resolveDaemonToken(flagValue)
	if err != nil || path == "" {
		return token, source, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", "", err
	}
	if source == "generated" {
		source = "token-file"
	}
	return token, source, nil
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
