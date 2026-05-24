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

type repeatedStrings []string

func (r *repeatedStrings) String() string { return strings.Join(*r, ",") }

func (r *repeatedStrings) Set(value string) error {
	*r = append(*r, value)
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
	Skill        string
	SkillArgs    map[string]string
	ClientID     string
	Model        string
	Role         string
	MaxTurns     int
}

type startRunPayload struct {
	Text      string            `json:"text,omitempty"`
	Skill     string            `json:"skill,omitempty"`
	Arguments map[string]string `json:"arguments,omitempty"`
	ClientID  string            `json:"client_id,omitempty"`
	Role      string            `json:"role,omitempty"`
	Model     string            `json:"model,omitempty"`
	MaxTurns  int               `json:"max_turns,omitempty"`
}

type startRunResult struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	EventsURL string `json:"events_url"`
}

type connectRunDonePayload struct {
	Text        string         `json:"text"`
	Message     *glue.Message  `json:"message,omitempty"`
	NewMessages []glue.Message `json:"new_messages,omitempty"`
}

type usageSummary struct {
	HasUsage         bool
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
}

type daemonToolCatalog struct {
	Tools []daemonToolCatalogEntry `json:"tools"`
}

type daemonSkillCatalog struct {
	Skills []daemon.SkillCatalogEntry `json:"skills"`
}

type daemonRoleCatalog struct {
	Roles []daemon.RoleCatalogEntry `json:"roles"`
}

type daemonMCPResourceCatalog struct {
	Resources []daemon.MCPResourceCatalogEntry `json:"resources"`
}

type daemonMCPPromptCatalog struct {
	Prompts []daemon.MCPPromptCatalogEntry `json:"prompts"`
}

type daemonToolCatalogEntry struct {
	Name                    string          `json:"name"`
	Description             string          `json:"description,omitempty"`
	Parameters              json.RawMessage `json:"parameters,omitempty"`
	RequiresPermission      bool            `json:"requires_permission"`
	PermissionAction        string          `json:"permission_action,omitempty"`
	PermissionTargetPreview string          `json:"permission_target_preview,omitempty"`
}

type daemonStatus struct {
	OK           bool     `json:"ok"`
	Version      int      `json:"version"`
	ActiveRuns   int      `json:"active_runs"`
	ToolsCount   int      `json:"tools_count"`
	Capabilities []string `json:"capabilities"`
}

type daemonInspect struct {
	Status       daemonStatus                     `json:"status"`
	Tools        []daemonToolCatalogEntry         `json:"tools"`
	Skills       []daemon.SkillCatalogEntry       `json:"skills,omitempty"`
	Roles        []daemon.RoleCatalogEntry        `json:"roles,omitempty"`
	MCPResources []daemon.MCPResourceCatalogEntry `json:"mcp_resources,omitempty"`
	MCPPrompts   []daemon.MCPPromptCatalogEntry   `json:"mcp_prompts,omitempty"`
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
	showUsage := flags.Bool("usage", false, "print token usage summary to stderr when available")
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
	} else if response.Text != "" {
		fmt.Fprintln(stdout, response.Text)
	}
	if *showUsage {
		writeUsageSummary(stderr, summarizeUsage(response.NewMessages))
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
	skillName := flags.String("skill", "", "daemon skill name to run instead of --prompt")
	baseURL := flags.String("base-url", "", "daemon base URL; defaults to metadata file")
	tokenFlag := flags.String("token", "", "daemon bearer token; defaults to metadata file or GLUE_DAEMON_TOKEN")
	metadataPath := flags.String("metadata", defaultMetadataPath(), "connection metadata JSON path; empty disables metadata file")
	clientID := flags.String("client-id", defaultClientID(), "daemon client id")
	model := flags.String("model", "", "per-run model override")
	role := flags.String("role", "", "per-run role override")
	maxTurns := flags.Int("max-turns", 0, "per-run loop turn budget")
	showUsage := flags.Bool("usage", false, "print token usage summary to stderr when available")
	showTools := flags.Bool("tools", false, "list daemon tools and exit without starting a run")
	toolsJSON := flags.Bool("tools-json", false, "print --tools output as JSON")
	showSkills := flags.Bool("skills", false, "list daemon skills and exit without starting a run")
	skillsJSON := flags.Bool("skills-json", false, "print --skills output as JSON")
	showRoles := flags.Bool("roles", false, "list daemon roles and exit without starting a run")
	rolesJSON := flags.Bool("roles-json", false, "print --roles output as JSON")
	showMCPResources := flags.Bool("mcp-resources", false, "list daemon MCP resources and exit without starting a run")
	mcpResourcesJSON := flags.Bool("mcp-resources-json", false, "print --mcp-resources output as JSON")
	showMCPPrompts := flags.Bool("mcp-prompts", false, "list daemon MCP prompts and exit without starting a run")
	mcpPromptsJSON := flags.Bool("mcp-prompts-json", false, "print --mcp-prompts output as JSON")
	showMCPRead := flags.Bool("mcp-read", false, "read one daemon MCP resource and exit without starting a run")
	mcpReadJSON := flags.Bool("mcp-read-json", false, "print --mcp-read output as JSON")
	showMCPPrompt := flags.Bool("mcp-prompt", false, "render one daemon MCP prompt and exit without starting a run")
	mcpPromptJSON := flags.Bool("mcp-prompt-json", false, "print --mcp-prompt output as JSON")
	mcpServer := flags.String("server", "", "MCP server name for --mcp-read or --mcp-prompt")
	mcpURI := flags.String("uri", "", "MCP resource URI for --mcp-read")
	mcpName := flags.String("name", "", "MCP prompt name for --mcp-prompt")
	showStatus := flags.Bool("status", false, "show daemon status and exit without starting a run")
	statusJSON := flags.Bool("status-json", false, "print --status output as JSON")
	showInspect := flags.Bool("inspect", false, "show daemon status and tools and exit without starting a run")
	inspectJSON := flags.Bool("inspect-json", false, "print --inspect output as JSON")
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")
	var runArgs repeatedStrings
	flags.Var(&runArgs, "arg", "skill or MCP prompt argument key=value; repeatable")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if *toolsJSON {
		*showTools = true
	}
	if *skillsJSON {
		*showSkills = true
	}
	if *rolesJSON {
		*showRoles = true
	}
	if *mcpResourcesJSON {
		*showMCPResources = true
	}
	if *mcpPromptsJSON {
		*showMCPPrompts = true
	}
	if *mcpReadJSON {
		*showMCPRead = true
	}
	if *mcpPromptJSON {
		*showMCPPrompt = true
	}
	if *statusJSON {
		*showStatus = true
	}
	if *inspectJSON {
		*showInspect = true
	}
	inspectModes := 0
	for _, enabled := range []bool{*showTools, *showSkills, *showRoles, *showMCPResources, *showMCPPrompts, *showMCPRead, *showMCPPrompt, *showStatus, *showInspect} {
		if enabled {
			inspectModes++
		}
	}
	if inspectModes > 1 {
		return errors.New("choose only one of --tools, --skills, --roles, --mcp-resources, --mcp-prompts, --mcp-read, --mcp-prompt, --status, or --inspect")
	}
	if inspectModes == 0 && strings.TrimSpace(*prompt) == "" && strings.TrimSpace(*skillName) == "" {
		return errors.New("missing required --prompt or --skill")
	}
	if inspectModes == 0 && strings.TrimSpace(*prompt) != "" && strings.TrimSpace(*skillName) != "" {
		return errors.New("choose only one of --prompt or --skill")
	}
	if err := loadEnvFiles(envs); err != nil {
		return err
	}
	runArgsMap, err := parseConnectArgs(runArgs)
	if err != nil {
		return err
	}
	cfg, err := resolveConnectConfig(connectConfig{
		BaseURL:      *baseURL,
		Token:        *tokenFlag,
		MetadataPath: *metadataPath,
		SessionID:    *sessionID,
		Prompt:       *prompt,
		Skill:        *skillName,
		SkillArgs:    runArgsMap,
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
	if *showSkills {
		return runConnectSkills(ctx, cfg, *skillsJSON, stdout, client)
	}
	if *showRoles {
		return runConnectRoles(ctx, cfg, *rolesJSON, stdout, client)
	}
	if *showMCPResources {
		return runConnectMCPResources(ctx, cfg, *mcpResourcesJSON, stdout, client)
	}
	if *showMCPPrompts {
		return runConnectMCPPrompts(ctx, cfg, *mcpPromptsJSON, stdout, client)
	}
	if *showMCPRead {
		return runConnectMCPRead(ctx, cfg, *mcpServer, *mcpURI, *mcpReadJSON, stdout, client)
	}
	if *showMCPPrompt {
		return runConnectMCPPrompt(ctx, cfg, *mcpServer, *mcpName, runArgsMap, *mcpPromptJSON, stdout, client)
	}
	if *showStatus {
		return runConnectStatus(ctx, cfg, *statusJSON, stdout, client)
	}
	if *showInspect {
		return runConnectInspect(ctx, cfg, *inspectJSON, stdout, client)
	}
	return runConnect(ctx, cfg, *showUsage, stdin, stdout, stderr, client)
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
	cfg.Prompt = strings.TrimSpace(cfg.Prompt)
	cfg.Skill = strings.TrimSpace(cfg.Skill)
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

func runConnect(ctx context.Context, cfg connectConfig, showUsage bool, stdin io.Reader, stdout io.Writer, stderr io.Writer, client httpDoer) error {
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
	return streamDaemonRun(ctx, cfg, start, showUsage, bufio.NewReader(stdin), stdout, stderr, client)
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

func runConnectSkills(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonSkills(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonSkillCatalog(stdout, catalog.Skills)
	return nil
}

func runConnectRoles(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonRoles(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonRoleCatalog(stdout, catalog.Roles)
	return nil
}

func runConnectMCPResources(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonMCPResources(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonMCPResourceCatalog(stdout, catalog.Resources)
	return nil
}

func runConnectMCPPrompts(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonMCPPrompts(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonMCPPromptCatalog(stdout, catalog.Prompts)
	return nil
}

func runConnectMCPRead(ctx context.Context, cfg connectConfig, server, uri string, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	server = strings.TrimSpace(server)
	uri = strings.TrimSpace(uri)
	if server == "" {
		return errors.New("--server is required for --mcp-read")
	}
	if uri == "" {
		return errors.New("--uri is required for --mcp-read")
	}
	read, err := requestDaemonMCPResourceRead(ctx, cfg, server, uri, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(read)
	}
	writeDaemonMCPResourceRead(stdout, read)
	return nil
}

func runConnectMCPPrompt(ctx context.Context, cfg connectConfig, server, name string, args map[string]string, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	server = strings.TrimSpace(server)
	name = strings.TrimSpace(name)
	if server == "" {
		return errors.New("--server is required for --mcp-prompt")
	}
	if name == "" {
		return errors.New("--name is required for --mcp-prompt")
	}
	rendered, err := requestDaemonMCPPrompt(ctx, cfg, server, name, args, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rendered)
	}
	writeDaemonMCPPrompt(stdout, rendered)
	return nil
}

func runConnectStatus(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	status, err := fetchDaemonStatus(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	writeDaemonStatus(stdout, status)
	return nil
}

func runConnectInspect(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	status, err := fetchDaemonStatus(ctx, cfg, client)
	if err != nil {
		return err
	}
	catalog, err := fetchDaemonTools(ctx, cfg, client)
	if err != nil {
		return err
	}
	inspect := daemonInspect{Status: status, Tools: catalog.Tools}
	if daemonHasCapability(status, "skills") {
		skills, err := fetchDaemonSkills(ctx, cfg, client)
		if err != nil {
			return err
		}
		inspect.Skills = skills.Skills
	}
	if daemonHasCapability(status, "roles") {
		roles, err := fetchDaemonRoles(ctx, cfg, client)
		if err != nil {
			return err
		}
		inspect.Roles = roles.Roles
	}
	if daemonHasCapability(status, "mcp_resources") {
		resources, err := fetchDaemonMCPResources(ctx, cfg, client)
		if err != nil {
			return err
		}
		inspect.MCPResources = resources.Resources
	}
	if daemonHasCapability(status, "mcp_prompts") {
		prompts, err := fetchDaemonMCPPrompts(ctx, cfg, client)
		if err != nil {
			return err
		}
		inspect.MCPPrompts = prompts.Prompts
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(inspect)
	}
	writeDaemonInspect(stdout, inspect)
	return nil
}

func fetchDaemonStatus(ctx context.Context, cfg connectConfig, client httpDoer) (daemonStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/status", nil)
	if err != nil {
		return daemonStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonStatus{}, fmt.Errorf("daemon status: %s", httpStatusError(resp))
	}
	var status daemonStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return daemonStatus{}, err
	}
	return status, nil
}

func writeDaemonStatus(w io.Writer, status daemonStatus) {
	state := "error"
	if status.OK {
		state = "ok"
	}
	fmt.Fprintf(w, "status: %s\n", state)
	fmt.Fprintf(w, "version: %d\n", status.Version)
	fmt.Fprintf(w, "active_runs: %d\n", status.ActiveRuns)
	fmt.Fprintf(w, "tools_count: %d\n", status.ToolsCount)
	if len(status.Capabilities) > 0 {
		fmt.Fprintf(w, "capabilities: %s\n", strings.Join(status.Capabilities, ", "))
	}
}

func writeDaemonInspect(w io.Writer, inspect daemonInspect) {
	writeDaemonStatus(w, inspect.Status)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "tools:")
	writeDaemonToolCatalogIndented(w, inspect.Tools, "  ")
	if daemonHasCapability(inspect.Status, "skills") {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "skills:")
		writeDaemonSkillCatalogIndented(w, inspect.Skills, "  ")
	}
	if daemonHasCapability(inspect.Status, "roles") {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "roles:")
		writeDaemonRoleCatalogIndented(w, inspect.Roles, "  ")
	}
	if daemonHasCapability(inspect.Status, "mcp_resources") {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "mcp_resources:")
		writeDaemonMCPResourceCatalogIndented(w, inspect.MCPResources, "  ")
	}
	if daemonHasCapability(inspect.Status, "mcp_prompts") {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "mcp_prompts:")
		writeDaemonMCPPromptCatalogIndented(w, inspect.MCPPrompts, "  ")
	}
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

func fetchDaemonSkills(ctx context.Context, cfg connectConfig, client httpDoer) (daemonSkillCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/skills", nil)
	if err != nil {
		return daemonSkillCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonSkillCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonSkillCatalog{}, fmt.Errorf("daemon skills: %s", httpStatusError(resp))
	}
	var catalog daemonSkillCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonSkillCatalog{}, err
	}
	if catalog.Skills == nil {
		catalog.Skills = []daemon.SkillCatalogEntry{}
	}
	return catalog, nil
}

func fetchDaemonRoles(ctx context.Context, cfg connectConfig, client httpDoer) (daemonRoleCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/roles", nil)
	if err != nil {
		return daemonRoleCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonRoleCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonRoleCatalog{}, fmt.Errorf("daemon roles: %s", httpStatusError(resp))
	}
	var catalog daemonRoleCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonRoleCatalog{}, err
	}
	if catalog.Roles == nil {
		catalog.Roles = []daemon.RoleCatalogEntry{}
	}
	return catalog, nil
}

func fetchDaemonMCPResources(ctx context.Context, cfg connectConfig, client httpDoer) (daemonMCPResourceCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/mcp/resources", nil)
	if err != nil {
		return daemonMCPResourceCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonMCPResourceCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonMCPResourceCatalog{}, fmt.Errorf("daemon MCP resources: %s", httpStatusError(resp))
	}
	var catalog daemonMCPResourceCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonMCPResourceCatalog{}, err
	}
	if catalog.Resources == nil {
		catalog.Resources = []daemon.MCPResourceCatalogEntry{}
	}
	return catalog, nil
}

func fetchDaemonMCPPrompts(ctx context.Context, cfg connectConfig, client httpDoer) (daemonMCPPromptCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/mcp/prompts", nil)
	if err != nil {
		return daemonMCPPromptCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonMCPPromptCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonMCPPromptCatalog{}, fmt.Errorf("daemon MCP prompts: %s", httpStatusError(resp))
	}
	var catalog daemonMCPPromptCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonMCPPromptCatalog{}, err
	}
	if catalog.Prompts == nil {
		catalog.Prompts = []daemon.MCPPromptCatalogEntry{}
	}
	return catalog, nil
}

func requestDaemonMCPResourceRead(ctx context.Context, cfg connectConfig, server, uri string, client httpDoer) (daemon.MCPResourceReadResponse, error) {
	payload := daemon.MCPReadResourceRequest{Server: server, URI: uri}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return daemon.MCPResourceReadResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/v1/mcp/resources/read", &body)
	if err != nil {
		return daemon.MCPResourceReadResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return daemon.MCPResourceReadResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.MCPResourceReadResponse{}, fmt.Errorf("daemon MCP resource read: %s", httpStatusError(resp))
	}
	var read daemon.MCPResourceReadResponse
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		return daemon.MCPResourceReadResponse{}, err
	}
	if read.Contents == nil {
		read.Contents = []daemon.MCPResourceContent{}
	}
	return read, nil
}

func requestDaemonMCPPrompt(ctx context.Context, cfg connectConfig, server, name string, args map[string]string, client httpDoer) (daemon.MCPPromptRenderResponse, error) {
	payload := daemon.MCPPromptRenderRequest{Server: server, Name: name, Arguments: args}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return daemon.MCPPromptRenderResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/v1/mcp/prompts/get", &body)
	if err != nil {
		return daemon.MCPPromptRenderResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return daemon.MCPPromptRenderResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.MCPPromptRenderResponse{}, fmt.Errorf("daemon MCP prompt: %s", httpStatusError(resp))
	}
	var rendered daemon.MCPPromptRenderResponse
	if err := json.NewDecoder(resp.Body).Decode(&rendered); err != nil {
		return daemon.MCPPromptRenderResponse{}, err
	}
	if rendered.Messages == nil {
		rendered.Messages = []daemon.MCPPromptMessage{}
	}
	return rendered, nil
}

func writeDaemonToolCatalog(w io.Writer, tools []daemonToolCatalogEntry) {
	writeDaemonToolCatalogIndented(w, tools, "")
}

func writeDaemonToolCatalogIndented(w io.Writer, tools []daemonToolCatalogEntry, indent string) {
	if len(tools) == 0 {
		fmt.Fprintf(w, "%sNo daemon tools reported.\n", indent)
		return
	}
	for i, tool := range tools {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%s\n", indent, tool.Name)
		if tool.Description != "" {
			fmt.Fprintf(w, "%s  description: %s\n", indent, oneLine(tool.Description))
		}
		if tool.RequiresPermission || tool.PermissionAction != "" || tool.PermissionTargetPreview != "" {
			fmt.Fprintf(w, "%s  permission: %s %s\n", indent, tool.PermissionAction, tool.PermissionTargetPreview)
		}
		if len(tool.Parameters) > 0 {
			fmt.Fprintf(w, "%s  parameters: %s\n", indent, compactJSON(tool.Parameters))
		}
	}
}

func writeDaemonSkillCatalog(w io.Writer, skills []daemon.SkillCatalogEntry) {
	writeDaemonSkillCatalogIndented(w, skills, "")
}

func writeDaemonSkillCatalogIndented(w io.Writer, skills []daemon.SkillCatalogEntry, indent string) {
	if len(skills) == 0 {
		fmt.Fprintf(w, "%sNo daemon skills reported.\n", indent)
		return
	}
	for i, skill := range skills {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%s\n", indent, skill.Name)
		if skill.Description != "" {
			fmt.Fprintf(w, "%s  description: %s\n", indent, oneLine(skill.Description))
		}
	}
}

func writeDaemonRoleCatalog(w io.Writer, roles []daemon.RoleCatalogEntry) {
	writeDaemonRoleCatalogIndented(w, roles, "")
}

func writeDaemonRoleCatalogIndented(w io.Writer, roles []daemon.RoleCatalogEntry, indent string) {
	if len(roles) == 0 {
		fmt.Fprintf(w, "%sNo daemon roles reported.\n", indent)
		return
	}
	for i, role := range roles {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%s\n", indent, role.Name)
		if role.Description != "" {
			fmt.Fprintf(w, "%s  description: %s\n", indent, oneLine(role.Description))
		}
		if role.Model != "" {
			fmt.Fprintf(w, "%s  model: %s\n", indent, role.Model)
		}
	}
}

func writeDaemonMCPResourceCatalog(w io.Writer, resources []daemon.MCPResourceCatalogEntry) {
	writeDaemonMCPResourceCatalogIndented(w, resources, "")
}

func writeDaemonMCPResourceCatalogIndented(w io.Writer, resources []daemon.MCPResourceCatalogEntry, indent string) {
	if len(resources) == 0 {
		fmt.Fprintf(w, "%sNo daemon MCP resources reported.\n", indent)
		return
	}
	for i, resource := range resources {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%s\n", indent, resource.URI)
		fmt.Fprintf(w, "%s  server: %s\n", indent, resource.Server)
		fmt.Fprintf(w, "%s  name: %s\n", indent, resource.Name)
		if resource.Title != "" {
			fmt.Fprintf(w, "%s  title: %s\n", indent, oneLine(resource.Title))
		}
		if resource.Description != "" {
			fmt.Fprintf(w, "%s  description: %s\n", indent, oneLine(resource.Description))
		}
		if resource.MIMEType != "" {
			fmt.Fprintf(w, "%s  mime_type: %s\n", indent, resource.MIMEType)
		}
		if resource.Size != nil {
			fmt.Fprintf(w, "%s  size: %d\n", indent, *resource.Size)
		}
		if len(resource.Annotations) > 0 {
			raw, err := json.Marshal(resource.Annotations)
			if err == nil {
				fmt.Fprintf(w, "%s  annotations: %s\n", indent, compactJSON(raw))
			}
		}
	}
}

func writeDaemonMCPPromptCatalog(w io.Writer, prompts []daemon.MCPPromptCatalogEntry) {
	writeDaemonMCPPromptCatalogIndented(w, prompts, "")
}

func writeDaemonMCPPromptCatalogIndented(w io.Writer, prompts []daemon.MCPPromptCatalogEntry, indent string) {
	if len(prompts) == 0 {
		fmt.Fprintf(w, "%sNo daemon MCP prompts reported.\n", indent)
		return
	}
	for i, prompt := range prompts {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%s\n", indent, prompt.Name)
		fmt.Fprintf(w, "%s  server: %s\n", indent, prompt.Server)
		if prompt.Title != "" {
			fmt.Fprintf(w, "%s  title: %s\n", indent, oneLine(prompt.Title))
		}
		if prompt.Description != "" {
			fmt.Fprintf(w, "%s  description: %s\n", indent, oneLine(prompt.Description))
		}
		if len(prompt.Arguments) > 0 {
			fmt.Fprintf(w, "%s  arguments:\n", indent)
			for _, arg := range prompt.Arguments {
				required := ""
				if arg.Required {
					required = " required"
				}
				line := arg.Name + required
				if arg.Description != "" {
					line += " - " + oneLine(arg.Description)
				}
				fmt.Fprintf(w, "%s    %s\n", indent, line)
			}
		}
	}
}

func writeDaemonMCPResourceRead(w io.Writer, read daemon.MCPResourceReadResponse) {
	if len(read.Contents) == 0 {
		fmt.Fprintln(w, "No daemon MCP resource contents returned.")
		return
	}
	for i, item := range read.Contents {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, item.URI)
		fmt.Fprintf(w, "  server: %s\n", read.Server)
		fmt.Fprintf(w, "  requested_uri: %s\n", read.URI)
		if item.MIMEType != "" {
			fmt.Fprintf(w, "  mime_type: %s\n", item.MIMEType)
		}
		if len(item.Meta) > 0 {
			raw, err := json.Marshal(item.Meta)
			if err == nil {
				fmt.Fprintf(w, "  meta: %s\n", compactJSON(raw))
			}
		}
		switch {
		case item.Text != nil:
			fmt.Fprintln(w, "  text:")
			for _, line := range strings.Split(*item.Text, "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		case item.Blob != nil:
			fmt.Fprintf(w, "  blob: %s\n", *item.Blob)
		}
	}
}

func writeDaemonMCPPrompt(w io.Writer, rendered daemon.MCPPromptRenderResponse) {
	fmt.Fprintln(w, rendered.Name)
	fmt.Fprintf(w, "  server: %s\n", rendered.Server)
	if rendered.Description != "" {
		fmt.Fprintf(w, "  description: %s\n", oneLine(rendered.Description))
	}
	if len(rendered.Messages) == 0 {
		fmt.Fprintln(w, "  messages: []")
		return
	}
	fmt.Fprintln(w, "  messages:")
	for _, message := range rendered.Messages {
		fmt.Fprintf(w, "    - role: %s\n", message.Role)
		if text, ok := daemonPromptTextContent(message.Content); ok {
			fmt.Fprintln(w, "      text:")
			for _, line := range strings.Split(text, "\n") {
				fmt.Fprintf(w, "        %s\n", line)
			}
			continue
		}
		fmt.Fprintf(w, "      content: %s\n", compactJSON(message.Content))
	}
}

func daemonPromptTextContent(raw json.RawMessage) (string, bool) {
	var content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		return "", false
	}
	if content.Type != "text" {
		return "", false
	}
	return content.Text, true
}

func daemonHasCapability(status daemonStatus, capability string) bool {
	for _, existing := range status.Capabilities {
		if existing == capability {
			return true
		}
	}
	return false
}

func parseConnectArgs(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	args := make(map[string]string, len(values))
	for _, raw := range values {
		key, value, ok := strings.Cut(raw, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("--arg must be key=value")
		}
		args[key] = value
	}
	return args, nil
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
		Text:      cfg.Prompt,
		Skill:     cfg.Skill,
		Arguments: cfg.SkillArgs,
		ClientID:  cfg.ClientID,
		Role:      strings.TrimSpace(cfg.Role),
		Model:     strings.TrimSpace(cfg.Model),
		MaxTurns:  cfg.MaxTurns,
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

func streamDaemonRun(ctx context.Context, cfg connectConfig, start startRunResult, showUsage bool, input *bufio.Reader, stdout io.Writer, stderr io.Writer, client httpDoer) error {
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
			} else {
				done, err := decodePayload[connectRunDonePayload](event.Payload)
				if err != nil {
					return err
				}
				if done.Text != "" {
					fmt.Fprintln(stdout, done.Text)
				}
			}
			if showUsage {
				done, err := decodePayload[connectRunDonePayload](event.Payload)
				if err != nil {
					return err
				}
				messages := done.NewMessages
				if len(messages) == 0 && done.Message != nil {
					messages = []glue.Message{*done.Message}
				}
				writeUsageSummary(stderr, summarizeUsage(messages))
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

func summarizeUsage(messages []glue.Message) usageSummary {
	var summary usageSummary
	for _, message := range messages {
		if message.Role != glue.MessageRoleAssistant || message.Usage == nil {
			continue
		}
		usage := message.Usage
		summary.HasUsage = true
		summary.InputTokens += usage.InputTokens
		summary.OutputTokens += usage.OutputTokens
		summary.CacheReadTokens += usage.CacheReadTokens
		summary.CacheWriteTokens += usage.CacheWriteTokens
		total := usage.TotalTokens
		if total == 0 {
			total = usage.InputTokens + usage.OutputTokens
		}
		summary.TotalTokens += total
	}
	return summary
}

func writeUsageSummary(w io.Writer, summary usageSummary) {
	if !summary.HasUsage {
		return
	}
	parts := []string{
		fmt.Sprintf("input=%d", summary.InputTokens),
		fmt.Sprintf("output=%d", summary.OutputTokens),
	}
	if summary.CacheReadTokens != 0 {
		parts = append(parts, fmt.Sprintf("cache_read=%d", summary.CacheReadTokens))
	}
	if summary.CacheWriteTokens != 0 {
		parts = append(parts, fmt.Sprintf("cache_write=%d", summary.CacheWriteTokens))
	}
	parts = append(parts, fmt.Sprintf("total=%d", summary.TotalTokens))
	fmt.Fprintf(w, "usage: %s\n", strings.Join(parts, " "))
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
  glue connect --skill <name> [--arg key=value] [--id <id>] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --inspect [--inspect-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --status [--status-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --tools [--tools-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --skills [--skills-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --roles [--roles-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --mcp-resources [--mcp-resources-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --mcp-prompts [--mcp-prompts-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --mcp-read --server <name> --uri <uri> [--mcp-read-json] [--metadata <path>] [--base-url <url>] [--token <token>]
  glue connect --mcp-prompt --server <name> --name <prompt> [--arg key=value] [--mcp-prompt-json] [--metadata <path>] [--base-url <url>] [--token <token>]

Commands:
  run      Run the local Gemini-backed agent.
  serve    Start a local HTTP+SSE daemon for Glue sessions.
  connect  Start a daemon prompt/skill run, or inspect daemon status/tools/skills/roles/MCP catalogs.

Flags:
  --id       Session id. Defaults to "default".
  --prompt   Prompt text. Required unless --skill is set or connect is in an inspection mode.
  --skill    Connect mode: run one daemon skill instead of --prompt.
  --model    Gemini model id or gemini/<model>. Defaults to gemini-2.5-flash.
  --store    File session store directory. Defaults to .glue/sessions.
  --work     Working directory for serve mode. Defaults to ".".
  --listen   Serve listen address. Defaults to 127.0.0.1:0.
  --token    Serve bearer token. Defaults to GLUE_DAEMON_TOKEN or a generated token.
  --metadata Serve connection metadata JSON. Defaults to the user config directory.
  --base-url Connect daemon base URL override.
  --role     Connect role override.
  --max-turns Connect loop turn budget override.
  --usage   Run/connect mode: print token usage summary to stderr when available.
  --inspect  Connect mode: show daemon status and tools without starting a run.
  --inspect-json
             Connect --inspect mode: print JSON.
  --status   Connect mode: show daemon status and exit without starting a run.
  --status-json
             Connect --status mode: print JSON.
  --tools    Connect mode: list daemon tools and exit without starting a run.
  --tools-json
             Connect --tools mode: print JSON.
  --skills   Connect mode: list daemon skills and exit without starting a run.
  --skills-json
             Connect --skills mode: print JSON.
  --roles    Connect mode: list daemon roles and exit without starting a run.
  --roles-json
             Connect --roles mode: print JSON.
  --mcp-resources
             Connect mode: list daemon MCP resources and exit without starting a run.
  --mcp-resources-json
             Connect --mcp-resources mode: print JSON.
  --mcp-prompts
             Connect mode: list daemon MCP prompts and exit without starting a run.
  --mcp-prompts-json
             Connect --mcp-prompts mode: print JSON.
  --mcp-read
             Connect mode: read one daemon MCP resource and exit without starting a run.
  --mcp-read-json
             Connect --mcp-read mode: print JSON.
  --mcp-prompt
             Connect mode: render one daemon MCP prompt and exit without starting a run.
  --mcp-prompt-json
             Connect --mcp-prompt mode: print JSON.
  --server   MCP server name for --mcp-read or --mcp-prompt.
  --uri      MCP resource URI for --mcp-read.
  --name     MCP prompt name for --mcp-prompt.
  --arg      Skill or MCP prompt argument key=value. Repeatable.
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
