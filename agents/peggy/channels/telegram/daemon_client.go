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
	Text     string `json:"text"`
	ClientID string `json:"client_id,omitempty"`
}

type daemonStartRunResponse struct {
	RunID     string `json:"run_id"`
	EventsURL string `json:"events_url"`
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
	start, err := d.startRun(ctx, sessionID, text, clientID)
	if err != nil {
		return "", err
	}
	return d.streamRun(ctx, start, api, chatID, clientID)
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

func (d *DaemonClient) startRun(ctx context.Context, sessionID, text, clientID string) (daemonStartRunResponse, error) {
	payload, _ := json.Marshal(daemonStartRunPayload{Text: text, ClientID: clientID})
	endpoint := d.baseURL + "/v1/sessions/" + url.PathEscape(sessionID) + "/runs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
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
