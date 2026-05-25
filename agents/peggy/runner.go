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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	toolsmcp "github.com/erain/glue/tools/mcp"
)

// Version is the package version string surfaced by `peggy --version`.
// Bumped by hand at release time.
const Version = "0.5.0"

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

type statusReport struct {
	Version     string             `json:"version"`
	Settings    statusFile         `json:"settings"`
	Identity    statusFile         `json:"identity"`
	Provider    statusProvider     `json:"provider"`
	Store       StoreSettings      `json:"store"`
	Compaction  CompactionSettings `json:"compaction"`
	Context     statusContext      `json:"context"`
	Coding      statusCoding       `json:"coding"`
	Permissions PermissionSettings `json:"permissions"`
	Channels    []string           `json:"channels,omitempty"`
	MCP         statusMCP          `json:"mcp"`
}

type statusFile struct {
	Path  string `json:"path,omitempty"`
	Found bool   `json:"found"`
	Bytes int    `json:"bytes,omitempty"`
}

type statusProvider struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

type statusCoding struct {
	Enabled         bool     `json:"enabled"`
	WorkDir         string   `json:"work_dir,omitempty"`
	AllowedBinaries []string `json:"allowed_binaries,omitempty"`
	AllowOverwrite  bool     `json:"allow_overwrite"`
}

type statusContext struct {
	Enabled bool   `json:"enabled"`
	WorkDir string `json:"work_dir,omitempty"`
}

type statusMCP struct {
	Configured int               `json:"configured"`
	Enabled    int               `json:"enabled"`
	Servers    []statusMCPServer `json:"servers,omitempty"`
}

type statusMCPServer struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Transport string `json:"transport"`
	Command   string `json:"command,omitempty"`
	URL       string `json:"url,omitempty"`
}

type skillCatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type roleCatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
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
		case "init":
			return runInit(args[1:], stdout, stderr)
		case "skill":
			return runSkill(ctx, args[1:], stdin, stdout, stderr)
		case "skills":
			return runSkills(args[1:], stdout, stderr)
		case "roles":
			return runRoles(args[1:], stdout, stderr)
		case "memories":
			return runMemories(ctx, args[1:], stdout, stderr)
		case "recall":
			return runRecall(ctx, args[1:], stdout, stderr)
		case "status":
			return runStatus(args[1:], stdout, stderr)
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
		roleName             = fs.String("role", "", "workspace role name for this prompt")
		showVersion          = fs.Bool("version", false, "print version and exit")
		enableCoding         = fs.Bool("coding", false, "enable local coding tools for this prompt")
		codingWorkDir        = fs.String("workdir", "", "workspace root for --coding (default current directory)")
		codingAllowOverwrite = fs.Bool("coding-allow-overwrite", false, "allow write_file to replace existing files after model and permission approval")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy — long-running personal-assistant agent built on glue.

Usage:
  peggy [flags] "<prompt text>"
  peggy init [flags]
  peggy skill [flags] <name>
  peggy skills [flags]
  peggy roles [flags]
  peggy memories [flags]
  peggy recall [flags] <query>
  peggy status [flags]
  peggy mcp [command]
  peggy serve [flags]

Examples:
  peggy "hello"
  peggy --session work "remind me about the migration plan"
  peggy --config /tmp/peggy.json "what do you know about my Aussie?"
  peggy --coding --workdir . "run the tests and fix the failure"
  peggy init --workdir .
  peggy skills --config ~/.config/peggy/settings.json
  peggy roles --config ~/.config/peggy/settings.json
  peggy memories --config ~/.config/peggy/settings.json
  peggy recall --config ~/.config/peggy/settings.json "Australian Shepherd"
  peggy skill --config ~/.config/peggy/settings.json --arg issue=GLUE-123 triage
  peggy status --config ~/.config/peggy/settings.json
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

	permission := cliPermissionForSettings(settings, stdin, stderr)
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

	promptOptions := promptOptionsForRole(*roleName)
	if _, err := p.PromptWithOptions(ctx, *sessionID, prompt, stdout, promptOptions...); err != nil {
		fmt.Fprintf(stderr, "\npeggy: prompt: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout) // trailing newline so shell prompts don't run on
	return 0
}

func cliPermissionForSettings(settings Settings, stdin io.Reader, stderr io.Writer) glue.Permission {
	if !settings.Coding.Enabled && !MCPEnabled(settings.MCP) {
		return nil
	}
	return NewTieredPermission(
		NewCLIPermission(CLIPermissionOptions{Stdin: stdin, Stderr: stderr}),
		PermissionTierForChannel(settings.Permissions, PermissionChannelCLI),
		PermissionChannelCLI,
	)
}

func promptOptionsForRole(roleName string) []glue.PromptOption {
	roleName = strings.TrimSpace(roleName)
	if roleName == "" {
		return nil
	}
	return []glue.PromptOption{glue.WithRole(roleName)}
}

type initStarterFile struct {
	Path    string
	Content string
}

func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		workDir = fs.String("workdir", ".", "workspace root to initialize")
		force   = fs.Bool("force", false, "overwrite existing starter files")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy init - create a starter Peggy workspace.

Usage:
  peggy init [flags]

Examples:
  peggy init --workdir .
  peggy init --workdir ~/workspace --force

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
		fmt.Fprintln(stderr, "peggy init: positional args not supported")
		return 2
	}
	root := strings.TrimSpace(*workDir)
	if root == "" {
		fmt.Fprintln(stderr, "peggy init: --workdir is required")
		return 2
	}
	expanded, err := expandPath(root)
	if err != nil {
		fmt.Fprintf(stderr, "peggy init: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(expanded, 0o755); err != nil {
		fmt.Fprintf(stderr, "peggy init: create %s: %v\n", expanded, err)
		return 1
	}
	for _, file := range peggyStarterFiles() {
		path := filepath.Join(expanded, file.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(stderr, "peggy init: create %s: %v\n", filepath.Dir(path), err)
			return 1
		}
		if !*force {
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(stdout, "skipped %s (exists)\n", file.Path)
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(stderr, "peggy init: stat %s: %v\n", path, err)
				return 1
			}
		}
		if err := os.WriteFile(path, []byte(file.Content), 0o644); err != nil {
			fmt.Fprintf(stderr, "peggy init: write %s: %v\n", path, err)
			return 1
		}
		if *force {
			fmt.Fprintf(stdout, "wrote %s\n", file.Path)
		} else {
			fmt.Fprintf(stdout, "created %s\n", file.Path)
		}
	}
	return 0
}

func peggyStarterFiles() []initStarterFile {
	return []initStarterFile{
		{
			Path: "AGENTS.md",
			Content: "# Peggy Workspace\n\n" +
				"Use this workspace context for local project conventions, active goals, and constraints.\n\n" +
				"- Prefer small, verifiable changes.\n" +
				"- Keep plans concrete and update them as work progresses.\n",
		},
		{
			Path: "roles/reviewer.md",
			Content: "---\nname: reviewer\ndescription: Review diffs for bugs, regressions, and missing tests\n---\n\n" +
				"Review like a senior engineer. Prioritize correctness, behavior changes, security, and test gaps. Lead with actionable findings tied to files or commands.\n",
		},
		{
			Path: "roles/operator.md",
			Content: "---\nname: operator\ndescription: Drive implementation work end to end\n---\n\n" +
				"Act as an implementation operator. Keep momentum, prefer repository patterns, verify changes locally, and summarize only the decisions and results that matter.\n",
		},
		{
			Path: ".agents/skills/triage/SKILL.md",
			Content: "---\nname: triage\ndescription: Triage one issue or task into an implementation plan\n---\n\n" +
				"Read the supplied context, identify the user-visible goal, list concrete acceptance criteria, note risks or unknowns, and propose the next implementation slice.\n",
		},
		{
			Path: ".agents/skills/daily_plan/SKILL.md",
			Content: "---\nname: daily_plan\ndescription: Produce a focused work plan for the current day\n---\n\n" +
				"Review the current project context and produce a short plan with priorities, blockers, validation steps, and the first concrete action.\n",
		},
		{
			Path: ".agents/skills/implementation_plan/SKILL.md",
			Content: "---\nname: implementation_plan\ndescription: Turn a task into a scoped build plan\n---\n\n" +
				"Break the task into a small implementation plan. Include files or subsystems to inspect, likely edits, tests to run, and rollout or follow-up notes.\n",
		},
	}
}

func runSkills(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy skills", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		jsonOutput = fs.Bool("json", false, "print machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy skills — list skills discovered from Peggy's configured workspace.

Usage:
  peggy skills [flags]

Examples:
  peggy skills --config ~/.config/peggy/settings.json
  peggy skills --json

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
		fmt.Fprintln(stderr, "peggy skills: positional args not supported")
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy skills: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy skills: no settings.json found; using built-in defaults")
	}
	catalog, err := loadSkillCatalog(settings.Context.WorkDir)
	if err != nil {
		fmt.Fprintf(stderr, "peggy skills: %v\n", err)
		return 1
	}
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(catalog); err != nil {
			fmt.Fprintf(stderr, "peggy skills: encode catalog: %v\n", err)
			return 1
		}
		return 0
	}
	writeSkillCatalog(stdout, catalog)
	return 0
}

func runRoles(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy roles", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		jsonOutput = fs.Bool("json", false, "print machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy roles — list roles discovered from Peggy's configured workspace.

Usage:
  peggy roles [flags]

Examples:
  peggy roles --config ~/.config/peggy/settings.json
  peggy roles --json

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
		fmt.Fprintln(stderr, "peggy roles: positional args not supported")
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy roles: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy roles: no settings.json found; using built-in defaults")
	}
	catalog, err := loadRoleCatalog(settings.Context.WorkDir)
	if err != nil {
		fmt.Fprintf(stderr, "peggy roles: %v\n", err)
		return 1
	}
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(catalog); err != nil {
			fmt.Fprintf(stderr, "peggy roles: encode catalog: %v\n", err)
			return 1
		}
		return 0
	}
	writeRoleCatalog(stdout, catalog)
	return 0
}

func runSkill(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy skill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath           = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		soulPath             = fs.String("soul", "", "path to identity Markdown (overrides $PEGGY_SOUL / XDG / ~/.config/peggy/SOUL.md)")
		sessionID            = fs.String("session", "default", "session id (file-backed transcripts key off this)")
		roleName             = fs.String("role", "", "workspace role name for this skill run")
		enableCoding         = fs.Bool("coding", false, "enable local coding tools for this skill run")
		codingWorkDir        = fs.String("workdir", "", "workspace root for --coding (default current directory)")
		codingAllowOverwrite = fs.Bool("coding-allow-overwrite", false, "allow write_file to replace existing files after model and permission approval")
		skillArgs            stringListFlag
	)
	fs.Var(&skillArgs, "arg", "skill argument as key=value (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy skill — run one skill discovered from Peggy's configured workspace.

Usage:
  peggy skill [flags] <name>

Examples:
  peggy skill --config ~/.config/peggy/settings.json --arg issue=GLUE-123 triage
  peggy skill --session work daily_plan

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
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "peggy skill: exactly one skill name is required")
		return 2
	}
	skillName := strings.TrimSpace(fs.Arg(0))
	if skillName == "" {
		fmt.Fprintln(stderr, "peggy skill: skill name is required")
		return 2
	}
	parsedArgs, err := parsePromptArgs(skillArgs)
	if err != nil {
		fmt.Fprintf(stderr, "peggy skill: %v\n", err)
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy skill: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy skill: no settings.json found; using built-in defaults")
	}
	applyCodingFlags(&settings, *enableCoding, *codingWorkDir, *codingAllowOverwrite)

	soul, soulPathUsed, err := LoadSoul(*soulPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy skill: %v\n", err)
		return 1
	}
	if soul == "" {
		fmt.Fprintln(stderr, "peggy skill: no SOUL.md found; running without identity context")
	} else {
		fmt.Fprintf(stderr, "peggy skill: identity loaded from %s (%d bytes)\n", soulPathUsed, len(soul))
	}
	if settings.Coding.Enabled {
		workDir := settings.Coding.WorkDir
		if strings.TrimSpace(workDir) == "" {
			workDir = "."
		}
		fmt.Fprintf(stderr, "peggy skill: coding tools enabled for %s\n", workDir)
	}

	p, err := New(Options{
		Settings:   settings,
		Soul:       soul,
		Stderr:     stderr,
		Permission: cliPermissionForSettings(settings, stdin, stderr),
	})
	if err != nil {
		fmt.Fprintf(stderr, "peggy skill: setup: %v\n", err)
		return 1
	}
	defer p.Close()

	promptOptions := promptOptionsForRole(*roleName)
	if _, err := p.SkillWithOptions(ctx, *sessionID, skillName, parsedArgs, stdout, promptOptions...); err != nil {
		fmt.Fprintf(stderr, "\npeggy skill: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout)
	return 0
}

func loadSkillCatalog(workDir string) ([]skillCatalogEntry, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return nil, nil
	}
	ctx, err := glue.LoadContext(workDir)
	if err != nil {
		return nil, err
	}
	names := sortedMapKeys(ctx.Skills)
	catalog := make([]skillCatalogEntry, 0, len(names))
	for _, name := range names {
		skill := ctx.Skills[name]
		catalog = append(catalog, skillCatalogEntry{
			Name:        skill.Name,
			Description: skill.Description,
		})
	}
	return catalog, nil
}

func loadRoleCatalog(workDir string) ([]roleCatalogEntry, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return nil, nil
	}
	ctx, err := glue.LoadContext(workDir)
	if err != nil {
		return nil, err
	}
	names := sortedMapKeys(ctx.Roles)
	catalog := make([]roleCatalogEntry, 0, len(names))
	for _, name := range names {
		role := ctx.Roles[name]
		catalog = append(catalog, roleCatalogEntry{
			Name:        role.Name,
			Description: role.Description,
			Model:       role.Model,
		})
	}
	return catalog, nil
}

func writeSkillCatalog(w io.Writer, catalog []skillCatalogEntry) {
	if len(catalog) == 0 {
		fmt.Fprintln(w, "No Peggy skills configured.")
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
	}
}

func writeRoleCatalog(w io.Writer, catalog []roleCatalogEntry) {
	if len(catalog) == 0 {
		fmt.Fprintln(w, "No Peggy roles configured.")
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
		if entry.Model != "" {
			fmt.Fprintf(w, "  model: %s\n", entry.Model)
		}
	}
}

func runMemories(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "forget" {
		return runMemoriesForget(ctx, args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("peggy memories", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		jsonOutput = fs.Bool("json", false, "print machine-readable JSON")
		limit      = fs.Int("limit", 0, "maximum memories to return; 0 means no limit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy memories - list curated memories from Peggy's store.

Usage:
  peggy memories [flags]

Examples:
  peggy memories --config ~/.config/peggy/settings.json
  peggy memories --config ~/.config/peggy/settings.json --json
  peggy memories --limit 20
  peggy memories forget <id>

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
		fmt.Fprintln(stderr, "peggy memories: positional args not supported")
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "peggy memories: --limit must be non-negative")
		return 2
	}

	store, missingSettings, err := openStoreForRunner(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy memories: %v\n", err)
		return 1
	}
	if missingSettings {
		fmt.Fprintln(stderr, "peggy memories: no settings.json found; using built-in defaults")
	}
	if closer, ok := store.(io.Closer); ok {
		defer closer.Close()
	}

	p := &Peggy{store: store}
	memories, err := p.ListMemories(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "peggy memories: %v\n", err)
		return 1
	}
	if *limit > 0 && len(memories) > *limit {
		memories = memories[:*limit]
	}
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(memories); err != nil {
			fmt.Fprintf(stderr, "peggy memories: encode memories: %v\n", err)
			return 1
		}
		return 0
	}
	writeMemories(stdout, memories)
	return 0
}

func runMemoriesForget(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy memories forget", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy memories forget - delete one curated memory by id.

Usage:
  peggy memories forget [flags] <id>

Examples:
  peggy memories forget --config ~/.config/peggy/settings.json mem_123

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
	id, err := parseMemoriesForgetArgs(fs.Args(), configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy memories forget: %v\n", err)
		return 2
	}
	store, missingSettings, err := openStoreForRunner(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy memories forget: %v\n", err)
		return 1
	}
	if missingSettings {
		fmt.Fprintln(stderr, "peggy memories forget: no settings.json found; using built-in defaults")
	}
	if closer, ok := store.(io.Closer); ok {
		defer closer.Close()
	}

	p := &Peggy{store: store}
	removed, err := p.ForgetMemory(ctx, id)
	if err != nil {
		fmt.Fprintf(stderr, "peggy memories forget: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "forgot %s\n", removed.ID)
	fmt.Fprintf(stdout, "  content: %s\n", singleLine(removed.Content))
	return 0
}

func parseMemoriesForgetArgs(args []string, configPath *string) (string, error) {
	var id string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--config" || arg == "-config":
			if i+1 >= len(args) {
				return "", errors.New("--config requires a value")
			}
			*configPath = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config="):
			*configPath = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-config="):
			*configPath = strings.TrimPrefix(arg, "-config=")
		case strings.HasPrefix(arg, "-"):
			return "", fmt.Errorf("unknown flag %s", arg)
		default:
			if id != "" {
				return "", errors.New("exactly one memory id is required")
			}
			id = strings.TrimSpace(arg)
		}
	}
	if id == "" {
		return "", errors.New("exactly one memory id is required")
	}
	return id, nil
}

func openStoreForRunner(configPath string) (glue.Store, bool, error) {
	settings, settingsPath, err := LoadSettings(configPath)
	if err != nil {
		return nil, false, err
	}
	store, err := buildStore(settings)
	if err != nil {
		return nil, settingsPath == "", fmt.Errorf("store: %w", err)
	}
	return store, settingsPath == "", nil
}

func writeMemories(w io.Writer, memories []Memory) {
	if len(memories) == 0 {
		fmt.Fprintln(w, "No memories recorded.")
		return
	}
	for i, memory := range memories {
		if i > 0 {
			fmt.Fprintln(w)
		}
		timestamp := "unknown"
		if !memory.Timestamp.IsZero() {
			timestamp = memory.Timestamp.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\n", timestamp)
		fmt.Fprintf(w, "  id: %s\n", memory.ID)
		fmt.Fprintf(w, "  content: %s\n", singleLine(memory.Content))
		if len(memory.Tags) > 0 {
			fmt.Fprintf(w, "  tags: %s\n", strings.Join(memory.Tags, ", "))
		}
	}
}

func runRecall(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy recall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath   = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		jsonOutput   = fs.Bool("json", false, "print machine-readable JSON")
		memoriesOnly = fs.Bool("memories", false, "search only curated memories")
		limit        = fs.Int("limit", 0, "maximum hits to return; 0 uses the store default")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy recall - search stored Peggy sessions.

Usage:
  peggy recall [flags] <query>

Examples:
  peggy recall --config ~/.config/peggy/settings.json "Australian Shepherd"
  peggy recall --config ~/.config/peggy/settings.json --memories "preference"
  peggy recall --config ~/.config/peggy/settings.json --json "project"

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
	if *limit < 0 {
		fmt.Fprintln(stderr, "peggy recall: --limit must be non-negative")
		return 2
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		fmt.Fprintln(stderr, "peggy recall: query is required")
		return 2
	}

	store, missingSettings, err := openStoreForRunner(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy recall: %v\n", err)
		return 1
	}
	if missingSettings {
		fmt.Fprintln(stderr, "peggy recall: no settings.json found; using built-in defaults")
	}
	if closer, ok := store.(io.Closer); ok {
		defer closer.Close()
	}

	searcher := glue.NewAgent(glue.AgentOptions{Store: store})
	searchOptions := []glue.SearchOption{}
	if *limit > 0 {
		searchOptions = append(searchOptions, glue.WithLimit(*limit))
	}
	if *memoriesOnly {
		searchOptions = append(searchOptions, glue.WithSessionID(MemoriesSessionID))
	}
	hits, err := searcher.SearchSessions(ctx, query, searchOptions...)
	if err != nil {
		if errors.Is(err, glue.ErrSearchNotSupported) {
			fmt.Fprintln(stderr, "peggy recall: configured store does not support search; use sqlite store")
			return 1
		}
		fmt.Fprintf(stderr, "peggy recall: %v\n", err)
		return 1
	}
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(hits); err != nil {
			fmt.Fprintf(stderr, "peggy recall: encode hits: %v\n", err)
			return 1
		}
		return 0
	}
	writeRecallHits(stdout, hits)
	return 0
}

func writeRecallHits(w io.Writer, hits []glue.SearchHit) {
	if len(hits) == 0 {
		fmt.Fprintln(w, "No recall hits.")
		return
	}
	for i, hit := range hits {
		if i > 0 {
			fmt.Fprintln(w)
		}
		timestamp := "unknown"
		if !hit.Timestamp.IsZero() {
			timestamp = hit.Timestamp.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s %s[%d] %s\n", timestamp, hit.SessionID, hit.Index, hit.Role)
		fmt.Fprintf(w, "  snippet: %s\n", singleLine(hit.Snippet))
	}
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		soulPath   = fs.String("soul", "", "path to identity Markdown (overrides $PEGGY_SOUL / XDG / ~/.config/peggy/SOUL.md)")
		jsonOutput = fs.Bool("json", false, "print machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy status — show local Peggy readiness.

Usage:
  peggy status [flags]

Examples:
  peggy status
  peggy status --config ~/.config/peggy/settings.json --soul ~/.config/peggy/SOUL.md
  peggy status --json

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
		fmt.Fprintln(stderr, "peggy status: positional args not supported")
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy status: %v\n", err)
		return 1
	}
	soul, soulPathUsed, err := LoadSoul(*soulPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy status: %v\n", err)
		return 1
	}
	report := buildStatusReport(settings, settingsPath, soul, soulPathUsed)
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "peggy status: encode status: %v\n", err)
			return 1
		}
		return 0
	}
	writeStatusReport(stdout, report)
	return 0
}

func buildStatusReport(settings Settings, settingsPath, soul, soulPath string) statusReport {
	channels := sortedMapKeys(settings.Channels)
	mcpServers := buildStatusMCP(settings.MCP)
	return statusReport{
		Version: Version,
		Settings: statusFile{
			Path:  settingsPath,
			Found: settingsPath != "",
			Bytes: fileSize(settingsPath),
		},
		Identity: statusFile{
			Path:  soulPath,
			Found: soulPath != "",
			Bytes: len([]byte(soul)),
		},
		Provider: statusProvider{
			Name:  settings.Provider,
			Model: statusModel(settings.Model),
		},
		Store:       settings.Store,
		Compaction:  settings.Compaction,
		Context:     buildStatusContext(settings.Context),
		Coding:      buildStatusCoding(settings.Coding),
		Permissions: settings.Permissions,
		Channels:    channels,
		MCP:         mcpServers,
	}
}

func buildStatusContext(contextSettings ContextSettings) statusContext {
	workDir := strings.TrimSpace(contextSettings.WorkDir)
	return statusContext{
		Enabled: workDir != "",
		WorkDir: workDir,
	}
}

func buildStatusCoding(coding CodingSettings) statusCoding {
	workDir := coding.WorkDir
	if coding.Enabled && strings.TrimSpace(workDir) == "" {
		workDir = "."
	}
	return statusCoding{
		Enabled:         coding.Enabled,
		WorkDir:         workDir,
		AllowedBinaries: append([]string(nil), coding.AllowedBinaries...),
		AllowOverwrite:  coding.AllowOverwrite,
	}
}

func buildStatusMCP(settings MCPSettings) statusMCP {
	names := make([]string, 0, len(settings.Servers))
	for name := range settings.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := statusMCP{Configured: len(names)}
	for _, name := range names {
		server := settings.Servers[name]
		transport := strings.TrimSpace(server.Transport)
		if transport == "" {
			transport = "stdio"
		}
		entry := statusMCPServer{
			Name:      name,
			Enabled:   server.Enabled,
			Transport: transport,
			Command:   server.Command,
			URL:       server.URL,
		}
		if entry.Enabled {
			out.Enabled++
		}
		out.Servers = append(out.Servers, entry)
	}
	return out
}

func writeStatusReport(w io.Writer, report statusReport) {
	fmt.Fprintf(w, "Peggy %s\n", report.Version)
	if report.Settings.Found {
		fmt.Fprintf(w, "settings: %s (%d bytes)\n", report.Settings.Path, report.Settings.Bytes)
	} else {
		fmt.Fprintln(w, "settings: built-in defaults")
	}
	if report.Identity.Found {
		fmt.Fprintf(w, "identity: %s (%d bytes)\n", report.Identity.Path, report.Identity.Bytes)
	} else {
		fmt.Fprintln(w, "identity: none")
	}
	fmt.Fprintf(w, "provider: %s %s\n", report.Provider.Name, report.Provider.Model)
	fmt.Fprintf(w, "store: %s %s\n", report.Store.Type, report.Store.Path)
	fmt.Fprintf(w, "compaction: threshold=%d target=%d keep=%d\n", report.Compaction.Threshold, report.Compaction.TargetTokens, report.Compaction.KeepRecent)
	writeStatusContext(w, report.Context)
	writeStatusCoding(w, report.Coding)
	writeStatusPermissions(w, report.Permissions)
	if len(report.Channels) > 0 {
		fmt.Fprintf(w, "channels: %s\n", strings.Join(report.Channels, ", "))
	} else {
		fmt.Fprintln(w, "channels: none")
	}
	writeStatusMCP(w, report.MCP)
}

func writeStatusContext(w io.Writer, contextSettings statusContext) {
	if !contextSettings.Enabled {
		fmt.Fprintln(w, "context: disabled")
		return
	}
	fmt.Fprintf(w, "context: enabled work_dir=%s\n", contextSettings.WorkDir)
}

func writeStatusCoding(w io.Writer, coding statusCoding) {
	state := "disabled"
	if coding.Enabled {
		state = "enabled"
	}
	fmt.Fprintf(w, "coding: %s", state)
	if coding.WorkDir != "" {
		fmt.Fprintf(w, " work_dir=%s", coding.WorkDir)
	}
	if coding.AllowOverwrite {
		fmt.Fprint(w, " allow_overwrite=true")
	}
	fmt.Fprintln(w)
	if len(coding.AllowedBinaries) > 0 {
		fmt.Fprintf(w, "coding_binaries: %s\n", strings.Join(coding.AllowedBinaries, ", "))
	}
}

func writeStatusPermissions(w io.Writer, permissions PermissionSettings) {
	fmt.Fprintf(w, "permissions: default=%s", permissions.DefaultTier)
	keys := sortedStringMapKeys(permissions.Channels)
	for _, key := range keys {
		fmt.Fprintf(w, " %s=%s", key, permissions.Channels[key])
	}
	fmt.Fprintln(w)
}

func writeStatusMCP(w io.Writer, mcp statusMCP) {
	fmt.Fprintf(w, "mcp: %d configured, %d enabled\n", mcp.Configured, mcp.Enabled)
	for _, server := range mcp.Servers {
		state := "disabled"
		if server.Enabled {
			state = "enabled"
		}
		fmt.Fprintf(w, "  - %s: %s transport=%s", server.Name, state, server.Transport)
		if server.Command != "" {
			fmt.Fprintf(w, " command=%s", server.Command)
		}
		if server.URL != "" {
			fmt.Fprintf(w, " url=%s", server.URL)
		}
		fmt.Fprintln(w)
	}
}

func statusModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "(provider default)"
	}
	return model
}

func fileSize(path string) int {
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return int(info.Size())
}

func sortedMapKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringMapKeys(m map[string]string) []string {
	return sortedMapKeys(m)
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

type stringListFlag []string

func (v *stringListFlag) String() string {
	return strings.Join(*v, ",")
}

func (v *stringListFlag) Set(s string) error {
	*v = append(*v, s)
	return nil
}

func runMCP(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprintf(stderr, `peggy mcp — inspect Peggy's configured MCP surface.

Usage:
  peggy mcp prompt [flags]
  peggy mcp prompts [flags]
  peggy mcp read [flags]
  peggy mcp resources [flags]
  peggy mcp tools [flags]

Commands:
  prompt     Render one prompt from an enabled MCP server.
  prompts    List prompts discovered from enabled MCP servers.
  read       Read one resource URI from an enabled MCP server.
  resources  List resources discovered from enabled MCP servers.
  tools      List tools discovered from enabled MCP servers.
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
	case "prompt":
		return runMCPPrompt(ctx, args[1:], stdout, stderr)
	case "prompts":
		return runMCPPrompts(ctx, args[1:], stdout, stderr)
	case "read":
		return runMCPRead(ctx, args[1:], stdout, stderr)
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

func runMCPPrompt(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy mcp prompt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		serverName = fs.String("server", "", "configured MCP server name")
		promptName = fs.String("name", "", "MCP prompt name to render")
		jsonOutput = fs.Bool("json", false, "print machine-readable JSON")
		promptArgs stringListFlag
	)
	fs.Var(&promptArgs, "arg", "prompt argument as key=value (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy mcp prompt — render one prompt from an enabled MCP server.

Usage:
  peggy mcp prompt --server <name> --name <prompt> [flags]

Examples:
  peggy mcp prompt --config ~/.config/peggy/settings.json --server linear --name summarize_issue --arg issue=GLUE-123
  peggy mcp prompt --server linear --name summarize_issue --arg issue=GLUE-123 --json

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
		fmt.Fprintln(stderr, "peggy mcp prompt: positional args not supported")
		return 2
	}
	if strings.TrimSpace(*serverName) == "" {
		fmt.Fprintln(stderr, "peggy mcp prompt: --server is required")
		return 2
	}
	if strings.TrimSpace(*promptName) == "" {
		fmt.Fprintln(stderr, "peggy mcp prompt: --name is required")
		return 2
	}
	parsedArgs, err := parsePromptArgs(promptArgs)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp prompt: %v\n", err)
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp prompt: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy mcp prompt: no settings.json found; using built-in defaults")
	}

	prompt, manager, _, err := MCPGetPrompt(ctx, settings.MCP, *serverName, *promptName, parsedArgs)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp prompt: %v\n", err)
		return 1
	}
	defer func() {
		if err := manager.Close(); err != nil {
			fmt.Fprintf(stderr, "peggy mcp prompt: close: %v\n", err)
		}
	}()

	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(prompt); err != nil {
			fmt.Fprintf(stderr, "peggy mcp prompt: encode prompt: %v\n", err)
			return 1
		}
		return 0
	}
	writeMCPPrompt(stdout, prompt)
	return 0
}

func runMCPPrompts(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy mcp prompts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		jsonOutput = fs.Bool("json", false, "print machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy mcp prompts — list prompts from enabled MCP servers.

Usage:
  peggy mcp prompts [flags]

Examples:
  peggy mcp prompts --config ~/.config/peggy/settings.json
  peggy mcp prompts --json

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
		fmt.Fprintln(stderr, "peggy mcp prompts: positional args not supported")
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp prompts: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy mcp prompts: no settings.json found; using built-in defaults")
	}

	prompts, manager, _, err := MCPPrompts(ctx, settings.MCP)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp prompts: %v\n", err)
		return 1
	}
	defer func() {
		if err := manager.Close(); err != nil {
			fmt.Fprintf(stderr, "peggy mcp prompts: close: %v\n", err)
		}
	}()

	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(prompts); err != nil {
			fmt.Fprintf(stderr, "peggy mcp prompts: encode catalog: %v\n", err)
			return 1
		}
		return 0
	}
	writeMCPPromptCatalog(stdout, prompts)
	return 0
}

func runMCPRead(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy mcp read", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		serverName = fs.String("server", "", "configured MCP server name")
		uri        = fs.String("uri", "", "MCP resource URI to read")
		jsonOutput = fs.Bool("json", false, "print machine-readable JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy mcp read — read one resource from an enabled MCP server.

Usage:
  peggy mcp read --server <name> --uri <uri> [flags]

Examples:
  peggy mcp read --config ~/.config/peggy/settings.json --server filesystem --uri file:///workspace/README.md
  peggy mcp read --server filesystem --uri file:///workspace/README.md --json

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
		fmt.Fprintln(stderr, "peggy mcp read: positional args not supported")
		return 2
	}
	if strings.TrimSpace(*serverName) == "" {
		fmt.Fprintln(stderr, "peggy mcp read: --server is required")
		return 2
	}
	if strings.TrimSpace(*uri) == "" {
		fmt.Fprintln(stderr, "peggy mcp read: --uri is required")
		return 2
	}

	settings, settingsPath, err := LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp read: %v\n", err)
		return 1
	}
	if settingsPath == "" {
		fmt.Fprintln(stderr, "peggy mcp read: no settings.json found; using built-in defaults")
	}

	read, manager, _, err := MCPReadResource(ctx, settings.MCP, *serverName, *uri)
	if err != nil {
		fmt.Fprintf(stderr, "peggy mcp read: %v\n", err)
		return 1
	}
	defer func() {
		if err := manager.Close(); err != nil {
			fmt.Fprintf(stderr, "peggy mcp read: close: %v\n", err)
		}
	}()

	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(read); err != nil {
			fmt.Fprintf(stderr, "peggy mcp read: encode resource: %v\n", err)
			return 1
		}
		return 0
	}
	writeMCPResourceRead(stdout, read)
	return 0
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

func writeMCPPromptCatalog(w io.Writer, prompts []toolsmcp.Prompt) {
	if len(prompts) == 0 {
		fmt.Fprintln(w, "No MCP prompts configured.")
		return
	}
	for i, prompt := range prompts {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, prompt.Name)
		fmt.Fprintf(w, "  server: %s\n", prompt.Server)
		if prompt.Title != "" {
			fmt.Fprintf(w, "  title: %s\n", singleLine(prompt.Title))
		}
		if prompt.Description != "" {
			fmt.Fprintf(w, "  description: %s\n", singleLine(prompt.Description))
		}
		if len(prompt.Arguments) > 0 {
			fmt.Fprintln(w, "  arguments:")
			for _, arg := range prompt.Arguments {
				required := ""
				if arg.Required {
					required = " required"
				}
				line := arg.Name + required
				if arg.Description != "" {
					line += " - " + singleLine(arg.Description)
				}
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
	}
}

func writeMCPPrompt(w io.Writer, prompt toolsmcp.PromptGet) {
	fmt.Fprintln(w, prompt.Name)
	fmt.Fprintf(w, "  server: %s\n", prompt.Server)
	if prompt.Description != "" {
		fmt.Fprintf(w, "  description: %s\n", singleLine(prompt.Description))
	}
	if len(prompt.Messages) == 0 {
		fmt.Fprintln(w, "  messages: []")
		return
	}
	fmt.Fprintln(w, "  messages:")
	for _, message := range prompt.Messages {
		fmt.Fprintf(w, "    - role: %s\n", message.Role)
		if text, ok := promptTextContent(message.Content); ok {
			fmt.Fprintln(w, "      text:")
			for _, line := range strings.Split(text, "\n") {
				fmt.Fprintf(w, "        %s\n", line)
			}
			continue
		}
		fmt.Fprintf(w, "      content: %s\n", compactJSONLine(message.Content))
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

func writeMCPResourceRead(w io.Writer, read toolsmcp.ResourceRead) {
	if len(read.Contents) == 0 {
		fmt.Fprintln(w, "No MCP resource contents returned.")
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
				fmt.Fprintf(w, "  meta: %s\n", compactJSONLine(raw))
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

func parsePromptArgs(values []string) (map[string]string, error) {
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

func promptTextContent(raw json.RawMessage) (string, bool) {
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
		Host:              p,
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
