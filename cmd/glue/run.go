// The "glue run" subcommand: one-shot, scripted, and interactive (TUI)
// agent runs, including coding-tool assembly and local permission prompts.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"sort"

	"github.com/erain/glue"
	"github.com/erain/glue/cmd/glue/atmentions"
	"github.com/erain/glue/cmd/glue/tui"
	filestore "github.com/erain/glue/stores/file"
	toolscoding "github.com/erain/glue/tools/coding"
	// Register the shipped providers so they resolve through the
	// providers registry by name (--provider). Importing for side
)

type codingFlagConfig struct {
	Enabled         bool
	WorkDir         string
	AllowedBinaries []string
	AllowOverwrite  bool
}

type agentConfig struct {
	Provider   string
	Model      string
	StoreDir   string
	WorkDir    string
	Tools      []glue.Tool
	Permission glue.Permission
	// Coding selects the assembled coding system prompt (built from
	// the active toolset in the provider's preferred variant).
	Coding bool
}

func runCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, newProvider providerFactory) error {
	flags := flag.NewFlagSet("glue run", flag.ContinueOnError)
	flags.SetOutput(stderr)

	id := flags.String("id", "default", "session id")
	prompt := flags.String("prompt", "", "prompt text")
	provider := flags.String("provider", defaultProvider, "provider name: codex, gemini, nvidia, or openrouter")
	model := flags.String("model", "", "model id (default: the provider's default model); gemini/<model> accepted")
	storeDir := flags.String("store", ".glue/sessions", "session store directory")
	workDir := flags.String("work", ".", "working directory for AGENTS.md, skills, roles, and optional coding tools")
	coding := flags.Bool("coding", false, "enable local coding tools for this run")
	codingAllowOverwrite := flags.Bool("coding-allow-overwrite", false, "allow write_file to replace existing files after model and permission approval")
	yolo := flags.Bool("yolo", false, "auto-allow all side-effecting tool calls (write_file / edit_file / shell_exec / mcp). implies --coding-allow-overwrite. always use on a feature branch.")
	showUsage := flags.Bool("usage", false, "print token usage summary to stderr when available")
	mode := flags.String("mode", "text", `output mode for one-shot runs: "text" (default streamed text) or "json" (JSON-Lines events)`)
	toolsAllow := flags.String("tools", "", "comma-separated allowlist of tool names to register (empty = all configured)")
	noTools := flags.Bool("no-tools", false, "register zero tools (read-only/text-only run)")
	usagePricing := registerUsagePricingFlags(flags)
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")
	var allowedBinaries repeatedStrings
	flags.Var(&allowedBinaries, "allow-binary", "allowed shell_exec binary basename for --coding; repeatable")

	if err := flags.Parse(args); err != nil {
		return err
	}
	markUsagePricingFlagState(flags, usagePricing)
	if err := validateUsagePricing(*usagePricing); err != nil {
		return err
	}

	agentName := "default"
	if flags.NArg() > 0 {
		agentName = flags.Arg(0)
	}
	if agentName != "default" && agentName != "gemini" {
		return fmt.Errorf("unknown agent %q; only 'default' is available in this runner", agentName)
	}

	// Resolve the prompt and the run mode:
	//   - --prompt set                            → one-shot (today's behavior, unchanged)
	//   - --prompt empty + stdin not a TTY        → read stdin as the prompt, one-shot
	//   - --prompt empty + stdin AND stdout TTYs  → interactive TUI
	//   - --prompt empty + only stdin TTY         → refuse (no place to draw)
	effectivePrompt := strings.TrimSpace(*prompt)
	interactive := false
	if effectivePrompt == "" {
		stdinTTY := isFileTTY(stdin)
		stdoutTTY := isFileTTY(stdout)
		switch {
		case !stdinTTY:
			// Piped or test input. Read everything as the prompt.
			data, err := io.ReadAll(stdin)
			if err != nil {
				return fmt.Errorf("read prompt from stdin: %w", err)
			}
			effectivePrompt = strings.TrimSpace(string(data))
			if effectivePrompt == "" {
				return errors.New("missing required --prompt (and stdin was empty)")
			}
		case stdinTTY && stdoutTTY:
			interactive = true
		default:
			return errors.New("missing required --prompt (stdout is not a terminal; interactive mode unavailable)")
		}
	}

	providerName, effectiveModel, err := resolveProvider(*provider, *model)
	if err != nil {
		return err
	}
	// Validate output mode. The TUI ignores --mode (it has its own
	// rendering); --mode json applies to one-shot text-streaming runs.
	switch strings.ToLower(*mode) {
	case "", "text", "json":
		// ok
	default:
		return fmt.Errorf("unknown --mode %q (want text or json)", *mode)
	}
	if interactive && strings.ToLower(*mode) == "json" {
		return errors.New("--mode json is only valid with --prompt or piped stdin (interactive TUI has its own output)")
	}
	jsonMode := strings.EqualFold(*mode, "json")

	if err := loadEnvFiles(envs); err != nil {
		return err
	}

	// --yolo upgrades the overwrite policy too: the user explicitly told
	// the model "do whatever," so making write_file ask permission to
	// overwrite would be incoherent.
	effectiveAllowOverwrite := *codingAllowOverwrite
	if *yolo {
		effectiveAllowOverwrite = true
	}

	tools, _, err := buildCodingTools(codingFlagConfig{
		Enabled:         *coding,
		WorkDir:         *workDir,
		AllowedBinaries: append([]string(nil), allowedBinaries...),
		AllowOverwrite:  effectiveAllowOverwrite,
	})
	if err != nil {
		return err
	}
	// Filter the tool set down: --no-tools strips them all (a "text-only"
	// run with no agentic capabilities); --tools is an allowlist by name.
	// Both apply on top of --coding so the user can say
	// `--coding --tools read_file,grep` for a read-only coding agent.
	tools, err = filterTools(tools, *toolsAllow, *noTools)
	if err != nil {
		return err
	}
	// @file expansion happens after env load (so any path side-effects
	// are honored) and before agent construction. Skipped mentions are
	// reported to stderr; the user's original "@path" stays in the
	// prompt verbatim so the model can see they asked.
	atRes, err := atmentions.Expand(effectivePrompt, atmentions.Options{WorkDir: *workDir})
	if err != nil {
		return fmt.Errorf("expand @-mentions: %w", err)
	}
	effectivePrompt = atRes.Prompt
	for _, skip := range atRes.Skipped {
		fmt.Fprintf(stderr, "glue run: %s: %s\n", skip.Mention, skip.Reason)
	}

	// In interactive mode the TUI installs its own permission bridge
	// per-prompt; the agent-level Permission is left nil so the bridge
	// gets to render the prompt instead of fighting with the readline
	// version. --yolo overrides both paths: every side-effecting tool
	// call is auto-approved without a prompt round-trip.
	var permission glue.Permission
	switch {
	case *yolo:
		permission = yoloPermission{}
		fmt.Fprintf(stderr, "glue run: --yolo enabled; permission prompts are off (use on a feature branch).\n")
	case *coding && !interactive:
		permission = newLocalPromptPermission(stdin, stderr)
		fmt.Fprintf(stderr, "glue run: coding tools enabled for %s\n", *workDir)
	}

	// Construct the provider and store once so the TUI can reuse them
	// for /compact (needs a Provider for SummarizingCompactor) and
	// /resume (needs the Store to list sessions). The agent then takes
	// the same pair via AgentOptions.
	providerImpl, err := newProvider(providerName)
	if err != nil {
		return err
	}
	storeImpl := filestore.New(*storeDir)
	systemPrompt, autoContinue := capabilityDefaults(providerName, tools, *coding)
	agent := glue.NewAgent(glue.AgentOptions{
		Provider:     providerImpl,
		Model:        normalizeModel(effectiveModel),
		Tools:        append([]glue.Tool(nil), tools...),
		Store:        storeImpl,
		WorkDir:      *workDir,
		Permission:   permission,
		SystemPrompt: systemPrompt,
		AutoContinue: autoContinue,
	})

	if interactive {
		// buildToolsAt re-roots the run's coding tool set for /goal -w
		// worktree isolation, honoring the same flags as the main set.
		// Only offered when --coding is on: an isolated goal without
		// coding tools could not touch its worktree anyway.
		var buildToolsAt func(dir string) ([]glue.Tool, error)
		if *coding {
			buildToolsAt = func(dir string) ([]glue.Tool, error) {
				wtTools, _, err := buildCodingTools(codingFlagConfig{
					Enabled:         true,
					WorkDir:         dir,
					AllowedBinaries: append([]string(nil), allowedBinaries...),
					AllowOverwrite:  effectiveAllowOverwrite,
				})
				if err != nil {
					return nil, err
				}
				return filterTools(wtTools, *toolsAllow, *noTools)
			}
		}
		return tui.Run(ctx, tui.Config{
			Agent:        agent,
			SessionID:    *id,
			Provider:     providerName,
			Model:        effectiveModel,
			WorkDir:      *workDir,
			Tools:        tools,
			Store:        storeImpl,
			ProviderImpl: providerImpl,
			AlwaysAllow:  *yolo,
			BuildTools:   buildToolsAt,
		})
	}
	session, err := agent.Session(ctx, *id)
	if err != nil {
		return err
	}

	if jsonMode {
		return runOneShotJSON(ctx, session, effectivePrompt, *id, providerName, effectiveModel, stdout, *showUsage, usagePricing, stderr)
	}

	wroteDelta := false
	response, err := session.Prompt(ctx, effectivePrompt, glue.WithEvents(func(event glue.Event) {
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
		writeUsageSummary(stderr, summarizeUsage(response.NewMessages), *usagePricing)
	}
	return nil
}

// runOneShotJSON streams loop events to stdout as JSON Lines. Event
// shape is intentionally small and stable across releases so scripts
// (editors, CI, /codeagent integrations) can consume it:
//
//	{"type":"start","session":"...","provider":"...","model":"..."}
//	{"type":"text","delta":"..."}
//	{"type":"tool_start","call_id":"...","name":"...","args":{...}}
//	{"type":"tool_end","call_id":"...","is_error":false,"text":"..."}
//	{"type":"done","text":"...","stop_reason":"..."}
//	{"type":"error","message":"..."}
func runOneShotJSON(
	ctx context.Context,
	session *glue.Session,
	prompt, sessionID, providerName, modelName string,
	stdout io.Writer,
	showUsage bool,
	pricing *usagePricing,
	stderr io.Writer,
) error {
	enc := json.NewEncoder(stdout)
	emit := func(event map[string]any) {
		_ = enc.Encode(event)
	}
	emit(map[string]any{
		"type":     "start",
		"session":  sessionID,
		"provider": providerName,
		"model":    modelName,
	})
	response, err := session.Prompt(ctx, prompt, glue.WithEvents(func(event glue.Event) {
		switch event.Type {
		case glue.EventTextDelta:
			if event.Delta == "" {
				return
			}
			emit(map[string]any{"type": "text", "delta": event.Delta})
		case glue.EventToolStart:
			if event.ToolCall == nil {
				return
			}
			var args any
			if len(event.ToolCall.Arguments) > 0 {
				_ = json.Unmarshal(event.ToolCall.Arguments, &args)
			}
			emit(map[string]any{
				"type":    "tool_start",
				"call_id": event.ToolCall.ID,
				"name":    event.ToolCall.Name,
				"args":    args,
			})
		case glue.EventToolEnd:
			text := ""
			isErr := false
			if event.ToolResult != nil {
				isErr = event.ToolResult.IsError
				var parts []string
				for _, p := range event.ToolResult.Content {
					if p.Type == glue.ContentTypeText && p.Text != "" {
						parts = append(parts, p.Text)
					}
				}
				text = strings.Join(parts, "\n")
			}
			emit(map[string]any{
				"type":     "tool_end",
				"call_id":  event.ToolCallID,
				"is_error": isErr,
				"text":     text,
			})
		case glue.EventError:
			if event.Error != "" {
				emit(map[string]any{"type": "error", "message": event.Error})
			}
		}
	}))
	if err != nil {
		emit(map[string]any{"type": "error", "message": err.Error()})
		return err
	}
	stopReason := ""
	if len(response.NewMessages) > 0 {
		last := response.NewMessages[len(response.NewMessages)-1]
		stopReason = string(last.StopReason)
	}
	emit(map[string]any{
		"type":        "done",
		"text":        response.Text,
		"stop_reason": stopReason,
	})
	if showUsage {
		writeUsageSummary(stderr, summarizeUsage(response.NewMessages), *pricing)
	}
	return nil
}

// filterTools applies --tools / --no-tools to the configured tool set.
func filterTools(in []glue.Tool, allowCSV string, noTools bool) ([]glue.Tool, error) {
	if noTools {
		if strings.TrimSpace(allowCSV) != "" {
			return nil, errors.New("--tools and --no-tools are mutually exclusive")
		}
		return nil, nil
	}
	allowCSV = strings.TrimSpace(allowCSV)
	if allowCSV == "" {
		return in, nil
	}
	want := map[string]bool{}
	for _, raw := range strings.Split(allowCSV, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		want[name] = true
	}
	if len(want) == 0 {
		return nil, nil
	}
	var out []glue.Tool
	seen := map[string]bool{}
	for _, t := range in {
		if want[t.Name] {
			out = append(out, t)
			seen[t.Name] = true
		}
	}
	// Refuse unknown names so a typo doesn't silently produce zero tools.
	var missing []string
	for name := range want {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		available := make([]string, 0, len(in))
		for _, t := range in {
			available = append(available, t.Name)
		}
		sort.Strings(available)
		return nil, fmt.Errorf("--tools: unknown tool name(s) %v (available: %v)", missing, available)
	}
	return out, nil
}

func buildCodingTools(cfg codingFlagConfig) ([]glue.Tool, toolscoding.Options, error) {
	return toolscoding.Tools(toolscoding.Options{
		Enabled:         cfg.Enabled,
		WorkDir:         cfg.WorkDir,
		AllowedBinaries: append([]string(nil), cfg.AllowedBinaries...),
		AllowOverwrite:  cfg.AllowOverwrite,
	})
}

// yoloPermission auto-approves every side-effecting tool call. Used by
// --yolo. The decision is marked RememberFor: RememberSession so a
// downstream consumer of the daemon protocol that persists remembers
// records the policy coherently (a yolo run trusts every action; that
// trust applies for the rest of the session, not forever).
type yoloPermission struct{}

func (yoloPermission) Decide(_ context.Context, _ glue.PermissionRequest) (glue.PermissionDecision, error) {
	return glue.PermissionDecision{
		Allow:       true,
		Reason:      "auto-approved by --yolo",
		RememberFor: glue.RememberSession,
	}, nil
}

type localPromptPermission struct {
	input         *bufio.Reader
	stderr        io.Writer
	sessionAllows map[string]struct{}
	targetAllows  map[string]struct{}
	foreverAllows map[string]struct{}
}

func newLocalPromptPermission(stdin io.Reader, stderr io.Writer) *localPromptPermission {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &localPromptPermission{
		input:         bufio.NewReader(stdin),
		stderr:        stderr,
		sessionAllows: map[string]struct{}{},
		targetAllows:  map[string]struct{}{},
		foreverAllows: map[string]struct{}{},
	}
}

func (p *localPromptPermission) Decide(ctx context.Context, req glue.PermissionRequest) (glue.PermissionDecision, error) {
	if p == nil {
		return glue.PermissionDecision{Allow: false, Reason: "permission denied: no local permission handler"}, nil
	}
	if _, ok := p.foreverAllows[localPermissionForeverKey(req)]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberForever}, nil
	}
	if _, ok := p.sessionAllows[localPermissionSessionKey(req)]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}, nil
	}
	if _, ok := p.targetAllows[localPermissionTargetKey(req)]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSessionTarget}, nil
	}

	select {
	case <-ctx.Done():
		return glue.PermissionDecision{}, ctx.Err()
	default:
	}
	decision, err := promptPermission(p.input, p.stderr, connectPermissionPayload{Request: req})
	if err != nil {
		return glue.PermissionDecision{}, err
	}
	out := glue.PermissionDecision{Allow: decision.Allow, Reason: decision.Reason}
	switch decision.RememberFor {
	case "session":
		out.RememberFor = glue.RememberSession
		p.sessionAllows[localPermissionSessionKey(req)] = struct{}{}
	case "session_target":
		out.RememberFor = glue.RememberSessionTarget
		p.targetAllows[localPermissionTargetKey(req)] = struct{}{}
	case "forever":
		out.RememberFor = glue.RememberForever
		p.foreverAllows[localPermissionForeverKey(req)] = struct{}{}
	}
	return out, nil
}

func localPermissionSessionKey(req glue.PermissionRequest) string {
	return req.SessionID + "\x00" + req.Tool + "\x00" + req.Action
}

func localPermissionTargetKey(req glue.PermissionRequest) string {
	return localPermissionSessionKey(req) + "\x00" + req.Target
}

func localPermissionForeverKey(req glue.PermissionRequest) string {
	return req.Tool + "\x00" + req.Action + "\x00" + req.Target
}
