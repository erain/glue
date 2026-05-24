// Package daemon serves local Glue sessions over the ADR-0010 HTTP+SSE
// protocol.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/erain/glue"
)

const protocolVersion = 1

const defaultPermissionTimeout = 10 * time.Minute

// Host supplies sessions to a daemon Server.
type Host interface {
	Session(ctx context.Context, id string, options ...glue.SessionOption) (*glue.Session, error)
}

// Options configures [New].
type Options struct {
	// Host is required. A *glue.Agent satisfies this interface.
	Host Host

	// Token is required for every route except /v1/health.
	Token string

	// PermissionPolicy, when non-nil, can allow, deny, or defer
	// side-effecting tool permission requests before the daemon emits a
	// permission_request event. The daemon package remains channel-blind:
	// hosts that need channel/client policy decide from the supplied context.
	PermissionPolicy PermissionPolicy

	// Now supplies event timestamps. Nil uses time.Now.
	Now func() time.Time

	// NewID returns ids for runs and events. Nil uses crypto/rand.
	NewID func(prefix string) string

	// PermissionTimeout caps how long a side-effecting tool waits for an
	// HTTP decision. Zero uses a conservative default.
	PermissionTimeout time.Duration
}

// PermissionPolicy optionally decides side-effecting tool permission requests
// before the daemon asks the run owner over HTTP.
type PermissionPolicy interface {
	DecidePermission(context.Context, PermissionContext, glue.PermissionRequest) (PermissionPolicyDecision, error)
}

// PermissionPolicyFunc adapts a function into a [PermissionPolicy].
type PermissionPolicyFunc func(context.Context, PermissionContext, glue.PermissionRequest) (PermissionPolicyDecision, error)

// DecidePermission implements [PermissionPolicy].
func (f PermissionPolicyFunc) DecidePermission(ctx context.Context, info PermissionContext, req glue.PermissionRequest) (PermissionPolicyDecision, error) {
	if f == nil {
		return PermissionPolicyDecision{}, nil
	}
	return f(ctx, info, req)
}

// PermissionContext describes the daemon run that owns a permission request.
type PermissionContext struct {
	RunID     string
	SessionID string
	ClientID  string
}

// PermissionPolicyAction is the host policy outcome for one permission
// request.
type PermissionPolicyAction int

const (
	// PermissionPolicyPrompt keeps the existing daemon behavior: use cached
	// remembered decisions or ask the owning client over HTTP.
	PermissionPolicyPrompt PermissionPolicyAction = iota
	// PermissionPolicyAllow allows the side effect without asking the client.
	PermissionPolicyAllow
	// PermissionPolicyDeny denies the side effect without asking the client.
	PermissionPolicyDeny
)

// PermissionPolicyDecision is returned by [PermissionPolicy].
type PermissionPolicyDecision struct {
	Action      PermissionPolicyAction
	Reason      string
	RememberFor glue.RememberScope
}

// Server is an http.Handler for the local daemon protocol.
type Server struct {
	host              Host
	token             string
	permissionPolicy  PermissionPolicy
	now               func() time.Time
	newID             func(prefix string) string
	permissionTimeout time.Duration

	mu   sync.Mutex
	runs map[string]*run

	permMu        sync.Mutex
	sessionAllows map[string]struct{}
	targetAllows  map[string]struct{}
	foreverAllows map[string]struct{}
}

// EventEnvelope is the JSON payload sent in each SSE data frame.
type EventEnvelope struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Seq       int64     `json:"seq"`
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	Payload   any       `json:"payload,omitempty"`
}

type protocolError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type errorResponse struct {
	Error protocolError `json:"error"`
}

type startRunRequest struct {
	Text     string         `json:"text"`
	ClientID string         `json:"client_id,omitempty"`
	Role     string         `json:"role,omitempty"`
	Model    string         `json:"model,omitempty"`
	MaxTurns int            `json:"max_turns,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

type startRunResponse struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	EventsURL string `json:"events_url"`
}

type permissionRequestPayload struct {
	PermissionID string                 `json:"permission_id"`
	Request      glue.PermissionRequest `json:"request"`
	ExpiresAt    time.Time              `json:"expires_at"`
}

type permissionDecisionRequest struct {
	Allow       bool   `json:"allow"`
	Reason      string `json:"reason,omitempty"`
	RememberFor string `json:"remember_for,omitempty"`
}

type permissionDecisionResponse struct {
	PermissionID string `json:"permission_id"`
	Accepted     bool   `json:"accepted"`
}

// New constructs a daemon Server.
func New(opts Options) (*Server, error) {
	if opts.Host == nil {
		return nil, errors.New("daemon: Host is required")
	}
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, errors.New("daemon: Token is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	newID := opts.NewID
	if newID == nil {
		newID = randomID
	}
	permissionTimeout := opts.PermissionTimeout
	if permissionTimeout <= 0 {
		permissionTimeout = defaultPermissionTimeout
	}
	return &Server{
		host:              opts.Host,
		token:             token,
		permissionPolicy:  opts.PermissionPolicy,
		now:               now,
		newID:             newID,
		permissionTimeout: permissionTimeout,
		runs:              map[string]*run{},
		sessionAllows:     map[string]struct{}{},
		targetAllows:      map[string]struct{}{},
		foreverAllows:     map[string]struct{}{},
	}, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/health" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": protocolVersion})
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token", false)
		return
	}

	if sessionID, ok := parseStartRunPath(r.URL.Path); ok {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleStartRun(w, r, sessionID)
		return
	}

	if runID, ok := parseRunEventsPath(r.URL.Path); ok {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleRunEvents(w, r, runID)
		return
	}

	if runID, permissionID, ok := parsePermissionDecisionPath(r.URL.Path); ok {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handlePermissionDecision(w, r, runID, permissionID)
		return
	}

	if runID, ok := parseRunPath(r.URL.Path); ok {
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleCancelRun(w, r, runID)
		return
	}

	writeError(w, http.StatusNotFound, "not_found", "route not found", false)
}

func (s *Server) authorized(r *http.Request) bool {
	want := "Bearer " + s.token
	got := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req startRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", false)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "text is required", false)
		return
	}
	session, err := s.host.Session(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := s.newRun(sessionID, req.ClientID, cancel)
	go s.executeRun(ctx, run, session, req)

	writeJSON(w, http.StatusCreated, startRunResponse{
		RunID:     run.id,
		SessionID: sessionID,
		EventsURL: "/v1/runs/" + url.PathEscape(run.id) + "/events",
	})
}

func (s *Server) executeRun(ctx context.Context, r *run, session *glue.Session, req startRunRequest) {
	r.emit("run_start", map[string]any{"client_id": req.ClientID})
	options := []glue.PromptOption{
		glue.WithPermission(runPermission{server: s, run: r}),
		glue.WithEvents(func(event glue.Event) {
			r.emit(string(event.Type), event)
		}),
	}
	if strings.TrimSpace(req.Role) != "" {
		options = append(options, glue.WithRole(req.Role))
	}
	if strings.TrimSpace(req.Model) != "" {
		options = append(options, glue.WithModel(req.Model))
	}
	if req.MaxTurns > 0 {
		options = append(options, glue.WithMaxTurns(req.MaxTurns))
	}
	if len(req.Options) > 0 {
		options = append(options, glue.WithProviderOptions(req.Options))
	}

	result, err := session.Prompt(ctx, req.Text, options...)
	if err != nil {
		r.emit("run_error", map[string]any{"error": errorFor(err)})
		r.finish()
		return
	}
	r.emit("run_done", map[string]any{
		"text":         result.Text,
		"message":      result.Message,
		"new_messages": result.NewMessages,
	})
	r.finish()
}

func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request, runID string) {
	run := s.getRun(runID)
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found", false)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported", false)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	next := 0
	for {
		events, done, notify := run.eventsFrom(next)
		for _, event := range events {
			if err := writeSSE(w, event); err != nil {
				run.cancel()
				return
			}
			next++
			flusher.Flush()
		}
		if done {
			return
		}
		select {
		case <-notify:
		case <-r.Context().Done():
			run.cancel()
			return
		}
	}
}

func (s *Server) handleCancelRun(w http.ResponseWriter, _ *http.Request, runID string) {
	run := s.getRun(runID)
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found", false)
		return
	}
	run.cancel()
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": runID, "canceled": true})
}

func (s *Server) handlePermissionDecision(w http.ResponseWriter, r *http.Request, runID, permissionID string) {
	run := s.getRun(runID)
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found", false)
		return
	}
	if clientID := r.Header.Get("X-Glue-Client-ID"); clientID != "" && run.clientID != "" && clientID != run.clientID {
		writeError(w, http.StatusForbidden, "forbidden", "permission decision belongs to another client", false)
		return
	}
	var req permissionDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", false)
		return
	}
	scope, ok := parseRememberScope(req.RememberFor)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid remember_for", false)
		return
	}
	decision := glue.PermissionDecision{
		Allow:       req.Allow,
		Reason:      req.Reason,
		RememberFor: scope,
	}
	if !decision.Allow && strings.TrimSpace(decision.Reason) == "" {
		decision.Reason = "permission denied by daemon client"
	}
	if !run.resolvePermission(permissionID, decision) {
		writeError(w, http.StatusNotFound, "not_found", "permission request not found", false)
		return
	}
	writeJSON(w, http.StatusOK, permissionDecisionResponse{PermissionID: permissionID, Accepted: true})
}

func (s *Server) newRun(sessionID, clientID string, cancel context.CancelFunc) *run {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		id := s.newID("run")
		if _, exists := s.runs[id]; exists {
			continue
		}
		r := &run{
			id:        id,
			sessionID: sessionID,
			clientID:  clientID,
			cancel:    cancel,
			now:       s.now,
			newID:     s.newID,
			notify:    make(chan struct{}),
			pending:   map[string]*pendingPermission{},
		}
		s.runs[id] = r
		return r
	}
}

func (s *Server) getRun(id string) *run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

func parseStartRunPath(path string) (string, bool) {
	const prefix = "/v1/sessions/"
	const suffix = "/runs"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if raw == "" || strings.Contains(raw, "/") {
		return "", false
	}
	id, err := url.PathUnescape(raw)
	return id, err == nil && id != ""
}

func parseRunEventsPath(path string) (string, bool) {
	const prefix = "/v1/runs/"
	const suffix = "/events"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if raw == "" || strings.Contains(raw, "/") {
		return "", false
	}
	id, err := url.PathUnescape(raw)
	return id, err == nil && id != ""
}

func parsePermissionDecisionPath(path string) (runID, permissionID string, ok bool) {
	const prefix = "/v1/runs/"
	const middle = "/permissions/"
	const suffix = "/decision"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	rawRunID, rawPermissionID, found := strings.Cut(rest, middle)
	if !found || rawRunID == "" || rawPermissionID == "" || strings.Contains(rawRunID, "/") || strings.Contains(rawPermissionID, "/") {
		return "", "", false
	}
	runID, err := url.PathUnescape(rawRunID)
	if err != nil || runID == "" {
		return "", "", false
	}
	permissionID, err = url.PathUnescape(rawPermissionID)
	if err != nil || permissionID == "" {
		return "", "", false
	}
	return runID, permissionID, true
}

func parseRunPath(path string) (string, bool) {
	const prefix = "/v1/runs/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	raw := strings.TrimPrefix(path, prefix)
	if raw == "" || strings.Contains(raw, "/") {
		return "", false
	}
	id, err := url.PathUnescape(raw)
	return id, err == nil && id != ""
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(w, status, errorResponse{Error: protocolError{Code: code, Message: message, Retryable: retryable}})
}

func writeSSE(w http.ResponseWriter, event EventEnvelope) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %s\n", event.ID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event.Type); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return nil
}

func errorFor(err error) protocolError {
	code := "internal"
	if errors.Is(err, context.Canceled) {
		code = "canceled"
	}
	return protocolError{Code: code, Message: err.Error(), Retryable: false}
}

type runPermission struct {
	server *Server
	run    *run
}

func (p runPermission) Decide(ctx context.Context, req glue.PermissionRequest) (glue.PermissionDecision, error) {
	return p.server.decidePermission(ctx, p.run, req)
}

func (s *Server) decidePermission(ctx context.Context, run *run, req glue.PermissionRequest) (glue.PermissionDecision, error) {
	if decision, handled, err := s.applyPermissionPolicy(ctx, run, req); err != nil || handled {
		return decision, err
	}
	if decision, ok := s.cachedPermission(run, req); ok {
		return decision, nil
	}

	var permissionID string
	var pending *pendingPermission
	for {
		permissionID = s.newID("perm")
		pending = &pendingPermission{
			id:   permissionID,
			done: make(chan glue.PermissionDecision, 1),
		}
		if run.addPermission(pending) {
			break
		}
	}

	expiresAt := s.now().UTC().Add(s.permissionTimeout)
	run.emit("permission_request", permissionRequestPayload{
		PermissionID: permissionID,
		Request:      req,
		ExpiresAt:    expiresAt,
	})

	timer := time.NewTimer(s.permissionTimeout)
	defer timer.Stop()
	select {
	case decision := <-pending.done:
		s.rememberPermission(run, req, decision)
		return decision, nil
	case <-timer.C:
		if !run.expirePermission(permissionID, pending) {
			decision := <-pending.done
			s.rememberPermission(run, req, decision)
			return decision, nil
		}
		return glue.PermissionDecision{Allow: false, Reason: "permission denied: daemon permission request timed out"}, nil
	case <-ctx.Done():
		if !run.expirePermission(permissionID, pending) {
			decision := <-pending.done
			s.rememberPermission(run, req, decision)
			return decision, nil
		}
		return glue.PermissionDecision{}, ctx.Err()
	}
}

func (s *Server) applyPermissionPolicy(ctx context.Context, run *run, req glue.PermissionRequest) (glue.PermissionDecision, bool, error) {
	if s.permissionPolicy == nil {
		return glue.PermissionDecision{}, false, nil
	}
	info := PermissionContext{}
	if run != nil {
		info.RunID = run.id
		info.SessionID = run.sessionID
		info.ClientID = run.clientID
	}
	if info.SessionID == "" {
		info.SessionID = req.SessionID
	}
	policyDecision, err := s.permissionPolicy.DecidePermission(ctx, info, req)
	if err != nil {
		return glue.PermissionDecision{}, true, err
	}
	switch policyDecision.Action {
	case PermissionPolicyPrompt:
		return glue.PermissionDecision{}, false, nil
	case PermissionPolicyAllow:
		return glue.PermissionDecision{
			Allow:       true,
			Reason:      policyDecision.Reason,
			RememberFor: policyDecision.RememberFor,
		}, true, nil
	case PermissionPolicyDeny:
		reason := strings.TrimSpace(policyDecision.Reason)
		if reason == "" {
			reason = "permission denied by daemon policy"
		}
		return glue.PermissionDecision{Allow: false, Reason: reason}, true, nil
	default:
		return glue.PermissionDecision{}, true, fmt.Errorf("daemon: invalid permission policy action %d", policyDecision.Action)
	}
}

func (s *Server) cachedPermission(run *run, req glue.PermissionRequest) (glue.PermissionDecision, bool) {
	sessionKey := permissionSessionKey(run, req)
	targetKey := permissionTargetKey(run, req)
	foreverKey := permissionForeverKey(run, req)
	s.permMu.Lock()
	defer s.permMu.Unlock()
	if _, ok := s.foreverAllows[foreverKey]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberForever}, true
	}
	if _, ok := s.sessionAllows[sessionKey]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}, true
	}
	if _, ok := s.targetAllows[targetKey]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSessionTarget}, true
	}
	return glue.PermissionDecision{}, false
}

func (s *Server) rememberPermission(run *run, req glue.PermissionRequest, decision glue.PermissionDecision) {
	if !decision.Allow {
		return
	}
	s.permMu.Lock()
	defer s.permMu.Unlock()
	switch decision.RememberFor {
	case glue.RememberSession:
		s.sessionAllows[permissionSessionKey(run, req)] = struct{}{}
	case glue.RememberSessionTarget:
		s.targetAllows[permissionTargetKey(run, req)] = struct{}{}
	case glue.RememberForever:
		s.foreverAllows[permissionForeverKey(run, req)] = struct{}{}
	}
}

func permissionSessionKey(run *run, req glue.PermissionRequest) string {
	return permissionOwnerKey(run, req) + "\x00" + req.SessionID + "\x00" + req.Tool + "\x00" + req.Action
}

func permissionTargetKey(run *run, req glue.PermissionRequest) string {
	return permissionSessionKey(run, req) + "\x00" + req.Target
}

func permissionForeverKey(run *run, req glue.PermissionRequest) string {
	return permissionOwnerKey(run, req) + "\x00" + req.Tool + "\x00" + req.Action + "\x00" + req.Target
}

func permissionOwnerKey(run *run, req glue.PermissionRequest) string {
	if run != nil && strings.TrimSpace(run.clientID) != "" {
		return "client:" + strings.TrimSpace(run.clientID)
	}
	if run != nil && strings.TrimSpace(run.sessionID) != "" {
		return "session:" + strings.TrimSpace(run.sessionID)
	}
	return "session:" + strings.TrimSpace(req.SessionID)
}

func parseRememberScope(raw string) (glue.RememberScope, bool) {
	switch strings.TrimSpace(raw) {
	case "", "never":
		return glue.RememberNever, true
	case "session":
		return glue.RememberSession, true
	case "session_target":
		return glue.RememberSessionTarget, true
	case "forever":
		return glue.RememberForever, true
	default:
		return glue.RememberNever, false
	}
}

func randomID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
