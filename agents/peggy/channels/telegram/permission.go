package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/agents/peggy"
)

const (
	defaultPermissionTimeout = 10 * time.Minute
	permissionCallbackPrefix = "perm:"
	permissionArgsPreviewMax = 320
)

// PermissionOptions configures [NewPermission].
type PermissionOptions struct {
	// Timeout caps how long a side-effecting tool waits for a Telegram
	// button response. Zero uses a conservative default.
	Timeout time.Duration

	// Stderr receives diagnostics. Nil discards output.
	Stderr io.Writer
}

// Permission is a Telegram inline-keyboard implementation of
// glue.Permission. It is configured by [Channel] and is safe for concurrent
// use by prompt goroutines and the long-poll callback handler.
type Permission struct {
	mu sync.Mutex

	api     *API
	allowed map[int64]struct{}
	stderr  io.Writer
	timeout time.Duration

	nextNonce     atomic.Uint64
	pending       map[string]*pendingPermission
	sessionAllows map[string]struct{}
	targetAllows  map[string]struct{}
}

type pendingPermission struct {
	chatID int64
	req    glue.PermissionRequest
	done   chan glue.PermissionDecision
}

// NewPermission constructs a Telegram permission adapter. The channel attaches
// API and allowlist state during New.
func NewPermission(opts PermissionOptions) *Permission {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultPermissionTimeout
	}
	return &Permission{
		stderr:        opts.Stderr,
		timeout:       timeout,
		pending:       map[string]*pendingPermission{},
		sessionAllows: map[string]struct{}{},
		targetAllows:  map[string]struct{}{},
	}
}

func (p *Permission) attach(api *API, allowed map[int64]struct{}, stderr io.Writer) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.api = api
	p.allowed = cloneAllowedChats(allowed)
	if p.stderr == nil {
		p.stderr = stderr
	}
	if p.pending == nil {
		p.pending = map[string]*pendingPermission{}
	}
	if p.sessionAllows == nil {
		p.sessionAllows = map[string]struct{}{}
	}
	if p.targetAllows == nil {
		p.targetAllows = map[string]struct{}{}
	}
}

// Decide implements glue.Permission.
func (p *Permission) Decide(ctx context.Context, req glue.PermissionRequest) (glue.PermissionDecision, error) {
	if p == nil {
		return glue.PermissionDecision{Allow: false, Reason: "permission denied: telegram permission handler is not configured"}, nil
	}
	if err := ctx.Err(); err != nil {
		return glue.PermissionDecision{}, err
	}

	chatID, err := chatIDFromSession(req.SessionID)
	if err != nil {
		return glue.PermissionDecision{Allow: false, Reason: err.Error()}, nil
	}

	sessionKey := permissionSessionKey(req)
	targetKey := permissionTargetKey(req)
	nonce := strconv.FormatUint(p.nextNonce.Add(1), 36)
	pending := &pendingPermission{chatID: chatID, req: req, done: make(chan glue.PermissionDecision, 1)}

	p.mu.Lock()
	if _, ok := p.sessionAllows[sessionKey]; ok {
		p.mu.Unlock()
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}, nil
	}
	if _, ok := p.targetAllows[targetKey]; ok {
		p.mu.Unlock()
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSessionTarget}, nil
	}
	api := p.api
	_, allowed := p.allowed[chatID]
	p.pending[nonce] = pending
	p.mu.Unlock()

	if !allowed {
		p.deletePending(nonce)
		return glue.PermissionDecision{Allow: false, Reason: "permission denied: chat is not allowlisted"}, nil
	}
	if api == nil {
		p.deletePending(nonce)
		return glue.PermissionDecision{Allow: false, Reason: "permission denied: telegram API is not configured"}, nil
	}

	if err := api.SendMessageWithReplyMarkup(ctx, chatID, permissionMessage(req), permissionKeyboard(nonce)); err != nil {
		p.deletePending(nonce)
		p.print("telegram: send permission prompt to chat %d: %v\n", chatID, err)
		return glue.PermissionDecision{Allow: false, Reason: "permission denied: failed to send Telegram permission prompt"}, nil
	}

	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case decision := <-pending.done:
		p.rememberDecision(req, decision)
		return decision, nil
	case <-timer.C:
		p.deletePending(nonce)
		return glue.PermissionDecision{Allow: false, Reason: "permission denied: Telegram permission prompt timed out"}, nil
	case <-ctx.Done():
		p.deletePending(nonce)
		return glue.PermissionDecision{}, ctx.Err()
	}
}

func (p *Permission) handleCallback(ctx context.Context, cb CallbackQuery) bool {
	if p == nil || !strings.HasPrefix(cb.Data, permissionCallbackPrefix) {
		return false
	}
	nonce, action, ok := parsePermissionCallback(cb.Data)
	if !ok {
		p.answerCallback(ctx, cb.ID, "Invalid permission response.")
		return true
	}
	chatID, ok := callbackChatID(cb)
	if !ok {
		p.answerCallback(ctx, cb.ID, "Permission response has no chat.")
		return true
	}

	p.mu.Lock()
	_, allowed := p.allowed[chatID]
	pending := p.pending[nonce]
	if pending != nil && pending.chatID == chatID && allowed {
		delete(p.pending, nonce)
	}
	p.mu.Unlock()

	if !allowed {
		p.answerCallback(ctx, cb.ID, "This chat is not allowed.")
		return true
	}
	if pending == nil || pending.chatID != chatID {
		p.answerCallback(ctx, cb.ID, "Permission request expired.")
		return true
	}

	decision := permissionDecisionForAction(action)
	pending.done <- decision
	if decision.Allow {
		p.answerCallback(ctx, cb.ID, "Allowed.")
	} else {
		p.answerCallback(ctx, cb.ID, "Denied.")
	}
	return true
}

func (p *Permission) rememberDecision(req glue.PermissionRequest, decision glue.PermissionDecision) {
	if !decision.Allow {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch decision.RememberFor {
	case glue.RememberSession:
		p.sessionAllows[permissionSessionKey(req)] = struct{}{}
	case glue.RememberSessionTarget:
		p.targetAllows[permissionTargetKey(req)] = struct{}{}
	}
}

func (p *Permission) deletePending(nonce string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, nonce)
}

func (p *Permission) answerCallback(ctx context.Context, callbackID, text string) {
	if p == nil || callbackID == "" {
		return
	}
	p.mu.Lock()
	api := p.api
	p.mu.Unlock()
	if api == nil {
		return
	}
	if err := api.AnswerCallbackQuery(ctx, callbackID, text); err != nil {
		p.print("telegram: answerCallbackQuery: %v\n", err)
	}
}

func (p *Permission) print(format string, args ...any) {
	if p.stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(p.stderr, format, args...)
}

func permissionKeyboard(nonce string) InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{
			{Text: "Deny", CallbackData: permissionCallback(nonce, "deny")},
			{Text: "Allow once", CallbackData: permissionCallback(nonce, "once")},
		},
		{
			{Text: "Allow session", CallbackData: permissionCallback(nonce, "session")},
			{Text: "Allow target", CallbackData: permissionCallback(nonce, "target")},
		},
	}}
}

func permissionCallback(nonce, action string) string {
	return permissionCallbackPrefix + nonce + ":" + action
}

func parsePermissionCallback(data string) (nonce string, action string, ok bool) {
	rest := strings.TrimPrefix(data, permissionCallbackPrefix)
	parts := strings.Split(rest, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func permissionDecisionForAction(action string) glue.PermissionDecision {
	switch action {
	case "once":
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberNever}
	case "session":
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}
	case "target":
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSessionTarget}
	default:
		return glue.PermissionDecision{Allow: false, Reason: "permission denied by Telegram user"}
	}
}

func permissionMessage(req glue.PermissionRequest) string {
	var b strings.Builder
	b.WriteString("Peggy wants permission to run a side-effecting tool.\n\n")
	fmt.Fprintf(&b, "Tool: %s\n", req.Tool)
	fmt.Fprintf(&b, "Action: %s\n", req.Action)
	if strings.TrimSpace(req.Target) != "" {
		fmt.Fprintf(&b, "Target: %s\n", req.Target)
	}
	if preview := permissionArgsPreview(req.Args); preview != "" {
		fmt.Fprintf(&b, "Args: %s\n", preview)
	}
	return b.String()
}

func permissionArgsPreview(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return ""
	}
	if len(text) <= permissionArgsPreviewMax {
		return text
	}
	return text[:permissionArgsPreviewMax] + "...(truncated)"
}

func permissionSessionKey(req glue.PermissionRequest) string {
	return req.SessionID + "\x00" + req.Tool + "\x00" + req.Action
}

func permissionTargetKey(req glue.PermissionRequest) string {
	return permissionSessionKey(req) + "\x00" + req.Target
}

func chatIDFromSession(sessionID string) (int64, error) {
	raw := strings.TrimPrefix(sessionID, peggy.ChannelSessionID(ChannelName, ""))
	if raw == "" || raw == sessionID {
		return 0, errors.New("permission denied: request is not from a Telegram session")
	}
	chatID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errors.New("permission denied: invalid Telegram session id")
	}
	return chatID, nil
}

func callbackChatID(cb CallbackQuery) (int64, bool) {
	if cb.Message == nil {
		return 0, false
	}
	return cb.Message.Chat.ID, cb.Message.Chat.ID != 0
}

func cloneAllowedChats(in map[int64]struct{}) map[int64]struct{} {
	out := make(map[int64]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}
