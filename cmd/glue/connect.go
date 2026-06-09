// The "glue connect" subcommand: the daemon client that starts runs,
// streams SSE events, and brokers permission prompts.

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
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	// Register the shipped providers so they resolve through the
	// providers registry by name (--provider). Importing for side
)

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
	usagePricing := registerUsagePricingFlags(flags)
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
	recallQuery := flags.String("recall", "", "search daemon recall history and exit without starting a run")
	recallJSON := flags.Bool("recall-json", false, "print --recall output as JSON")
	recallMemories := flags.Bool("recall-memories", false, "restrict --recall to curated memories")
	recallLimit := flags.Int("recall-limit", 0, "maximum recall hits; 0 uses daemon default")
	showMemories := flags.Bool("memories", false, "list daemon memories and exit without starting a run")
	memoriesJSON := flags.Bool("memories-json", false, "print --memories output as JSON")
	memoryLimit := flags.Int("memory-limit", 0, "maximum memories to return; 0 means no limit")
	forgetMemoryID := flags.String("forget-memory", "", "delete one daemon memory by id and exit without starting a run")
	forgetMemoryJSON := flags.Bool("forget-memory-json", false, "print --forget-memory output as JSON")
	showPermissions := flags.Bool("permissions", false, "list daemon permission grants and exit without starting a run")
	permissionsJSON := flags.Bool("permissions-json", false, "print --permissions output as JSON")
	forgetPermissionID := flags.String("forget-permission", "", "delete one daemon permission grant by id and exit without starting a run")
	forgetPermissionJSON := flags.Bool("forget-permission-json", false, "print --forget-permission output as JSON")
	showStatus := flags.Bool("status", false, "show daemon status and exit without starting a run")
	statusJSON := flags.Bool("status-json", false, "print --status output as JSON")
	showDiagnose := flags.Bool("diagnose", false, "diagnose local daemon connectivity and runtime state without starting a run")
	diagnoseJSON := flags.Bool("diagnose-json", false, "print --diagnose output as JSON")
	showInspect := flags.Bool("inspect", false, "show daemon status and tools and exit without starting a run")
	inspectJSON := flags.Bool("inspect-json", false, "print --inspect output as JSON")
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")
	var runArgs repeatedStrings
	flags.Var(&runArgs, "arg", "skill or MCP prompt argument key=value; repeatable")

	if err := flags.Parse(args); err != nil {
		return err
	}
	markUsagePricingFlagState(flags, usagePricing)
	if err := validateUsagePricing(*usagePricing); err != nil {
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
	if *diagnoseJSON {
		*showDiagnose = true
	}
	if *inspectJSON {
		*showInspect = true
	}
	if *recallJSON && strings.TrimSpace(*recallQuery) == "" && flags.NArg() == 1 {
		*recallQuery = flags.Arg(0)
	}
	if *memoriesJSON {
		*showMemories = true
	}
	if *permissionsJSON {
		*showPermissions = true
	}
	showRecall := strings.TrimSpace(*recallQuery) != "" || *recallJSON
	showForgetMemory := strings.TrimSpace(*forgetMemoryID) != "" || *forgetMemoryJSON
	showForgetPermission := strings.TrimSpace(*forgetPermissionID) != "" || *forgetPermissionJSON
	inspectModes := 0
	for _, enabled := range []bool{*showTools, *showSkills, *showRoles, *showMCPResources, *showMCPPrompts, *showMCPRead, *showMCPPrompt, showRecall, *showMemories, showForgetMemory, *showPermissions, showForgetPermission, *showStatus, *showDiagnose, *showInspect} {
		if enabled {
			inspectModes++
		}
	}
	if inspectModes > 1 {
		return errors.New("choose only one of --tools, --skills, --roles, --mcp-resources, --mcp-prompts, --mcp-read, --mcp-prompt, --recall, --memories, --forget-memory, --permissions, --forget-permission, --status, --diagnose, or --inspect")
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
	rawCfg := connectConfig{
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
	}
	if *showDiagnose {
		return runConnectDiagnose(ctx, rawCfg, *diagnoseJSON, stdout, client)
	}
	cfg, err := resolveConnectConfig(rawCfg)
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
	if showRecall {
		return runConnectRecall(ctx, cfg, *recallQuery, *recallMemories, *recallLimit, *recallJSON, stdout, client)
	}
	if *showMemories {
		return runConnectMemories(ctx, cfg, *memoryLimit, *memoriesJSON, stdout, client)
	}
	if showForgetMemory {
		return runConnectForgetMemory(ctx, cfg, *forgetMemoryID, *forgetMemoryJSON, stdout, client)
	}
	if *showPermissions {
		return runConnectPermissions(ctx, cfg, *permissionsJSON, stdout, client)
	}
	if showForgetPermission {
		return runConnectForgetPermission(ctx, cfg, *forgetPermissionID, *forgetPermissionJSON, stdout, client)
	}
	if *showStatus {
		return runConnectStatus(ctx, cfg, *statusJSON, stdout, client)
	}
	if *showInspect {
		return runConnectInspect(ctx, cfg, *memoryLimit, *inspectJSON, stdout, client)
	}
	return runConnect(ctx, cfg, *showUsage, *usagePricing, stdin, stdout, stderr, client)
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

func runConnect(ctx context.Context, cfg connectConfig, showUsage bool, pricing usagePricing, stdin io.Reader, stdout io.Writer, stderr io.Writer, client httpDoer) error {
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
	return streamDaemonRun(ctx, cfg, start, showUsage, pricing, bufio.NewReader(stdin), stdout, stderr, client)
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

func streamDaemonRun(ctx context.Context, cfg connectConfig, start startRunResult, showUsage bool, pricing usagePricing, input *bufio.Reader, stdout io.Writer, stderr io.Writer, client httpDoer) error {
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
				writeUsageSummary(stderr, summarizeUsage(messages), pricing)
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
		return connectPermissionDecision{Allow: false, Reason: "permission denied by user"}, nil
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
