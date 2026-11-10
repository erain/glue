// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package gateway

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
	"github.com/erain/glue/webui"
)

// Server serves the glue-gateway HTTP API, upstream SSE proxy, and browser UI.
type Server struct {
	cfg      Config
	reg      *Registry
	backends map[string]Backend
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	watchers map[string]bool
	notify   chan struct{}
}

// NewServer validates configuration, opens the registry, and initializes backends.
func NewServer(cfg Config) (*Server, error) {
	cfg, err := cfg.Validate()
	if err != nil {
		return nil, err
	}
	reg, err := OpenRegistry(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	backends := make(map[string]Backend, len(cfg.Backends))
	for _, b := range cfg.Backends {
		backends[b.Name] = NewLocalBackend(b, nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		cfg: cfg, reg: reg, backends: backends,
		ctx: ctx, cancel: cancel, watchers: map[string]bool{}, notify: make(chan struct{}),
	}, nil
}

// Close stops active upstream stream watchers and closes the registry.
func (s *Server) Close() error {
	s.cancel()
	s.wg.Wait()
	return s.reg.Close()
}
func isTerminal(st jobs.Status) bool {
	return st != "" && st != jobs.Created && st != jobs.Running && st != jobs.WaitingForInput
}

func (s *Server) ensureWatcher(jobID string, b Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchers[jobID] || s.ctx.Err() != nil {
		return
	}
	s.watchers[jobID] = true
	s.wg.Go(func() {
		defer func() { s.mu.Lock(); delete(s.watchers, jobID); s.mu.Unlock() }()
		bg := context.Background()
		for s.ctx.Err() == nil {
			rec, err := s.reg.GetJob(bg, jobID)
			if err != nil || isTerminal(rec.Status) {
				return
			}
			_ = b.Stream(s.ctx, rec.BackendRef, rec.LastSeq, func(ev eventlog.Envelope) error {
				if ev.Type == "job_done" {
					if res, err := b.Result(s.ctx, rec.BackendRef); err == nil {
						_ = s.reg.SaveResult(bg, jobID, res)
					}
				}
				_, err := s.reg.AppendEvent(bg, jobID, ev)
				s.mu.Lock()
				close(s.notify)
				s.notify = make(chan struct{})
				s.mu.Unlock()
				return err
			})
			if rec, err := s.reg.GetJob(bg, jobID); err == nil && !isTerminal(rec.Status) {
				time.Sleep(20 * time.Millisecond)
			}
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg, "retryable": false}})
}
func respond(w http.ResponseWriter, status int, v any, err error) {
	var se *jobclient.StatusError
	if err == nil {
		writeJSON(w, status, v)
	} else if errors.As(err, &se) {
		writeErr(w, se.Code, "upstream_error", se.Message)
	} else if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "job not found")
	} else if errors.Is(err, jobs.ErrInvalid) {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
	} else if errors.Is(err, jobs.ErrConflict) {
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	} else {
		writeErr(w, http.StatusBadGateway, "bad_gateway", err.Error())
	}
}
func (s *Server) tokenHash(prefix string) string {
	sum := sha256.Sum256([]byte(prefix + s.cfg.Auth.Token))
	return hex.EncodeToString(sum[:])
}
func eqToken(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if got := r.Header.Get("Authorization"); got != "" {
		if eqToken(got, "Bearer "+s.cfg.Auth.Token) {
			return true
		}
	} else if c, err := r.Cookie("glue_session"); err == nil && eqToken(c.Value, s.tokenHash("glue-session-v1:")) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions || eqToken(r.Header.Get("X-Glue-CSRF"), s.tokenHash("glue-csrf-v1:")) {
			return true
		}
		writeErr(w, http.StatusForbidden, "forbidden", "missing or invalid CSRF token")
		return false
	}
	writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
	return false
}

// ServeHTTP routes browser UI, session, health, backend list, and job requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" || r.URL.Path == "/index.html" || strings.HasPrefix(r.URL.Path, "/static/") {
		webui.Handler().ServeHTTP(w, r)
		return
	}
	route := r.Method + " " + r.URL.Path
	if route != "GET /v1/health" && route != "POST /v1/session/login" && !s.authorize(w, r) {
		return
	}
	switch route {
	case "GET /v1/health":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": 2})
		return
	case "POST /v1/session/login":
		var body struct{ Token string }
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil || !eqToken(body.Token, s.cfg.Auth.Token) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "glue_session", Value: s.tokenHash("glue-session-v1:"), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		fallthrough
	case "GET /v1/session":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authenticated": true, "csrf_token": s.tokenHash("glue-csrf-v1:")})
		return
	case "GET /v1/backends":
		writeJSON(w, http.StatusOK, map[string]any{"backends": s.cfg.Backends, "default_backend": s.cfg.DefaultBackend})
		return
	case "GET /v1/jobs":
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		list, err := s.reg.QueryJobs(r.Context(), jobs.Filter{Status: jobs.Status(r.URL.Query().Get("status")), Limit: cmp.Or(limit, 50), Before: r.URL.Query().Get("before")})
		respond(w, http.StatusOK, map[string]any{"jobs": list}, err)
		return
	case "POST /v1/jobs":
		var spec jobs.Spec
		err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&spec)
		name := cmp.Or(strings.TrimSpace(r.URL.Query().Get("backend")), strings.TrimSpace(spec.Backend), s.cfg.DefaultBackend)
		b := s.backends[name]
		if err != nil || b == nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", "invalid job spec or backend")
			return
		}
		id, _ := jobs.NewID()
		created, err := b.Create(r.Context(), cmp.Or(strings.TrimSpace(spec.ID), id), spec)
		if err == nil {
			_, err = s.reg.CreateJob(r.Context(), Record{ID: created.ID, Backend: name, BackendRef: created.ID, Owner: "dev-token", Spec: created.Spec, Status: created.Status, CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt})
		}
		if err == nil {
			s.ensureWatcher(created.ID, b)
			if start, _ := strconv.ParseBool(r.URL.Query().Get("start")); start {
				created, err = b.Start(r.Context(), created.ID)
			}
		}
		created.Spec.Backend = name
		respond(w, http.StatusCreated, map[string]any{"job": created}, err)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/jobs/") {
		writeErr(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/"), "/")
	id, sub := parts[0], strings.Join(parts[1:], "/")
	rec, err := s.reg.GetJob(r.Context(), id)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	b := s.backends[rec.Backend]
	switch r.Method + " " + sub {
	case "GET ":
		detail, err := s.reg.Inspect(r.Context(), id)
		respond(w, http.StatusOK, detail, err)
	case "DELETE ":
		s.ensureWatcher(id, b)
		respond(w, http.StatusAccepted, map[string]any{"job_id": id, "canceled": true}, b.Cancel(r.Context(), rec.BackendRef))
	case "POST start":
		s.ensureWatcher(id, b)
		job, err := b.Start(r.Context(), rec.BackendRef)
		job.Spec.Backend = rec.Backend
		respond(w, http.StatusOK, map[string]any{"job": job}, err)
	case "GET permissions":
		perms, err := s.reg.Permissions(r.Context(), id, r.URL.Query().Get("state"))
		respond(w, http.StatusOK, map[string]any{"permissions": perms}, err)
	case "GET result":
		if rec.Result != nil {
			writeJSON(w, http.StatusOK, rec.Result)
			return
		}
		res, err := b.Result(r.Context(), rec.BackendRef)
		_ = s.reg.SaveResult(r.Context(), id, res)
		respond(w, http.StatusOK, res, err)
	case "GET events":
		after, err := strconv.ParseInt(cmp.Or(r.URL.Query().Get("after"), r.Header.Get("Last-Event-ID"), "0"), 10, 64)
		flusher, ok := w.(http.Flusher)
		if err != nil || after < 0 || !ok {
			writeErr(w, http.StatusBadRequest, "invalid_request", "invalid stream request")
			return
		}
		s.ensureWatcher(id, b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		for {
			s.mu.Lock()
			notify := s.notify
			s.mu.Unlock()
			cur, err1 := s.reg.GetJob(r.Context(), id)
			evs, err2 := s.reg.Events(r.Context(), id, after)
			if errors.Join(err1, err2) != nil {
				return
			}
			for _, ev := range evs {
				_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, mustJSON(ev))
				after = ev.Seq
			}
			flusher.Flush()
			if isTerminal(cur.Status) {
				return
			}
			select {
			case <-notify:
			case <-r.Context().Done():
				return
			}
		}
	default:
		if r.Method == http.MethodPost && (sub == "messages" || sub == "continue" || (len(parts) == 4 && parts[1] == "permissions" && parts[3] == "decision")) {
			body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			s.ensureWatcher(id, b)
			if code, respBody, err := b.Do(r.Context(), r.Method, rec.BackendRef+"/"+sub, body); err == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = w.Write(respBody)
			} else {
				respond(w, 0, nil, err)
			}
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", "job route not found")
	}
}
