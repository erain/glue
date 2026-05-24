package peggy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	toolsmcp "github.com/erain/glue/tools/mcp"
)

// Version is the package version string surfaced by `peggy --version`.
// Bumped by hand at release time.
const Version = "0.3.0"

const (
	defaultDaemonListenAddr      = "127.0.0.1:0"
	defaultDaemonShutdownTimeout = 5 * time.Second
)

type serveFunc func(context.Context, serveConfig, http.Handler, io.Writer) error

type serveConfig struct {
	ListenAddr        string
	Token             string
	TokenSource       string
	MetadataPath      string
	PermissionTimeout time.Duration
	ShutdownTimeout   time.Duration
}

// Run is the top-level CLI entry point. It parses args, loads the
// settings and identity files, constructs a Peggy, and dispatches a
// single prompt. Returns a process exit code.
//
// Tests can drive Run directly with a synthetic args slice and
// captured stdout/stderr writers. For programmatic use prefer
// constructing a Peggy via New.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return RunWithInput(ctx, args, os.Stdin, stdout, stderr)
}

// RunWithInput is like [Run] but lets tests and embedded callers provide the
// stdin reader used by the CLI permission prompt.
func RunWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithDeps(ctx, args, stdin, stdout, stderr, servePeggyDaemon)
}

func runWithDeps(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, serve serveFunc) int {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return runServe(ctx, args[1:], stdout, stderr, serve)
		case "mcp":
			return runMCP(ctx, args[1:], stdout, stderr)
		}
	}

	fs := flag.NewFlagSet("peggy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath           = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		soulPath             = fs.String("soul", "", "path to identity Markdown (overrides $PEGGY_SOUL / XDG / ~/.config/peggy/SOUL.md)")
		sessionID            = fs.String("session", "default", "session id (file-backed transcripts key off this)")
		showVersion          = fs.Bool("version", false, "print version and exit")
		enableCoding         = fs.Bool("coding", false, "enable local coding tools for this prompt")
		codingWorkDir        = fs.String("workdir", "", "workspace root for --coding (default current directory)")
		codingAllowOverwrite = fs.Bool("coding-allow-overwrite", false, "allow write_file to replace existing files after model and permission approval")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy — long-running personal-assistant agent built on glue.

Usage:
  peggy [flags] "<prompt text>"
  peggy mcp tools [flags]
  peggy serve [flags]

Examples:
  peggy "hello"
  peggy --session work "remind me about the migration plan"
  peggy --config /tmp/peggy.json "what do you know about my Aussie?"
  peggy --coding --workdir . "run the tests and fix the failure"
  peggy mcp tools --config ~/.config/peggy/settings.json
  peggy serve --config ~/.config/peggy/settings.json

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(stderr, `
Config resolution: --config > $PEGGY_CONFIG > $XDG_CONFIG_HOME/peggy/settings.json > ~/.config/peggy/settings.json.
Identity (SOUL.md) resolution: --soul > $PEGGY_SOUL > $XDG_CONFIG_HOME/peggy/SOUL.md > ~/.config/peggy/SOUL.md. Missing identity is non-fatal.
`)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Fprintln(stdout, Version)
		return 0
	}

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		fs.Usage()
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy: no settings.json found; using built-in defaults")
	}
	applyCodingFlags(&settings, *enableCoding, *codingWorkDir, *codingAllowOverwrite)

	soul, soulPathUsed, err := LoadSoul(*soulPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy: %v\n", err)
		return 1
	}
	if soul == "" {
		fmt.Fprintln(stderr, "peggy: no SOUL.md found; running without identity context")
	} else {
		fmt.Fprintf(stderr, "peggy: identity loaded from %s (%d bytes)\n", soulPathUsed, len(soul))
	}

	var permission glue.Permission
	if settings.Coding.Enabled || MCPEnabled(settings.MCP) {
		permission = NewTieredPermission(
			NewCLIPermission(CLIPermissionOptions{Stdin: stdin, Stderr: stderr}),
			PermissionTierForChannel(settings.Permissions, PermissionChannelCLI),
			PermissionChannelCLI,
		)
	}
	if settings.Coding.Enabled {
		workDir := settings.Coding.WorkDir
		if strings.TrimSpace(workDir) == "" {
			workDir = "."
		}
		fmt.Fprintf(stderr, "peggy: coding tools enabled for %s\n", workDir)
	}

	p, err := New(Options{
		Settings:   settings,
		Soul:       soul,
		Stderr:     stderr,
		Permission: permission,
	})
	if err != nil {
		fmt.Fprintf(stderr, "peggy: setup: %v\n", err)
		return 1
	}
	defer p.Close()

	if _, err := p.Prompt(ctx, *sessionID, prompt, stdout); err != nil {
		fmt.Fprintf(stderr, "\npeggy: prompt: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout) // trailing newline so shell prompts don't run on
	return 0
}

type mcpToolCatalogEntry struct {
	Name               string          `json:"name"`
	Description        string          `json:"description,omitempty"`
	Parameters         json.RawMessage `json:"parameters,omitempty"`
	RequiresPermission bool            `json:"requires_permission"`
	PermissionAction   string          `json:"permission_action,omitempty"`
	PermissionTarget   string          `json:"permission_target,omitempty"`
}

type mcpResourceCatalogEntry struct {
	Server      string         `json:"server"`
	URI         string         `json:"uri"`
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	MIMEType    string         `json:"mime_type,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Size        *int64         `json:"size,omitempty"`
}

func runMCP(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprintf(stderr, `peggy mcp — inspect Peggy's configured MCP surface.

Usage:
  peggy mcp resources [flags]
  peggy mcp tools [flags]

Commands:
  resources  List resources discovered from enabled MCP servers.
  tools    List tools discovered from enabled MCP servers.
`)
	}
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage()
		return 0
	case "resources":
		return runMCPResources(ctx, args[1:], stdout, stderr)
	case "tools":
		return runMCPTools(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "peggy mcp: unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func runMCPResources(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy mcp resources", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		jsonOutput = fs.Bool("json", false, "print machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy mcp resources — list resources from enabled MCP servers.

Usage:
  peggy mcp resources [flags]

Examples:
  peggy mcp resources --config ~/.config/peggy/settings.json
  peggy mcp resources --json

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "peggy mcp resources: positional args not supported")
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp resources: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy mcp resources: no settings.json found; using built-in defaults")
	}

	resources, manager, _, err := MCPResources(ctx, settings.MCP)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp resources: %v\n", err)
		return 1
	}
	defer func() {
		if err := manager.Close(); err != nil {
			fmt.Fprintf(stderr, "peggy mcp resources: close: %v\n", err)
		}
	}()

	catalog := buildMCPResourceCatalog(resources)
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(catalog); err != nil {
			fmt.Fprintf(stderr, "peggy mcp resources: encode catalog: %v\n", err)
			return 1
		}
		return 0
	}
	writeMCPResourceCatalog(stdout, catalog)
	return 0
}

func runMCPTools(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy mcp tools", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		jsonOutput = fs.Bool("json", false, "print machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy mcp tools — list tools from enabled MCP servers.

Usage:
  peggy mcp tools [flags]

Examples:
  peggy mcp tools --config ~/.config/peggy/settings.json
  peggy mcp tools --json

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "peggy mcp tools: positional args not supported")
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp tools: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy mcp tools: no settings.json found; using built-in defaults")
	}

	tools, manager, _, err := MCPTools(ctx, settings.MCP)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp tools: %v\n", err)
		return 1
	}
	defer func() {
		if err := manager.Close(); err != nil {
			fmt.Fprintf(stderr, "peggy mcp tools: close: %v\n", err)
		}
	}()

	catalog := buildMCPToolCatalog(tools)
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(catalog); err != nil {
			fmt.Fprintf(stderr, "peggy mcp tools: encode catalog: %v\n", err)
			return 1
		}
		return 0
	}
	writeMCPToolCatalog(stdout, catalog)
	return 0
}

func buildMCPToolCatalog(tools []glue.Tool) []mcpToolCatalogEntry {
	catalog := make([]mcpToolCatalogEntry, 0, len(tools))
	for _, tool := range tools {
		entry := mcpToolCatalogEntry{
			Name:               tool.Name,
			Description:        tool.Description,
			Parameters:         append(json.RawMessage(nil), tool.Parameters...),
			RequiresPermission: tool.RequiresPermission,
			PermissionAction:   tool.PermissionAction,
		}
		if tool.PermissionTarget != nil {
			entry.PermissionTarget = tool.PermissionTarget(glue.ToolCall{Name: tool.Name})
		}
		catalog = append(catalog, entry)
	}
	return catalog
}

func buildMCPResourceCatalog(resources []toolsmcp.Resource) []mcpResourceCatalogEntry {
	catalog := make([]mcpResourceCatalogEntry, 0, len(resources))
	for _, resource := range resources {
		catalog = append(catalog, mcpResourceCatalogEntry{
			Server:      resource.Server,
			URI:         resource.URI,
			Name:        resource.Name,
			Title:       resource.Title,
			Description: resource.Description,
			MIMEType:    resource.MIMEType,
			Annotations: cloneResourceAnnotations(resource.Annotations),
			Size:        cloneResourceSize(resource.Size),
		})
	}
	return catalog
}

func writeMCPToolCatalog(w io.Writer, catalog []mcpToolCatalogEntry) {
	if len(catalog) == 0 {
		fmt.Fprintln(w, "No MCP tools configured.")
		return
	}
	for i, entry := range catalog {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, entry.Name)
		if entry.Description != "" {
			fmt.Fprintf(w, "  description: %s\n", singleLine(entry.Description))
		}
		if entry.RequiresPermission || entry.PermissionAction != "" || entry.PermissionTarget != "" {
			fmt.Fprintf(w, "  permission: %s %s\n", entry.PermissionAction, entry.PermissionTarget)
		}
		if len(entry.Parameters) > 0 {
			fmt.Fprintf(w, "  parameters: %s\n", compactJSONLine(entry.Parameters))
		}
	}
}

func writeMCPResourceCatalog(w io.Writer, catalog []mcpResourceCatalogEntry) {
	if len(catalog) == 0 {
		fmt.Fprintln(w, "No MCP resources configured.")
		return
	}
	for i, entry := range catalog {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, entry.URI)
		fmt.Fprintf(w, "  server: %s\n", entry.Server)
		fmt.Fprintf(w, "  name: %s\n", entry.Name)
		if entry.Title != "" {
			fmt.Fprintf(w, "  title: %s\n", singleLine(entry.Title))
		}
		if entry.Description != "" {
			fmt.Fprintf(w, "  description: %s\n", singleLine(entry.Description))
		}
		if entry.MIMEType != "" {
			fmt.Fprintf(w, "  mime_type: %s\n", entry.MIMEType)
		}
		if entry.Size != nil {
			fmt.Fprintf(w, "  size: %d\n", *entry.Size)
		}
		if len(entry.Annotations) > 0 {
			raw, err := json.Marshal(entry.Annotations)
			if err == nil {
				fmt.Fprintf(w, "  annotations: %s\n", compactJSONLine(raw))
			}
		}
	}
}

func cloneResourceAnnotations(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneResourceSize(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func compactJSONLine(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return singleLine(string(raw))
	}
	return buf.String()
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer, serve serveFunc) int {
	fs := flag.NewFlagSet("peggy serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath           = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		soulPath             = fs.String("soul", "", "path to identity Markdown (overrides $PEGGY_SOUL / XDG / ~/.config/peggy/SOUL.md)")
		listenAddr           = fs.String("listen", defaultDaemonListenAddr, "local listen address")
		tokenFlag            = fs.String("token", "", "bearer token; defaults to GLUE_DAEMON_TOKEN or a generated token")
		metadataPath         = fs.String("metadata", daemon.DefaultMetadataPath(), "connection metadata JSON path; empty disables metadata file")
		permissionTimeout    = fs.Duration("permission-timeout", 0, "permission decision timeout; 0 uses daemon default")
		showVersion          = fs.Bool("version", false, "print version and exit")
		enableCoding         = fs.Bool("coding", false, "enable local coding tools for this daemon")
		codingWorkDir        = fs.String("workdir", "", "workspace root for --coding (default current directory)")
		codingAllowOverwrite = fs.Bool("coding-allow-overwrite", false, "allow write_file to replace existing files after model and permission approval")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy serve — run Peggy as a local HTTP+SSE daemon.

Usage:
  peggy serve [flags]

Then connect from another terminal:
  glue connect --prompt "hello"

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, Version)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "peggy serve: positional args not supported")
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy serve: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy serve: no settings.json found; using built-in defaults")
	}
	applyCodingFlags(&settings, *enableCoding, *codingWorkDir, *codingAllowOverwrite)

	soul, soulPathUsed, err := LoadSoul(*soulPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy serve: %v\n", err)
		return 1
	}
	if soul == "" {
		fmt.Fprintln(stderr, "peggy serve: no SOUL.md found; running without identity context")
	} else {
		fmt.Fprintf(stderr, "peggy serve: identity loaded from %s (%d bytes)\n", soulPathUsed, len(soul))
	}
	if settings.Coding.Enabled {
		workDir := settings.Coding.WorkDir
		if strings.TrimSpace(workDir) == "" {
			workDir = "."
		}
		fmt.Fprintf(stderr, "peggy serve: coding tools enabled for %s\n", workDir)
	}

	token, tokenSource, err := daemon.ResolveToken(*tokenFlag)
	if err != nil {
		fmt.Fprintf(stderr, "peggy serve: %v\n", err)
		return 1
	}
	if strings.TrimSpace(*metadataPath) == "" && tokenSource == "generated" {
		fmt.Fprintln(stderr, "peggy serve: metadata disabled requires --token or GLUE_DAEMON_TOKEN")
		return 1
	}

	p, err := New(Options{
		Settings: settings,
		Soul:     soul,
		Stderr:   stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "peggy serve: setup: %v\n", err)
		return 1
	}
	defer p.Close()

	handler, err := daemon.New(daemon.Options{
		Host:              p.Agent(),
		Token:             token,
		PermissionPolicy:  NewDaemonPermissionPolicy(settings.Permissions),
		PermissionTimeout: *permissionTimeout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "peggy serve: setup: %v\n", err)
		return 1
	}

	if err := serve(ctx, serveConfig{
		ListenAddr:        *listenAddr,
		Token:             token,
		TokenSource:       tokenSource,
		MetadataPath:      *metadataPath,
		PermissionTimeout: *permissionTimeout,
		ShutdownTimeout:   defaultDaemonShutdownTimeout,
	}, handler, stdout); err != nil {
		fmt.Fprintf(stderr, "peggy serve: %v\n", err)
		return 1
	}
	return 0
}

func applyCodingFlags(settings *Settings, enable bool, workDir string, allowOverwrite bool) {
	if settings == nil {
		return
	}
	if enable || workDir != "" || allowOverwrite {
		settings.Coding.Enabled = true
	}
	if workDir != "" {
		settings.Coding.WorkDir = workDir
	}
	if allowOverwrite {
		settings.Coding.AllowOverwrite = true
	}
}

func servePeggyDaemon(ctx context.Context, cfg serveConfig, handler http.Handler, stdout io.Writer) error {
	return daemon.ServeLocal(ctx, daemon.LocalConfig{
		Name:            "peggy daemon",
		ListenAddr:      cfg.ListenAddr,
		Token:           cfg.Token,
		TokenSource:     cfg.TokenSource,
		MetadataPath:    cfg.MetadataPath,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}, handler, stdout)
}
