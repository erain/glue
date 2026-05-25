package telegram

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
)

// DaemonClient talks to a Peggy daemon on behalf of the Telegram channel.
type DaemonClient struct {
	baseURL string
	token   string
	client  *http.Client
	stderr  io.Writer

	nextNonce atomic.Uint64
	mu        sync.Mutex
	pending   map[string]daemonPendingPermission
}

type daemonPendingPermission struct {
	chatID       int64
	runID        string
	permissionID string
	clientID     string
}

type daemonStartRunPayload struct {
	Text      string            `json:"text,omitempty"`
	Skill     string            `json:"skill,omitempty"`
	Arguments map[string]string `json:"arguments,omitempty"`
	Role      string            `json:"role,omitempty"`
	ClientID  string            `json:"client_id,omitempty"`
}

type daemonStartRunResponse struct {
	RunID     string `json:"run_id"`
	EventsURL string `json:"events_url"`
}

type daemonSkillCatalog struct {
	Skills []daemon.SkillCatalogEntry `json:"skills"`
}

type daemonRoleCatalog struct {
	Roles []daemon.RoleCatalogEntry `json:"roles"`
}

type daemonPermissionPayload struct {
	PermissionID string                 `json:"permission_id"`
	Request      glue.PermissionRequest `json:"request"`
	ExpiresAt    time.Time              `json:"expires_at"`
}

type daemonPermissionDecision struct {
	Allow       bool   `json:"allow"`
	Reason      string `json:"reason,omitempty"`
	RememberFor string `json:"remember_for,omitempty"`
}

const (
	defaultTelegramMemoryLimit = 10
	defaultTelegramRecallLimit = 5
)

// ResolveDaemonClientConfig applies metadata and environment fallback rules.
func ResolveDaemonClientConfig(cfg DaemonClientConfig) (DaemonClientConfig, error) {
	if strings.TrimSpace(cfg.MetadataPath) == "" {
		cfg.MetadataPath = daemon.DefaultMetadataPath()
	}
	var meta daemon.Metadata
	var metadataErr error
	if strings.TrimSpace(cfg.MetadataPath) != "" {
		loaded, err := daemon.ReadMetadata(cfg.MetadataPath)
		if err != nil {
			metadataErr = err
		} else {
			meta = loaded
		}
	}
	if strings.TrimSpace(cfg.BaseURL) == "" && meta.BaseURL != "" {
		cfg.BaseURL = meta.BaseURL
	}
	if strings.TrimSpace(cfg.Token) == "" && meta.Token != "" {
		cfg.Token = meta.Token
	}
	if strings.TrimSpace(cfg.Token) == "" {
		cfg.Token = strings.TrimSpace(os.Getenv("GLUE_DAEMON_TOKEN"))
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		if metadataErr != nil {
			return cfg, fmt.Errorf("telegram daemon: base URL not configured and metadata unavailable: %w", metadataErr)
		}
		return cfg, errors.New("telegram daemon: base URL is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		if metadataErr != nil {
			return cfg, fmt.Errorf("telegram daemon: token not configured and metadata unavailable: %w", metadataErr)
		}
		return cfg, errors.New("telegram daemon: token is required")
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.Token = strings.TrimSpace(cfg.Token)
	return cfg, nil
}

// NewDaemonClient constructs a daemon client from resolved config.
func NewDaemonClient(cfg DaemonClientConfig, client *http.Client, stderr io.Writer) (*DaemonClient, error) {
	resolved, err := ResolveDaemonClientConfig(cfg)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &DaemonClient{
		baseURL: resolved.BaseURL,
		token:   resolved.Token,
		client:  client,
		stderr:  stderr,
		pending: map[string]daemonPendingPermission{},
	}, nil
}

func (d *DaemonClient) Prompt(ctx context.Context, sessionID, text string, api *API, chatID int64) (string, error) {
	if d == nil {
		return "", errors.New("telegram daemon: client is not configured")
	}
	clientID := daemonTelegramClientID(chatID)
	start, err := d.startRun(ctx, sessionID, daemonStartRunPayload{Text: text, ClientID: clientID})
	if err != nil {
		return "", err
	}
	return d.streamRun(ctx, start, api, chatID, clientID)
}

// Command handles Telegram slash commands that map to daemon runtime actions.
func (d *DaemonClient) Command(ctx context.Context, sessionID, text string, api *API, chatID int64) (string, bool, error) {
	if d == nil {
		return "", false, errors.New("telegram daemon: client is not configured")
	}
	trimmed := strings.TrimSpace(text)
	fields := strings.Fields(trimmed)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", false, nil
	}
	token := fields[0]
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, token))
	switch telegramCommandName(token) {
	case "roles":
		if rest != "" {
			return "", true, errors.New("usage: /roles")
		}
		catalog, err := d.roleCatalog(ctx)
		if err != nil {
			return "", true, err
		}
		return formatTelegramRoles(catalog.Roles), true, nil
	case "role":
		role, prompt, err := parseTelegramRoleRun(rest)
		if err != nil {
			return "", true, err
		}
		clientID := daemonTelegramClientID(chatID)
		start, err := d.startRun(ctx, sessionID, daemonStartRunPayload{Text: prompt, Role: role, ClientID: clientID})
		if err != nil {
			return "", true, err
		}
		text, err := d.streamRun(ctx, start, api, chatID, clientID)
		return text, true, err
	case "skills":
		if rest != "" {
			return "", true, errors.New("usage: /skills")
		}
		catalog, err := d.skillCatalog(ctx)
		if err != nil {
			return "", true, err
		}
		return formatTelegramSkills(catalog.Skills), true, nil
	case "skill":
		name, args, err := parseTelegramSkillRun(rest)
		if err != nil {
			return "", true, err
		}
		clientID := daemonTelegramClientID(chatID)
		start, err := d.startRun(ctx, sessionID, daemonStartRunPayload{Skill: name, Arguments: args, ClientID: clientID})
		if err != nil {
			return "", true, err
		}
		text, err := d.streamRun(ctx, start, api, chatID, clientID)
		return text, true, err
	case "memories":
		limit, err := parseTelegramMemoryLimit(rest)
		if err != nil {
			return "", true, err
		}
		catalog, err := d.memoryCatalog(ctx, limit)
		if err != nil {
			return "", true, err
		}
		return formatTelegramMemories(catalog.Memories), true, nil
	case "recall":
		if rest == "" {
			return "", true, errors.New("/recall query is required")
		}
		recall, err := d.recall(ctx, daemon.RecallRequest{Query: rest, Limit: defaultTelegramRecallLimit})
		if err != nil {
			return "", true, err
		}
		return formatTelegramRecallHits(recall.Hits), true, nil
	case "recall_memories":
		if rest == "" {
			return "", true, errors.New("/recall_memories query is required")
		}
		recall, err := d.recall(ctx, daemon.RecallRequest{Query: rest, Limit: defaultTelegramRecallLimit, MemoriesOnly: true})
		if err != nil {
			return "", true, err
		}
		return formatTelegramRecallHits(recall.Hits), true, nil
	case "forget_memory":
		id, err := parseTelegramForgetMemoryID(rest)
		if err != nil {
			return "", true, err
		}
		forgotten, err := d.forgetMemory(ctx, id)
		if err != nil {
			return "", true, err
		}
		return formatTelegramForgottenMemory(forgotten.Memory), true, nil
	default:
		return "", false, nil
	}
}

func (d *DaemonClient) HandleCallback(ctx context.Context, cb CallbackQuery, api *API) bool {
	if d == nil || !strings.HasPrefix(cb.Data, permissionCallbackPrefix) {
		return false
	}
	nonce, action, ok := parsePermissionCallback(cb.Data)
	if !ok {
		answerDaemonCallback(ctx, api, cb.ID, "Invalid permission response.")
		return true
	}
	chatID, ok := callbackChatID(cb)
	if !ok {
		answerDaemonCallback(ctx, api, cb.ID, "Permission response has no chat.")
		return true
	}

	d.mu.Lock()
	pending, ok := d.pending[nonce]
	if ok && pending.chatID == chatID {
		delete(d.pending, nonce)
	}
	d.mu.Unlock()
	if !ok || pending.chatID != chatID {
		answerDaemonCallback(ctx, api, cb.ID, "Permission request expired.")
		return true
	}

	decision := glueDecisionToDaemon(permissionDecisionForAction(action))
	if err := d.postDecision(ctx, pending, decision); err != nil {
		d.print("telegram daemon: permission decision: %v\n", err)
		answerDaemonCallback(ctx, api, cb.ID, "Permission response failed.")
		return true
	}
	if decision.Allow {
		answerDaemonCallback(ctx, api, cb.ID, "Allowed.")
	} else {
		answerDaemonCallback(ctx, api, cb.ID, "Denied.")
	}
	return true
}

func (d *DaemonClient) roleCatalog(ctx context.Context) (daemonRoleCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/v1/roles", nil)
	if err != nil {
		return daemonRoleCatalog{}, err
	}
	d.authorize(req)
	resp, err := d.client.Do(req)
	if err != nil {
		return daemonRoleCatalog{}, redactDaemonErr(err, d.token)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonRoleCatalog{}, fmt.Errorf("telegram daemon: roles: %s", httpStatusText(resp))
	}
	var out daemonRoleCatalog
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return daemonRoleCatalog{}, err
	}
	if out.Roles == nil {
		out.Roles = []daemon.RoleCatalogEntry{}
	}
	return out, nil
}

func (d *DaemonClient) skillCatalog(ctx context.Context) (daemonSkillCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/v1/skills", nil)
	if err != nil {
		return daemonSkillCatalog{}, err
	}
	d.authorize(req)
	resp, err := d.client.Do(req)
	if err != nil {
		return daemonSkillCatalog{}, redactDaemonErr(err, d.token)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonSkillCatalog{}, fmt.Errorf("telegram daemon: skills: %s", httpStatusText(resp))
	}
	var out daemonSkillCatalog
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return daemonSkillCatalog{}, err
	}
	if out.Skills == nil {
		out.Skills = []daemon.SkillCatalogEntry{}
	}
	return out, nil
}

func (d *DaemonClient) recall(ctx context.Context, in daemon.RecallRequest) (daemon.RecallResponse, error) {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/v1/recall", bytes.NewReader(body))
	if err != nil {
		return daemon.RecallResponse{}, err
	}
	d.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return daemon.RecallResponse{}, redactDaemonErr(err, d.token)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.RecallResponse{}, fmt.Errorf("telegram daemon: recall: %s", httpStatusText(resp))
	}
	var out daemon.RecallResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return daemon.RecallResponse{}, err
	}
	if out.Hits == nil {
		out.Hits = []daemon.RecallHit{}
	}
	return out, nil
}

func (d *DaemonClient) memoryCatalog(ctx context.Context, limit int) (daemon.MemoryCatalogResponse, error) {
	endpoint := d.baseURL + "/v1/memories"
	if limit > 0 {
		values := url.Values{}
		values.Set("limit", strconv.Itoa(limit))
		endpoint += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return daemon.MemoryCatalogResponse{}, err
	}
	d.authorize(req)
	resp, err := d.client.Do(req)
	if err != nil {
		return daemon.MemoryCatalogResponse{}, redactDaemonErr(err, d.token)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.MemoryCatalogResponse{}, fmt.Errorf("telegram daemon: memories: %s", httpStatusText(resp))
	}
	var out daemon.MemoryCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return daemon.MemoryCatalogResponse{}, err
	}
	if out.Memories == nil {
		out.Memories = []daemon.MemoryEntry{}
	}
	return out, nil
}

func (d *DaemonClient) forgetMemory(ctx context.Context, id string) (daemon.MemoryForgetResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, d.baseURL+"/v1/memories/"+url.PathEscape(id), nil)
	if err != nil {
		return daemon.MemoryForgetResponse{}, err
	}
	d.authorize(req)
	resp, err := d.client.Do(req)
	if err != nil {
		return daemon.MemoryForgetResponse{}, redactDaemonErr(err, d.token)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.MemoryForgetResponse{}, fmt.Errorf("telegram daemon: forget memory: %s", httpStatusText(resp))
	}
	var out daemon.MemoryForgetResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return daemon.MemoryForgetResponse{}, err
	}
	return out, nil
}

func (d *DaemonClient) startRun(ctx context.Context, sessionID string, payload daemonStartRunPayload) (daemonStartRunResponse, error) {
	body, _ := json.Marshal(payload)
	endpoint := d.baseURL + "/v1/sessions/" + url.PathEscape(sessionID) + "/runs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return daemonStartRunResponse{}, err
	}
	d.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return daemonStartRunResponse{}, redactDaemonErr(err, d.token)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return daemonStartRunResponse{}, fmt.Errorf("telegram daemon: start run: %s", httpStatusText(resp))
	}
	var out daemonStartRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return daemonStartRunResponse{}, err
	}
	if out.RunID == "" || out.EventsURL == "" {
		return daemonStartRunResponse{}, errors.New("telegram daemon: start run response missing run id or events url")
	}
	return out, nil
}

func (d *DaemonClient) streamRun(ctx context.Context, start daemonStartRunResponse, api *API, chatID int64, clientID string) (string, error) {
	endpoint := d.baseURL + start.EventsURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	d.authorize(req)
	resp, err := d.client.Do(req)
	if err != nil {
		return "", redactDaemonErr(err, d.token)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("telegram daemon: stream run: %s", httpStatusText(resp))
	}

	var text strings.Builder
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event daemon.EventEnvelope
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			return "", err
		}
		switch event.Type {
		case "text_delta":
			text.WriteString(payloadString(event.Payload, "delta"))
		case "permission_request":
			if err := d.handlePermissionRequest(ctx, api, chatID, start.RunID, clientID, event.Payload); err != nil {
				return "", err
			}
		case "run_error":
			if msg := payloadString(event.Payload, "error"); msg != "" {
				return "", errors.New(msg)
			}
			return "", errors.New("telegram daemon: run failed")
		case "run_done":
			if text.Len() == 0 {
				text.WriteString(payloadString(event.Payload, "text"))
			}
			return text.String(), nil
		}
	}
	if err := scan.Err(); err != nil {
		return "", err
	}
	return text.String(), nil
}

func (d *DaemonClient) handlePermissionRequest(ctx context.Context, api *API, chatID int64, runID, clientID string, payload any) error {
	var perm daemonPermissionPayload
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &perm); err != nil {
		return err
	}
	if perm.PermissionID == "" {
		return errors.New("telegram daemon: permission request missing id")
	}
	nonce := strconv.FormatUint(d.nextNonce.Add(1), 36)
	d.mu.Lock()
	d.pending[nonce] = daemonPendingPermission{
		chatID:       chatID,
		runID:        runID,
		permissionID: perm.PermissionID,
		clientID:     clientID,
	}
	d.mu.Unlock()
	if err := api.SendMessageWithReplyMarkup(ctx, chatID, permissionMessage(perm.Request), permissionKeyboard(nonce)); err != nil {
		d.deletePending(nonce)
		_ = d.postDecision(ctx, daemonPendingPermission{runID: runID, permissionID: perm.PermissionID, clientID: clientID}, daemonPermissionDecision{
			Allow:  false,
			Reason: "permission denied: failed to send Telegram permission prompt",
		})
		return err
	}
	return nil
}

func (d *DaemonClient) postDecision(ctx context.Context, pending daemonPendingPermission, decision daemonPermissionDecision) error {
	path := "/v1/runs/" + url.PathEscape(pending.runID) + "/permissions/" + url.PathEscape(pending.permissionID) + "/decision"
	body, _ := json.Marshal(decision)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	d.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Glue-Client-ID", pending.clientID)
	resp, err := d.client.Do(req)
	if err != nil {
		return redactDaemonErr(err, d.token)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram daemon: permission decision: %s", httpStatusText(resp))
	}
	return nil
}

func (d *DaemonClient) deletePending(nonce string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.pending, nonce)
}

func (d *DaemonClient) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+d.token)
}

func (d *DaemonClient) print(format string, args ...any) {
	if d.stderr != nil {
		_, _ = fmt.Fprintf(d.stderr, format, args...)
	}
}

func telegramCommandName(token string) string {
	name := strings.TrimPrefix(strings.TrimSpace(token), "/")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	return strings.ToLower(name)
}

func parseTelegramMemoryLimit(rest string) (int, error) {
	if strings.TrimSpace(rest) == "" {
		return defaultTelegramMemoryLimit, nil
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 {
		return 0, errors.New("usage: /memories [limit]")
	}
	limit, err := strconv.Atoi(fields[0])
	if err != nil || limit < 0 {
		return 0, errors.New("/memories limit must be non-negative")
	}
	return limit, nil
}

func parseTelegramForgetMemoryID(rest string) (string, error) {
	fields := strings.Fields(rest)
	if len(fields) != 1 {
		return "", errors.New("usage: /forget_memory <id>")
	}
	return fields[0], nil
}

func parseTelegramSkillRun(rest string) (string, map[string]string, error) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", nil, errors.New("/skill name is required")
	}
	name := strings.TrimSpace(fields[0])
	if name == "" {
		return "", nil, errors.New("/skill name is required")
	}
	args := make(map[string]string, len(fields)-1)
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return "", nil, errors.New("usage: /skill <name> [key=value ...]")
		}
		args[key] = strings.TrimSpace(value)
	}
	if len(args) == 0 {
		args = nil
	}
	return name, args, nil
}

func parseTelegramRoleRun(rest string) (string, string, error) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", "", errors.New("/role name is required")
	}
	role := strings.TrimSpace(fields[0])
	if role == "" {
		return "", "", errors.New("/role name is required")
	}
	prompt := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), fields[0]))
	if prompt == "" {
		return "", "", errors.New("/role prompt is required")
	}
	return role, prompt, nil
}

func formatTelegramRoles(roles []daemon.RoleCatalogEntry) string {
	if len(roles) == 0 {
		return "No daemon roles reported."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Roles (%d):\n", len(roles))
	for i, role := range roles {
		fmt.Fprintf(&b, "%d. %s\n", i+1, role.Name)
		if role.Description != "" {
			fmt.Fprintf(&b, "   %s\n", telegramOneLine(role.Description, 260))
		}
		if role.Model != "" {
			fmt.Fprintf(&b, "   model: %s\n", role.Model)
		}
	}
	return strings.TrimSpace(b.String())
}

func formatTelegramSkills(skills []daemon.SkillCatalogEntry) string {
	if len(skills) == 0 {
		return "No daemon skills reported."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Skills (%d):\n", len(skills))
	for i, skill := range skills {
		fmt.Fprintf(&b, "%d. %s\n", i+1, skill.Name)
		if skill.Description != "" {
			fmt.Fprintf(&b, "   %s\n", telegramOneLine(skill.Description, 260))
		}
	}
	return strings.TrimSpace(b.String())
}

func formatTelegramMemories(memories []daemon.MemoryEntry) string {
	if len(memories) == 0 {
		return "No memories."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Memories (%d):\n", len(memories))
	for i, memory := range memories {
		fmt.Fprintf(&b, "%d. %s\n", i+1, memory.ID)
		if !memory.Timestamp.IsZero() {
			fmt.Fprintf(&b, "   %s\n", memory.Timestamp.Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "   %s\n", telegramOneLine(memory.Content, 260))
		if len(memory.Tags) > 0 {
			fmt.Fprintf(&b, "   tags: %s\n", strings.Join(memory.Tags, ", "))
		}
	}
	return strings.TrimSpace(b.String())
}

func formatTelegramRecallHits(hits []daemon.RecallHit) string {
	if len(hits) == 0 {
		return "No recall hits."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Recall hits (%d):\n", len(hits))
	for i, hit := range hits {
		sessionID := hit.SessionID
		if sessionID == "" {
			sessionID = "(unknown session)"
		}
		fmt.Fprintf(&b, "%d. %s#%d", i+1, sessionID, hit.Index)
		if hit.Role != "" {
			fmt.Fprintf(&b, " %s", hit.Role)
		}
		fmt.Fprintln(&b)
		if !hit.Timestamp.IsZero() {
			fmt.Fprintf(&b, "   %s\n", hit.Timestamp.Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "   %s\n", telegramOneLine(hit.Snippet, 260))
	}
	return strings.TrimSpace(b.String())
}

func formatTelegramForgottenMemory(memory daemon.MemoryEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Forgot %s", memory.ID)
	if memory.Content != "" {
		fmt.Fprintf(&b, ":\n%s", telegramOneLine(memory.Content, 260))
	}
	return strings.TrimSpace(b.String())
}

func telegramOneLine(text string, maxRunes int) string {
	text = strings.Join(strings.Fields(text), " ")
	if maxRunes <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	const suffix = " ... [truncated]"
	suffixRunes := []rune(suffix)
	if maxRunes <= len(suffixRunes) {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-len(suffixRunes)]) + suffix
}

func daemonTelegramClientID(chatID int64) string {
	return "telegram:" + strconv.FormatInt(chatID, 10)
}

func glueDecisionToDaemon(decision glue.PermissionDecision) daemonPermissionDecision {
	return daemonPermissionDecision{
		Allow:       decision.Allow,
		Reason:      decision.Reason,
		RememberFor: daemonRememberScope(decision.RememberFor),
	}
}

func daemonRememberScope(scope glue.RememberScope) string {
	switch scope {
	case glue.RememberSession:
		return "session"
	case glue.RememberSessionTarget:
		return "session_target"
	case glue.RememberForever:
		return "forever"
	default:
		return ""
	}
}

func payloadString(payload any, key string) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func httpStatusText(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return resp.Status
	}
	return resp.Status + ": " + msg
}

func redactDaemonErr(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, token) {
		return errors.New(strings.ReplaceAll(msg, token, "<redacted>"))
	}
	return err
}

func answerDaemonCallback(ctx context.Context, api *API, callbackID, text string) {
	if api == nil || callbackID == "" {
		return
	}
	_ = api.AnswerCallbackQuery(ctx, callbackID, text)
}
