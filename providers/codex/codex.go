package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/erain/glue/loop"
	"github.com/erain/glue/providers"
	"github.com/erain/glue/providers/codex/auth"
)

// DefaultModel is the registry-level default model id for the codex
// provider. Callers typically override per-request via
// glue.WithModel.
const DefaultModel = "gpt-5-codex"

// DefaultBaseURL is the Codex Responses endpoint root used when
// Options.BaseURL is empty.
const DefaultBaseURL = "https://chatgpt.com/backend-api/codex"

const (
	providerName       = "codex"
	defaultOriginator  = "codex_cli_rs"
	defaultOpenAIBeta  = "responses=experimental"
	defaultContentType = "application/json"
	streamAccept       = "text/event-stream"
)

func init() {
	providers.Register(providerName, providers.Factory{
		New:          func() loop.Provider { return New(Options{}) },
		DefaultModel: DefaultModel,
		// No env key: subscription auth lives in auth.json, not an env
		// var. providers.KeyAvailable("codex") will always report false
		// — agents should probe auth.LoadTokens instead.
		Capabilities: providers.Capabilities{
			// gpt-5-codex: 400k window, frontier model, terse steering.
			ContextWindow: 400_000,
			ParallelTools: true,
			PromptVariant: "terse",
		},
	})
}

// Options configures the codex provider. All fields are optional.
type Options struct {
	// Model is the registry-level default model when ProviderRequest.Model
	// is empty. Empty falls back to DefaultModel.
	Model string

	// BaseURL overrides the Codex Responses root. Empty falls back to
	// DefaultBaseURL. Tests inject an httptest URL here.
	BaseURL string

	// HTTPClient is used for the Responses POST. A cookie jar scoped
	// to the chatgpt.com host is installed automatically when this is
	// nil; supply your own client to take ownership.
	HTTPClient *http.Client

	// OriginatorOverride replaces the "originator" header (default
	// "codex_cli_rs"). Server-side allowlist may reject other values.
	OriginatorOverride string

	// Auth is the token manager. When nil a default auth.Manager is
	// constructed; AuthFile is applied to it when non-empty.
	Auth *auth.Manager

	// AuthFile sets Auth.PathOverride. Convenient for tests that don't
	// want to construct a Manager directly.
	AuthFile string
}

// Provider implements loop.Provider against the Codex Responses
// endpoint. Construct via New.
type Provider struct {
	model      string
	baseURL    string
	httpClient *http.Client
	originator string
	auth       *auth.Manager
	userAgent  string
	version    string
}

// New constructs a Provider. Required fields are validated lazily in
// Stream so that the constructor never returns an error.
func New(o Options) *Provider {
	httpClient := o.HTTPClient
	if httpClient == nil {
		jar, _ := cookiejar.New(nil)
		httpClient = &http.Client{
			Timeout: 5 * time.Minute,
			Jar:     jar,
		}
	}
	authMgr := o.Auth
	if authMgr == nil {
		authMgr = auth.NewManager()
	}
	if o.AuthFile != "" {
		authMgr.PathOverride = o.AuthFile
	}
	originator := o.OriginatorOverride
	if originator == "" {
		originator = defaultOriginator
	}
	baseURL := o.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	ver := readVersion()
	return &Provider{
		model:      o.Model,
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
		originator: originator,
		auth:       authMgr,
		userAgent:  fmt.Sprintf("glue-codex/%s (%s %s) codex-compat", ver, runtime.GOOS, runtime.GOARCH),
		version:    ver,
	}
}

// readVersion returns the module version from build info, falling back
// to "dev" when running outside a build (e.g. tests).
func readVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
	}
	return "dev"
}

// Stream implements loop.Provider. See ADR-0006 §3 for the request and
// SSE shapes.
func (p *Provider) Stream(ctx context.Context, req loop.ProviderRequest) (<-chan loop.ProviderEvent, error) {
	if p == nil {
		return nil, errors.New("codex: nil provider")
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		model = DefaultModel
	}

	tokens, err := p.auth.EnsureFresh(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("codex: auth: %w", err)
	}
	if tokens.AccountID == "" {
		return nil, errors.New("codex: auth.json missing ChatGPT account_id (re-run `codex login`)")
	}

	input, err := messagesToInput(req.Messages)
	if err != nil {
		return nil, err
	}
	body := responsesRequest{
		Model:             model,
		Instructions:      req.SystemPrompt,
		Input:             input,
		Tools:             toolSpecsToTools(req.Tools),
		ToolChoice:        "auto",
		ParallelToolCalls: false,
		Stream:            true,
		Store:             false,
		Include:           []string{"reasoning.encrypted_content"},
	}
	payload, err := json.Marshal(&body)
	if err != nil {
		return nil, fmt.Errorf("codex: marshal request: %w", err)
	}

	sessionID := newUUID()
	conversationID := newUUID()

	httpResp, err := p.postWithRetry(ctx, tokens, payload, sessionID, conversationID)
	if err != nil {
		return nil, err
	}

	events := make(chan loop.ProviderEvent)
	go p.consumeStream(ctx, httpResp, model, events)
	return events, nil
}

// postWithRetry issues the Responses POST and refreshes-and-retries
// once on 401, per ADR-0006 §3.
func (p *Provider) postWithRetry(ctx context.Context, tokens *auth.Tokens, payload []byte, sessionID, conversationID string) (*http.Response, error) {
	resp, err := p.doPost(ctx, tokens, payload, sessionID, conversationID)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	// 401: try refresh + one retry.
	_ = drainAndClose(resp)
	if tokens.RefreshToken == "" {
		return nil, errors.New("codex: http 401 and no refresh_token available")
	}
	refreshed, err := p.auth.EnsureFresh(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("codex: 401 refresh: %w", err)
	}
	resp2, err := p.doPost(ctx, refreshed, payload, sessionID, conversationID)
	if err != nil {
		return nil, err
	}
	if resp2.StatusCode == http.StatusUnauthorized {
		_ = drainAndClose(resp2)
		return nil, errors.New("codex: http 401 after refresh (re-run `codex login`)")
	}
	return resp2, nil
}

func (p *Provider) doPost(ctx context.Context, tokens *auth.Tokens, payload []byte, sessionID, conversationID string) (*http.Response, error) {
	endpoint := p.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("codex: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	httpReq.Header.Set("ChatGPT-Account-ID", tokens.AccountID)
	httpReq.Header.Set("OpenAI-Beta", defaultOpenAIBeta)
	httpReq.Header.Set("Content-Type", defaultContentType)
	httpReq.Header.Set("Accept", streamAccept)
	httpReq.Header.Set("originator", p.originator)
	httpReq.Header.Set("User-Agent", p.userAgent)
	httpReq.Header.Set("version", p.version)
	httpReq.Header.Set("session_id", sessionID)
	httpReq.Header.Set("conversation_id", conversationID)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("codex: send request: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Let caller decide whether to refresh.
		return resp, nil
	}
	// Non-401 non-2xx: surface as error.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return nil, fmt.Errorf("codex: http %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
}

func drainAndClose(resp *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.Body.Close()
}

// consumeStream reads the SSE event stream, maps each event to a
// ProviderEvent, and emits Done with the final accumulated message
// when response.completed arrives.
func (p *Provider) consumeStream(ctx context.Context, resp *http.Response, model string, events chan<- loop.ProviderEvent) {
	defer close(events)
	defer resp.Body.Close()

	out := loop.Message{
		Role:      loop.MessageRoleAssistant,
		Provider:  providerName,
		Model:     model,
		CreatedAt: time.Now().UTC(),
		Metadata:  map[string]any{},
	}
	if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventStart, Message: &out}) {
		return
	}

	var (
		text      strings.Builder
		hasText   bool
		toolCalls []loop.ToolCall
		completed bool
		failed    bool
	)

	frames, errCh := readSSEEvents(ctx, resp.Body)
	for frame := range frames {
		switch frame.Event {
		case "response.created":
			var payload respCreatedPayload
			if err := json.Unmarshal([]byte(frame.Data), &payload); err == nil {
				if payload.Response.ID != "" {
					out.Metadata["response_id"] = payload.Response.ID
				}
				if payload.Response.Model != "" {
					out.Model = payload.Response.Model
				}
			}
		case "response.output_text.delta":
			var payload respTextDeltaPayload
			if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
				continue
			}
			if payload.Delta == "" {
				continue
			}
			text.WriteString(payload.Delta)
			hasText = true
			if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventTextDelta, Delta: payload.Delta}) {
				return
			}
		case "response.output_item.done":
			var payload respOutputItemDonePayload
			if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
				continue
			}
			if payload.Item.Type != "function_call" {
				continue
			}
			tc := loop.ToolCall{
				ID:        payload.Item.CallID,
				Name:      payload.Item.Name,
				Arguments: payload.Item.Arguments,
			}
			if tc.ID == "" {
				tc.ID = payload.Item.ID
			}
			toolCalls = append(toolCalls, tc)
			if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventToolCall, ToolCall: &tc}) {
				return
			}
		case "response.completed":
			var payload respCompletedPayload
			if err := json.Unmarshal([]byte(frame.Data), &payload); err == nil {
				if payload.Response.ID != "" {
					out.Metadata["response_id"] = payload.Response.ID
				}
				if payload.Response.Model != "" {
					out.Model = payload.Response.Model
				}
				if payload.Response.Usage != nil {
					out.Usage = &loop.Usage{
						InputTokens:     payload.Response.Usage.InputTokens,
						OutputTokens:    payload.Response.Usage.OutputTokens,
						TotalTokens:     payload.Response.Usage.TotalTokens,
						CacheReadTokens: payload.Response.Usage.CachedInputTokens,
					}
				}
			}
			completed = true
		case "response.failed":
			var payload respFailedPayload
			_ = json.Unmarshal([]byte(frame.Data), &payload)
			msg := "codex: response.failed"
			if payload.Response.Error != nil && payload.Response.Error.Message != "" {
				msg = "codex: " + payload.Response.Error.Message
			}
			send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: msg})
			failed = true
		default:
			// Ignored event type (reasoning, queue, etc.). No-op.
		}
	}

	if err := <-errCh; err != nil {
		send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: fmt.Sprintf("codex: %v", err)})
		return
	}
	if failed {
		return
	}
	if !completed {
		send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: "codex: stream closed before response.completed"})
		return
	}

	// Assemble final message content.
	if hasText && text.Len() > 0 {
		out.Content = append(out.Content, loop.ContentPart{Type: loop.ContentTypeText, Text: text.String()})
	}
	for i := range toolCalls {
		tc := toolCalls[i]
		out.Content = append(out.Content, loop.ContentPart{Type: loop.ContentTypeToolCall, ToolCall: &tc})
	}
	if len(toolCalls) > 0 {
		out.StopReason = loop.StopReasonToolUse
	} else {
		out.StopReason = loop.StopReasonStop
	}
	if len(out.Metadata) == 0 {
		out.Metadata = nil
	}
	send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventDone, Message: &out})
}

func send(ctx context.Context, ch chan<- loop.ProviderEvent, ev loop.ProviderEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case ch <- ev:
		return true
	}
}

// newUUID returns an RFC-4122 v4 UUID using crypto/rand. No third-party
// dependency.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Compile-time assertion that Provider satisfies loop.Provider.
var _ loop.Provider = (*Provider)(nil)

// Ensure cookiejar import is referenced even when callers supply their
// own HTTPClient (some builds tree-shake unused symbols).
var _ = cookiejar.New
var _ = url.Parse
