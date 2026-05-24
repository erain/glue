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

	// Now supplies event timestamps. Nil uses time.Now.
	Now func() time.Time

	// NewID returns ids for runs and events. Nil uses crypto/rand.
	NewID func(prefix string) string
}

// Server is an http.Handler for the local daemon protocol.
type Server struct {
	host  Host
	token string
	now   func() time.Time
	newID func(prefix string) string

	mu   sync.Mutex
	runs map[string]*run
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
	return &Server{
		host:  opts.Host,
		token: token,
		now:   now,
		newID: newID,
		runs:  map[string]*run{},
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

func randomID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
