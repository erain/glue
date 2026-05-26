package peggy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
)

const defaultDashboardListenAddr = "127.0.0.1:0"

type dashboardHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type dashboardOptions struct {
	ConfigPath      string
	BaseURL         string
	Token           string
	MetadataPath    string
	ListenAddr      string
	AllowNonLocal   bool
	Once            bool
	RecallQuery     string
	MemoryLimit     int
	SessionLimit    int
	RecallLimit     int
	ShutdownTimeout time.Duration
	HTTPClient      dashboardHTTPDoer
}

type dashboardDaemonConfig struct {
	BaseURL       string
	Token         string
	MetadataPath  string
	MetadataPID   int
	TokenSource   string
	BaseURLSource string
}

type dashboardStatus struct {
	OK           bool     `json:"ok"`
	Version      int      `json:"version"`
	ActiveRuns   int      `json:"active_runs"`
	ToolsCount   int      `json:"tools_count"`
	Capabilities []string `json:"capabilities"`
}

type dashboardToolCatalog struct {
	Tools []dashboardTool `json:"tools"`
}

type dashboardTool struct {
	Name                    string          `json:"name"`
	Description             string          `json:"description,omitempty"`
	Parameters              json.RawMessage `json:"parameters,omitempty"`
	RequiresPermission      bool            `json:"requires_permission"`
	PermissionAction        string          `json:"permission_action,omitempty"`
	PermissionTargetPreview string          `json:"permission_target_preview,omitempty"`
}

type dashboardSkillCatalog struct {
	Skills []daemon.SkillCatalogEntry `json:"skills"`
}

type dashboardRoleCatalog struct {
	Roles []daemon.RoleCatalogEntry `json:"roles"`
}

type dashboardPage struct {
	GeneratedAt time.Time
	Daemon      dashboardDaemonConfig
	Status      dashboardStatus
	Diagnostics daemon.DiagnosticResponse
	Tools       []dashboardTool
	Skills      []daemon.SkillCatalogEntry
	Roles       []daemon.RoleCatalogEntry
	Memories    []daemon.MemoryEntry
	RecallQuery string
	RecallHits  []daemon.RecallHit
	Sessions    []glue.SessionSummary
	RunForm     dashboardRunForm
	RunResult   dashboardRunResult
	Errors      []string
	Warnings    []string
	Limits      dashboardLimits
}

type dashboardRunForm struct {
	SessionID string
	Text      string
}

type dashboardRunResult struct {
	Submitted bool
	OK        bool
	RunID     string
	SessionID string
	Text      string
	Error     string
}

type dashboardLimits struct {
	Memory  int
	Session int
	Recall  int
}

func runDashboard(ctx context.Context, args []string, stdout, stderr io.Writer, client dashboardHTTPDoer) int {
	fs := flag.NewFlagSet("peggy dashboard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath    = fs.String("config", "", "path to settings.json for local session history")
		baseURL       = fs.String("base-url", "", "Peggy daemon base URL; defaults to metadata")
		tokenFlag     = fs.String("token", "", "Peggy daemon bearer token; defaults to metadata or GLUE_DAEMON_TOKEN")
		metadataPath  = fs.String("metadata", daemon.DefaultMetadataPath(), "Peggy daemon metadata JSON path; empty disables metadata")
		listenAddr    = fs.String("listen", defaultDashboardListenAddr, "local dashboard listen address")
		allowNonLocal = fs.Bool("allow-nonlocal", false, "allow binding dashboard to a non-loopback address")
		once          = fs.Bool("once", false, "render one dashboard HTML snapshot to stdout and exit")
		recallQuery   = fs.String("recall", "", "initial recall query to show on the dashboard")
		memoryLimit   = fs.Int("memory-limit", 10, "maximum memories to show")
		sessionLimit  = fs.Int("session-limit", 10, "maximum recent sessions to show")
		recallLimit   = fs.Int("recall-limit", 5, "maximum recall hits to show")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy dashboard - run a localhost Peggy control surface.

Usage:
  peggy dashboard [flags]
  peggy dashboard --once [flags]

Examples:
  peggy dashboard
  peggy dashboard --config ~/.config/peggy/settings.json --recall "project"
  peggy dashboard --once --base-url http://127.0.0.1:1234 --token tok

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
		fmt.Fprintln(stderr, "peggy dashboard: positional args not supported")
		return 2
	}
	if *memoryLimit < 0 || *sessionLimit < 0 || *recallLimit < 0 {
		fmt.Fprintln(stderr, "peggy dashboard: limits must be non-negative")
		return 2
	}
	if client == nil {
		client = http.DefaultClient
	}
	opts := dashboardOptions{
		ConfigPath:      *configPath,
		BaseURL:         *baseURL,
		Token:           *tokenFlag,
		MetadataPath:    *metadataPath,
		ListenAddr:      *listenAddr,
		AllowNonLocal:   *allowNonLocal,
		Once:            *once,
		RecallQuery:     *recallQuery,
		MemoryLimit:     *memoryLimit,
		SessionLimit:    *sessionLimit,
		RecallLimit:     *recallLimit,
		ShutdownTimeout: defaultDaemonShutdownTimeout,
		HTTPClient:      client,
	}
	if opts.Once {
		page := loadDashboardPage(ctx, opts)
		if err := renderDashboardHTML(stdout, page); err != nil {
			fmt.Fprintf(stderr, "peggy dashboard: %v\n", err)
			return 1
		}
		return 0
	}
	if !opts.AllowNonLocal && !isLocalListenAddr(opts.ListenAddr) {
		fmt.Fprintln(stderr, "peggy dashboard: refusing to bind non-loopback address without --allow-nonlocal")
		return 2
	}
	handler := newDashboardHandler(opts)
	if err := serveDashboard(ctx, opts.ListenAddr, opts.ShutdownTimeout, handler, stdout); err != nil {
		fmt.Fprintf(stderr, "peggy dashboard: %v\n", err)
		return 1
	}
	return 0
}

func newDashboardHandler(opts dashboardOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" && r.Method == http.MethodGet:
			reqOpts := opts
			if q := strings.TrimSpace(r.URL.Query().Get("recall")); q != "" {
				reqOpts.RecallQuery = q
			}
			page := loadDashboardPage(r.Context(), reqOpts)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := renderDashboardHTML(w, page); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		case r.URL.Path == "/run" && r.Method == http.MethodPost:
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			runForm := dashboardRunForm{
				SessionID: strings.TrimSpace(r.FormValue("session_id")),
				Text:      strings.TrimSpace(r.FormValue("prompt")),
			}
			result := runDashboardPrompt(r.Context(), opts, runForm)
			page := loadDashboardPage(r.Context(), opts)
			page.RunForm = runForm
			page.RunResult = result
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := renderDashboardHTML(w, page); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		case r.URL.Path == "/run":
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		default:
			http.NotFound(w, r)
		}
	})
}

func loadDashboardPage(ctx context.Context, opts dashboardOptions) dashboardPage {
	page := dashboardPage{
		GeneratedAt: time.Now().UTC(),
		RecallQuery: strings.TrimSpace(opts.RecallQuery),
		Limits: dashboardLimits{
			Memory:  opts.MemoryLimit,
			Session: opts.SessionLimit,
			Recall:  opts.RecallLimit,
		},
		RunForm: dashboardRunForm{SessionID: "dashboard"},
	}
	daemonCfg, err := resolveDashboardDaemonConfig(opts)
	if err != nil {
		page.Errors = append(page.Errors, err.Error())
	} else {
		page.Daemon = daemonCfg
		loadDashboardDaemonData(ctx, opts, daemonCfg, &page)
	}
	loadDashboardSessions(ctx, opts, &page)
	sort.Strings(page.Errors)
	sort.Strings(page.Warnings)
	return page
}

func resolveDashboardDaemonConfig(opts dashboardOptions) (dashboardDaemonConfig, error) {
	cfg := dashboardDaemonConfig{MetadataPath: strings.TrimSpace(opts.MetadataPath)}
	var meta daemon.Metadata
	var metadataErr error
	if cfg.MetadataPath != "" {
		meta, metadataErr = daemon.ReadMetadata(cfg.MetadataPath)
		if metadataErr == nil {
			cfg.MetadataPID = meta.PID
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if cfg.BaseURL != "" {
		cfg.BaseURLSource = "flag"
	} else if meta.BaseURL != "" {
		cfg.BaseURL = strings.TrimRight(meta.BaseURL, "/")
		cfg.BaseURLSource = "metadata"
	}
	cfg.Token = strings.TrimSpace(opts.Token)
	switch {
	case cfg.Token != "":
		cfg.TokenSource = "flag"
	case meta.Token != "":
		cfg.Token = meta.Token
		cfg.TokenSource = "metadata"
	case strings.TrimSpace(os.Getenv("GLUE_DAEMON_TOKEN")) != "":
		cfg.Token = strings.TrimSpace(os.Getenv("GLUE_DAEMON_TOKEN"))
		cfg.TokenSource = "GLUE_DAEMON_TOKEN"
	}
	if cfg.BaseURL == "" {
		if metadataErr != nil {
			return cfg, fmt.Errorf("daemon metadata unavailable: %w", metadataErr)
		}
		return cfg, errors.New("daemon base URL is not configured")
	}
	if cfg.Token == "" {
		return cfg, errors.New("daemon token is not configured")
	}
	return cfg, nil
}

func loadDashboardDaemonData(ctx context.Context, opts dashboardOptions, daemonCfg dashboardDaemonConfig, page *dashboardPage) {
	client := dashboardClient(opts.HTTPClient)
	if err := dashboardGetJSON(ctx, client, daemonCfg, "/v1/status", &page.Status); err != nil {
		page.Errors = append(page.Errors, "daemon status: "+err.Error())
		return
	}
	if err := dashboardGetJSON(ctx, client, daemonCfg, "/v1/diagnostics", &page.Diagnostics); err != nil {
		page.Warnings = append(page.Warnings, "daemon diagnostics: "+err.Error())
	}
	var tools dashboardToolCatalog
	if err := dashboardGetJSON(ctx, client, daemonCfg, "/v1/tools", &tools); err != nil {
		page.Warnings = append(page.Warnings, "daemon tools: "+err.Error())
	} else {
		page.Tools = tools.Tools
	}
	if dashboardHasCapability(page.Status, "skills") {
		var skills dashboardSkillCatalog
		if err := dashboardGetJSON(ctx, client, daemonCfg, "/v1/skills", &skills); err != nil {
			page.Warnings = append(page.Warnings, "daemon skills: "+err.Error())
		} else {
			page.Skills = skills.Skills
		}
	}
	if dashboardHasCapability(page.Status, "roles") {
		var roles dashboardRoleCatalog
		if err := dashboardGetJSON(ctx, client, daemonCfg, "/v1/roles", &roles); err != nil {
			page.Warnings = append(page.Warnings, "daemon roles: "+err.Error())
		} else {
			page.Roles = roles.Roles
		}
	}
	if dashboardHasCapability(page.Status, "memories") {
		var memories daemon.MemoryCatalogResponse
		path := "/v1/memories"
		if opts.MemoryLimit > 0 {
			path += "?limit=" + url.QueryEscape(fmt.Sprint(opts.MemoryLimit))
		}
		if err := dashboardGetJSON(ctx, client, daemonCfg, path, &memories); err != nil {
			page.Warnings = append(page.Warnings, "daemon memories: "+err.Error())
		} else {
			page.Memories = memories.Memories
		}
	}
	if page.RecallQuery != "" && dashboardHasCapability(page.Status, "recall") {
		var recall daemon.RecallResponse
		req := daemon.RecallRequest{Query: page.RecallQuery, Limit: opts.RecallLimit}
		if err := dashboardPostJSON(ctx, client, daemonCfg, "/v1/recall", req, &recall); err != nil {
			page.Warnings = append(page.Warnings, "daemon recall: "+err.Error())
		} else {
			page.RecallHits = recall.Hits
		}
	}
}

func loadDashboardSessions(ctx context.Context, opts dashboardOptions, page *dashboardPage) {
	store, missingSettings, err := openStoreForRunner(opts.ConfigPath)
	if err != nil {
		page.Warnings = append(page.Warnings, "sessions: "+err.Error())
		return
	}
	if missingSettings {
		page.Warnings = append(page.Warnings, "sessions: no settings.json found; using built-in defaults")
	}
	if closer, ok := store.(io.Closer); ok {
		defer closer.Close()
	}
	lister, ok := store.(glue.SessionLister)
	if !ok {
		page.Warnings = append(page.Warnings, "sessions: configured store does not support session listing")
		return
	}
	sessions, err := lister.ListSessions(ctx, glue.ListSessionsOptions{Limit: opts.SessionLimit})
	if err != nil {
		page.Warnings = append(page.Warnings, "sessions: "+err.Error())
		return
	}
	page.Sessions = sessions
}

func runDashboardPrompt(ctx context.Context, opts dashboardOptions, form dashboardRunForm) dashboardRunResult {
	result := dashboardRunResult{
		Submitted: true,
		SessionID: strings.TrimSpace(form.SessionID),
	}
	text := strings.TrimSpace(form.Text)
	if result.SessionID == "" {
		result.SessionID = "dashboard"
	}
	if text == "" {
		result.Error = "prompt is required"
		return result
	}
	daemonCfg, err := resolveDashboardDaemonConfig(opts)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	client := dashboardClient(opts.HTTPClient)
	start, err := dashboardStartRun(ctx, client, daemonCfg, result.SessionID, text)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.RunID = start.RunID
	result.SessionID = start.SessionID
	out, err := dashboardStreamRun(ctx, client, daemonCfg, start)
	if err != nil {
		_ = dashboardCancelRun(context.Background(), client, daemonCfg, start.RunID)
		result.Error = err.Error()
		return result
	}
	result.OK = true
	result.Text = out
	return result
}

func dashboardClient(client dashboardHTTPDoer) dashboardHTTPDoer {
	if client != nil {
		return client
	}
	return http.DefaultClient
}

type dashboardStartRunRequest struct {
	Text     string `json:"text"`
	ClientID string `json:"client_id,omitempty"`
}

type dashboardStartRunResponse struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	EventsURL string `json:"events_url"`
}

func dashboardStartRun(ctx context.Context, client dashboardHTTPDoer, cfg dashboardDaemonConfig, sessionID, text string) (dashboardStartRunResponse, error) {
	payload := dashboardStartRunRequest{
		Text:     text,
		ClientID: fmt.Sprintf("dashboard:%d", os.Getpid()),
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return dashboardStartRunResponse{}, err
	}
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/runs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+path, &body)
	if err != nil {
		return dashboardStartRunResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	var out dashboardStartRunResponse
	if err := dashboardDoJSON(client, cfg, req, &out); err != nil {
		return dashboardStartRunResponse{}, fmt.Errorf("start run: %w", err)
	}
	if out.RunID == "" || out.EventsURL == "" {
		return dashboardStartRunResponse{}, errors.New("start run: missing run id or events URL")
	}
	return out, nil
}

func dashboardStreamRun(ctx context.Context, client dashboardHTTPDoer, cfg dashboardDaemonConfig, start dashboardStartRunResponse) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+start.EventsURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("event stream: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var text strings.Builder
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
			return "", err
		}
		switch event.Type {
		case string(glue.EventTextDelta):
			if delta := dashboardPayloadString(event.Payload, "delta"); delta != "" {
				text.WriteString(delta)
			}
		case "permission_request":
			return "", errors.New("run requested tool permission; use glue connect for permission-gated runs")
		case "run_done":
			if text.Len() > 0 {
				return text.String(), nil
			}
			done, err := dashboardDecodePayload[dashboardRunDonePayload](event.Payload)
			if err != nil {
				return "", err
			}
			return done.Text, nil
		case "run_error":
			if msg := dashboardPayloadErrorMessage(event.Payload); msg != "" {
				return "", errors.New(msg)
			}
			return "", errors.New("daemon run failed")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("event stream closed before terminal event")
}

type dashboardRunDonePayload struct {
	Text string `json:"text"`
}

func dashboardCancelRun(ctx context.Context, client dashboardHTTPDoer, cfg dashboardDaemonConfig, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
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
	return nil
}

func dashboardDecodePayload[T any](payload any) (T, error) {
	var out T
	data, err := json.Marshal(payload)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(data, &out)
	return out, err
}

func dashboardPayloadString(payload any, key string) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	value, _ := m[key].(string)
	return value
}

func dashboardPayloadErrorMessage(payload any) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	errValue, ok := m["error"].(map[string]any)
	if !ok {
		return ""
	}
	msg, _ := errValue["message"].(string)
	return strings.TrimSpace(msg)
}

func dashboardGetJSON(ctx context.Context, client dashboardHTTPDoer, cfg dashboardDaemonConfig, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+path, nil)
	if err != nil {
		return err
	}
	return dashboardDoJSON(client, cfg, req, out)
}

func dashboardPostJSON(ctx context.Context, client dashboardHTTPDoer, cfg dashboardDaemonConfig, path string, payload any, out any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return dashboardDoJSON(client, cfg, req, out)
}

func dashboardDoJSON(client dashboardHTTPDoer, cfg dashboardDaemonConfig, req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("%s: %s", resp.Status, msg)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}
	return nil
}

func dashboardHasCapability(status dashboardStatus, capability string) bool {
	for _, got := range status.Capabilities {
		if got == capability {
			return true
		}
	}
	return false
}

func serveDashboard(ctx context.Context, listenAddr string, shutdownTimeout time.Duration, handler http.Handler, stdout io.Writer) error {
	if listenAddr == "" {
		listenAddr = defaultDashboardListenAddr
	}
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultDaemonShutdownTimeout
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
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
	if stdout != nil {
		fmt.Fprintln(stdout, "Peggy dashboard listening")
		fmt.Fprintf(stdout, "url: http://%s\n", ln.Addr().String())
	}
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return <-errCh
	}
}

func isLocalListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func renderDashboardHTML(w io.Writer, page dashboardPage) error {
	return dashboardTemplate.Execute(w, page)
}

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"count":         dashboardCount,
	"formatTime":    dashboardFormatTime,
	"join":          strings.Join,
	"shortText":     dashboardShortText,
	"compactJSON":   dashboardCompactJSON,
	"capability":    dashboardHasCapability,
	"formatRuntime": dashboardFormatRuntime,
}).Parse(dashboardHTML))

func dashboardCount(n int) string {
	if n == 0 {
		return "0"
	}
	return fmt.Sprint(n)
}

func dashboardFormatTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

func dashboardShortText(s string) string {
	return singleLine(s)
}

func dashboardCompactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return compactJSONLine(raw)
}

func dashboardFormatRuntime(runtime daemon.DiagnosticInfo) string {
	parts := []string{}
	if runtime.Provider != "" {
		parts = append(parts, runtime.Provider)
	}
	if runtime.Model != "" {
		parts = append(parts, runtime.Model)
	}
	if runtime.StoreType != "" {
		store := runtime.StoreType
		if runtime.StorePath != "" {
			store += " " + runtime.StorePath
		}
		parts = append(parts, store)
	}
	if len(parts) == 0 {
		return "not reported"
	}
	return strings.Join(parts, " / ")
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Peggy Dashboard</title>
<style>
:root {
  color-scheme: light;
  --ink: #1d2433;
  --muted: #657084;
  --line: #d9dee8;
  --bg: #f6f7f9;
  --panel: #ffffff;
  --accent: #0f766e;
  --accent-2: #92400e;
  --danger: #b42318;
  --ok: #16803c;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  color: var(--ink);
  background: var(--bg);
  letter-spacing: 0;
}
header {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 24px;
  padding: 28px 32px 20px;
  background: #ffffff;
  border-bottom: 1px solid var(--line);
}
h1, h2, h3, p { margin: 0; }
h1 { font-size: 30px; line-height: 1.1; font-weight: 700; }
h2 { font-size: 18px; margin-bottom: 14px; }
h3 { font-size: 14px; margin-bottom: 6px; }
.eyebrow {
  color: var(--muted);
  font-size: 13px;
  margin-bottom: 6px;
}
.status-pill {
  border: 1px solid var(--line);
  background: #f9fafb;
  padding: 8px 12px;
  font-size: 14px;
  min-width: 150px;
  text-align: center;
}
.status-pill.ok { border-color: #a7d9b7; color: var(--ok); background: #f1faf4; }
.status-pill.fail { border-color: #f2b8b5; color: var(--danger); background: #fff5f5; }
nav {
  display: flex;
  gap: 14px;
  padding: 10px 32px;
  background: #eef1f5;
  border-bottom: 1px solid var(--line);
  overflow-x: auto;
  white-space: nowrap;
}
nav a { color: #263244; text-decoration: none; font-size: 14px; }
main { padding: 24px 32px 48px; max-width: 1280px; margin: 0 auto; }
.band {
  padding: 22px 0;
  border-bottom: 1px solid var(--line);
}
.summary-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 12px;
}
.metric {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 6px;
  padding: 14px;
  min-height: 92px;
}
.metric .label { color: var(--muted); font-size: 12px; text-transform: uppercase; }
.metric .value { font-size: 24px; margin-top: 8px; font-weight: 700; }
.muted { color: var(--muted); }
.split {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  gap: 18px;
}
.list {
  display: grid;
  gap: 10px;
}
.item {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 6px;
  padding: 12px;
}
.item-title {
  font-weight: 650;
  overflow-wrap: anywhere;
}
.item-meta {
  margin-top: 5px;
  color: var(--muted);
  font-size: 13px;
  overflow-wrap: anywhere;
}
.alert {
  border-left: 4px solid var(--danger);
  background: #fff7f6;
  padding: 12px 14px;
  margin-bottom: 12px;
}
.warning {
  border-left-color: var(--accent-2);
  background: #fff8ed;
}
form {
  display: flex;
  gap: 8px;
  max-width: 760px;
  margin-bottom: 14px;
}
input[type="search"] {
  flex: 1;
  min-width: 0;
  border: 1px solid var(--line);
  border-radius: 6px;
  padding: 10px 12px;
  font: inherit;
}
textarea, input[type="text"] {
  width: 100%;
  border: 1px solid var(--line);
  border-radius: 6px;
  padding: 10px 12px;
  font: inherit;
}
textarea {
  min-height: 112px;
  resize: vertical;
}
button {
  border: 1px solid #0b5f59;
  background: var(--accent);
  color: #ffffff;
  border-radius: 6px;
  padding: 10px 14px;
  font: inherit;
  cursor: pointer;
}
code {
  background: #edf0f4;
  padding: 1px 4px;
  border-radius: 4px;
}
footer { padding-top: 18px; color: var(--muted); font-size: 13px; }
@media (max-width: 680px) {
  header { display: block; padding: 22px 18px 16px; }
  .status-pill { margin-top: 16px; text-align: left; }
  nav { padding: 10px 18px; }
  main { padding: 18px; }
  form { display: block; }
  input[type="search"], button { width: 100%; }
  button { margin-top: 8px; }
}
</style>
</head>
<body>
<header>
  <div>
    <p class="eyebrow">Local control surface</p>
    <h1>Peggy Dashboard</h1>
    <p class="muted">Generated {{ formatTime .GeneratedAt }}</p>
  </div>
  <div class="status-pill {{ if .Status.OK }}ok{{ else }}fail{{ end }}">{{ if .Status.OK }}Daemon online{{ else }}Daemon offline{{ end }}</div>
</header>
<nav>
  <a href="#health">Health</a>
  <a href="#run">Run</a>
  <a href="#recall">Recall</a>
  <a href="#skills">Skills</a>
  <a href="#roles">Roles</a>
  <a href="#memories">Memories</a>
  <a href="#sessions">Sessions</a>
  <a href="#tools">Tools</a>
</nav>
<main>
  {{ range .Errors }}<div class="alert">{{ . }}</div>{{ end }}
  {{ range .Warnings }}<div class="alert warning">{{ . }}</div>{{ end }}

  <section id="health" class="band">
    <h2>Daemon Health</h2>
    <div class="summary-grid">
      <div class="metric"><div class="label">Active runs</div><div class="value">{{ count .Status.ActiveRuns }}</div></div>
      <div class="metric"><div class="label">Tools</div><div class="value">{{ count .Status.ToolsCount }}</div></div>
      <div class="metric"><div class="label">Version</div><div class="value">{{ count .Status.Version }}</div></div>
      <div class="metric"><div class="label">Runtime</div><div class="item-meta">{{ formatRuntime .Diagnostics.Runtime }}</div></div>
    </div>
    <p class="item-meta">Base URL: {{ if .Daemon.BaseURL }}<code>{{ .Daemon.BaseURL }}</code>{{ else }}not configured{{ end }} · metadata: {{ if .Daemon.MetadataPath }}<code>{{ .Daemon.MetadataPath }}</code>{{ else }}disabled{{ end }}{{ if .Daemon.MetadataPID }} · pid {{ .Daemon.MetadataPID }}{{ end }}</p>
    {{ if .Status.Capabilities }}<p class="item-meta">Capabilities: {{ join .Status.Capabilities ", " }}</p>{{ end }}
    {{ if .Diagnostics.RecentErrors }}
      <div class="list" style="margin-top:12px">
      {{ range .Diagnostics.RecentErrors }}
        <div class="item"><div class="item-title">{{ .Error }}</div><div class="item-meta">{{ formatTime .Time }} {{ .SessionID }} {{ .ClientID }}</div></div>
      {{ end }}
      </div>
    {{ end }}
  </section>

  <section id="run" class="band">
    <h2>Run Prompt</h2>
    <form method="post" action="/run" style="display:block">
      <label class="item-meta" for="session_id">Session</label>
      <input type="text" id="session_id" name="session_id" value="{{ .RunForm.SessionID }}" placeholder="dashboard">
      <label class="item-meta" for="prompt" style="display:block;margin-top:12px">Prompt</label>
      <textarea id="prompt" name="prompt" placeholder="Ask Peggy to do something">{{ .RunForm.Text }}</textarea>
      <button type="submit" style="margin-top:10px">Run</button>
    </form>
    {{ if .RunResult.Submitted }}
      {{ if .RunResult.OK }}
        <div class="item">
          <div class="item-title">Run {{ .RunResult.RunID }} · {{ .RunResult.SessionID }}</div>
          <p style="margin-top:8px;white-space:pre-wrap">{{ .RunResult.Text }}</p>
        </div>
      {{ else }}
        <div class="alert">Run failed: {{ .RunResult.Error }}</div>
      {{ end }}
    {{ end }}
  </section>

  <section id="recall" class="band">
    <h2>Recall</h2>
    <form method="get" action="/">
      <input type="search" name="recall" value="{{ .RecallQuery }}" placeholder="Search prior sessions or curated memories">
      <button type="submit">Search</button>
    </form>
    {{ if .RecallQuery }}
      <div class="list">
      {{ if .RecallHits }}
        {{ range .RecallHits }}
          <div class="item">
            <div class="item-title">{{ .SessionID }}[{{ .Index }}] {{ .Role }}</div>
            <div class="item-meta">{{ formatTime .Timestamp }}</div>
            <p style="margin-top:8px">{{ .Snippet }}</p>
          </div>
        {{ end }}
      {{ else }}
        <p class="muted">No recall hits.</p>
      {{ end }}
      </div>
    {{ else }}
      <p class="muted">Run a search to inspect prior sessions and curated memories.</p>
    {{ end }}
  </section>

  <div class="split">
    <section id="skills" class="band">
      <h2>Skills</h2>
      <div class="list">
      {{ if .Skills }}{{ range .Skills }}
        <div class="item"><div class="item-title">{{ .Name }}</div>{{ if .Description }}<div class="item-meta">{{ shortText .Description }}</div>{{ end }}</div>
      {{ end }}{{ else }}<p class="muted">No skills advertised.</p>{{ end }}
      </div>
    </section>

    <section id="roles" class="band">
      <h2>Roles</h2>
      <div class="list">
      {{ if .Roles }}{{ range .Roles }}
        <div class="item"><div class="item-title">{{ .Name }}</div><div class="item-meta">{{ if .Model }}{{ .Model }} · {{ end }}{{ shortText .Description }}</div></div>
      {{ end }}{{ else }}<p class="muted">No roles advertised.</p>{{ end }}
      </div>
    </section>
  </div>

  <div class="split">
    <section id="memories" class="band">
      <h2>Memories</h2>
      <div class="list">
      {{ if .Memories }}{{ range .Memories }}
        <div class="item">
          <div class="item-title">{{ .ID }}</div>
          <p style="margin-top:8px">{{ .Content }}</p>
          <div class="item-meta">{{ formatTime .Timestamp }}{{ if .Tags }} · {{ join .Tags ", " }}{{ end }}</div>
        </div>
      {{ end }}{{ else }}<p class="muted">No memories reported.</p>{{ end }}
      </div>
    </section>

    <section id="sessions" class="band">
      <h2>Recent Sessions</h2>
      <div class="list">
      {{ if .Sessions }}{{ range .Sessions }}
        <div class="item">
          <div class="item-title">{{ .ID }}</div>
          <div class="item-meta">updated {{ formatTime .UpdatedAt }} · messages {{ .Messages }} · user {{ .UserMessages }} · assistant {{ .AssistantMessages }}</div>
        </div>
      {{ end }}{{ else }}<p class="muted">No sessions found.</p>{{ end }}
      </div>
    </section>
  </div>

  <section id="tools" class="band">
    <h2>Tools</h2>
    <div class="list">
    {{ if .Tools }}{{ range .Tools }}
      <div class="item">
        <div class="item-title">{{ .Name }}</div>
        {{ if .Description }}<div class="item-meta">{{ shortText .Description }}</div>{{ end }}
        {{ if .RequiresPermission }}<div class="item-meta">permission: {{ .PermissionAction }} {{ .PermissionTargetPreview }}</div>{{ end }}
        {{ if .Parameters }}<div class="item-meta">parameters: <code>{{ compactJSON .Parameters }}</code></div>{{ end }}
      </div>
    {{ end }}{{ else }}<p class="muted">No tools reported.</p>{{ end }}
    </div>
  </section>

  <footer>
    Bound to localhost by default. The dashboard does not expose the daemon bearer token in HTML; it uses the token server-side from metadata, flag, or <code>GLUE_DAEMON_TOKEN</code>.
  </footer>
</main>
</body>
</html>
`
