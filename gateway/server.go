// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package gateway

import (
	"cmp"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
	"github.com/erain/glue/webui"
)

// Route describes one HTTP route registered on the gateway Server.
type Route struct {
	Method, Path string
	Public       bool
}

var gatewayRoutes = []Route{
	{http.MethodGet, "/", true}, {http.MethodGet, "/index.html", true}, {http.MethodGet, "/static/app.js", true}, {http.MethodGet, "/static/api.js", true}, {http.MethodGet, "/static/app.css", true},
	{http.MethodGet, "/healthz", true}, {http.MethodGet, "/auth/github/login", true}, {http.MethodGet, "/auth/github/callback", true},
	{http.MethodGet, "/v1/health", false}, {http.MethodGet, "/v1/session", false}, {http.MethodPost, "/v1/session/login", false}, {http.MethodPost, "/v1/session/logout", false},
	{http.MethodGet, "/v1/tokens", false}, {http.MethodPost, "/v1/tokens", false}, {http.MethodDelete, "/v1/tokens/{id}", false},
	{http.MethodGet, "/v1/backends", false}, {http.MethodGet, "/v1/diagnostics", false}, {http.MethodGet, "/v1/jobs", false}, {http.MethodPost, "/v1/jobs", false},
	{http.MethodGet, "/v1/jobs/{id}", false}, {http.MethodDelete, "/v1/jobs/{id}", false}, {http.MethodPost, "/v1/jobs/{id}/start", false},
	{http.MethodGet, "/v1/jobs/{id}/permissions", false}, {http.MethodPost, "/v1/jobs/{id}/permissions/{permID}/decision", false},
	{http.MethodGet, "/v1/jobs/{id}/result", false}, {http.MethodGet, "/v1/jobs/{id}/events", false}, {http.MethodPost, "/v1/jobs/{id}/messages", false}, {http.MethodPost, "/v1/jobs/{id}/continue", false},
	{http.MethodPost, "/v1/jobs/{id}/answers", false},
}

// Server serves the glue-gateway HTTP API, upstream SSE proxy, and browser UI.
type Server struct {
	cfg      Config
	reg      *Registry
	backends map[string]Backend
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	sweepMu  sync.Mutex
	watchers map[string]bool
	viewers  map[string]int
	waiting  map[string]time.Time
	remap    map[string]string
	orphans  []string
	rateHits map[string][]time.Time
	notify   chan struct{}
}

// Routes returns a copy of all HTTP routes registered on the gateway Server.
func (s *Server) Routes() []Route { return slices.Clone(gatewayRoutes) }

func ghAuthToken(parent context.Context) string {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	return strings.TrimSpace(string(out))
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
	reg.now = cfg.Now
	ctx, cancel := context.WithCancel(context.Background())
	backends := make(map[string]Backend, len(cfg.Backends))
	for _, b := range cfg.Backends {
		if cb := cfg.CustomBackends[b.Name]; cb != nil {
			backends[b.Name] = cb
			continue
		}
		if b.Type != "ax" {
			backends[b.Name] = NewLocalBackend(b, nil)
			continue
		}
		name := b.Name
		creds := cfg.Credentials
		if creds.GitHubToken == "" {
			creds.GitHubToken = ghAuthToken(ctx)
		}
		axb, err := axbackend.NewBackend(ctx, axbackend.Options{
			Connect: axbackend.ConnectOptions{
				AXServer:    b.AXServer,
				KubeConfig:  b.KubeConfig,
				KubeContext: b.KubeContext,
				Atespace:    b.Atespace,
			},
			Image:       b.Image,
			CPU:         b.Resources.CPU,
			Memory:      b.Resources.Memory,
			EgressHosts: b.EgressHosts,
			CacheDir:    filepath.Join(cfg.StateDir, "ax-jobs", name),
			Credentials: creds,
			GetJob: func(ctx context.Context, id string) (jobs.Spec, jobs.Status, error) {
				r, e := reg.GetJob(ctx, id)
				return r.Spec, r.Status, e
			},
			SetStatus: func(ctx context.Context, id string, s jobs.Spec, st jobs.Status) error {
				return reg.UpsertStatus(ctx, id, name, s, st)
			},
		})
		if err != nil {
			cancel()
			for _, open := range backends {
				if c, ok := open.(io.Closer); ok {
					_ = c.Close()
				}
			}
			_ = reg.Close()
			return nil, err
		}
		backends[b.Name] = axb
	}
	s := &Server{
		cfg: cfg, reg: reg, backends: backends, ctx: ctx, cancel: cancel, watchers: map[string]bool{},
		viewers: map[string]int{}, waiting: map[string]time.Time{}, remap: map[string]string{}, orphans: []string{}, rateHits: map[string][]time.Time{}, notify: make(chan struct{}),
	}
	s.reconcileOrphans(ctx)
	s.wg.Go(s.retentionLoop)
	return s, nil
}

// Close stops active upstream stream watchers and closes the registry.
func (s *Server) Close() error {
	s.cancel()
	s.wg.Wait()
	for _, b := range s.backends {
		if c, ok := b.(io.Closer); ok {
			_ = c.Close()
		}
	}
	return s.reg.Close()
}
func (s *Server) Poke() { s.mu.Lock(); close(s.notify); s.notify = make(chan struct{}); s.mu.Unlock() }
func isTerminal(st jobs.Status) bool {
	return st != "" && st != jobs.Created && st != SubmitUnknown && st != jobs.Running && st != jobs.WaitingForInput && st != jobs.WaitingForCredentials && st != jobs.Suspended
}
func (s *Server) addViewer(id string, delta int) {
	s.mu.Lock()
	s.viewers[id] += delta
	s.waiting[id] = s.cfg.Now().UTC()
	s.mu.Unlock()
	s.Poke()
}
func (s *Server) idleDue(jobID string) bool {
	rec, err := s.reg.GetJob(context.Background(), jobID)
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.cfg.SuspendAfter
	return err == nil && rec.Status == jobs.WaitingForInput && s.viewers[jobID] == 0 && d > 0 && s.cfg.Now().Sub(rec.UpdatedAt) >= d && s.cfg.Now().Sub(s.waiting[jobID]) >= d
}
func (s *Server) gwEvent(ctx context.Context, jobID, typ, reason string) error {
	_, err := s.reg.AppendEvent(ctx, jobID, eventlog.Envelope{Time: s.cfg.Now().UTC(), Type: typ, Payload: map[string]any{"reason": reason}})
	s.Poke()
	return err
}

// deleteJobTask persists the job completion result in the registry before
// deleting the backend sandbox task so DeleteTask never loses /workspace first.
func (s *Server) deleteJobTask(ctx context.Context, id string) error {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	rec, err := s.reg.GetJob(ctx, id)
	if err != nil || rec.TaskDeleted {
		return err
	}
	b := s.backends[rec.Backend]
	if rec.Result == nil {
		res, err := b.Result(ctx, rec.BackendRef)
		if err != nil {
			return err
		}
		if err := s.reg.SaveResult(ctx, id, res); err != nil {
			return err
		}
	}
	if td, ok := b.(TaskDeleter); ok {
		if err := td.DeleteTask(ctx, rec.BackendRef); err != nil {
			return err
		}
	}
	return s.reg.MarkTaskDeleted(ctx, id)
}

func (s *Server) sweepRetention(ctx context.Context) {
	if s.cfg.RetainTasks <= 0 {
		return
	}
	recs, _ := s.reg.ActiveTaskJobs(ctx)
	for _, rec := range recs {
		if isTerminal(rec.Status) && s.cfg.Now().Sub(rec.UpdatedAt) >= s.cfg.RetainTasks {
			_ = s.deleteJobTask(ctx, rec.ID)
		}
	}
}
func (s *Server) retentionLoop() {
	for s.ctx.Err() == nil {
		s.mu.Lock()
		notify := s.notify
		s.mu.Unlock()
		s.sweepRetention(s.ctx)
		select {
		case <-s.ctx.Done():
			return
		case <-notify:
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (s *Server) reconcileOrphans(ctx context.Context) []string {
	found := []string{}
	for _, b := range s.backends {
		if tl, ok := b.(TaskLister); ok {
			names, _ := tl.ListTasks(ctx)
			for _, name := range names {
				if strings.HasPrefix(name, "glue-") {
					if _, err := s.reg.GetJob(ctx, strings.TrimPrefix(name, "glue-")); errors.Is(err, sql.ErrNoRows) {
						found = append(found, name)
					}
				}
			}
		}
	}
	slices.Sort(found)
	found = slices.Compact(found)
	s.mu.Lock()
	for _, name := range found {
		if !slices.Contains(s.orphans, name) {
			log.Printf("gateway: orphan task %s has no registry row", name)
		}
	}
	s.orphans = slices.Clone(found)
	s.mu.Unlock()
	return found
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
		sb, isSusp := b.(Suspender)
		for s.ctx.Err() == nil {
			rec, err := s.reg.GetJob(bg, jobID)
			if err != nil || isTerminal(rec.Status) || rec.Status == jobs.Suspended {
				return
			}
			streamCtx, cancelStream := context.WithCancel(s.ctx)
			go func() {
				for isSusp && streamCtx.Err() == nil && !s.idleDue(jobID) {
					time.Sleep(10 * time.Millisecond)
				}
				if isSusp {
					cancelStream()
				}
			}()
			_ = b.Stream(streamCtx, rec.BackendRef, rec.UpstreamSeq, func(ev eventlog.Envelope) error {
				if m, ok := ev.Payload.(map[string]any); ok && ev.Type == "permission_decision" {
					pid, _ := m["permission_id"].(string)
					s.mu.Lock()
					if orig := s.remap[jobID+"\x00"+pid]; orig != "" {
						delete(s.remap, jobID+"\x00"+pid)
						m["original_permission_id"] = orig
					}
					s.mu.Unlock()
				}
				if ev.Type == "job_done" {
					if res, err := b.Result(s.ctx, rec.BackendRef); err == nil {
						_ = s.reg.SaveResult(bg, jobID, res)
					}
				}
				_, err := s.reg.AppendEvent(bg, jobID, ev)
				if ev.Type == "job_done" && isSusp && (s.cfg.SuspendOnFinish == nil || *s.cfg.SuspendOnFinish) && sb.Suspend(s.ctx, rec.BackendRef) == nil {
					_ = s.gwEvent(bg, jobID, "task_suspended", "finished")
				}
				s.Poke()
				return err
			})
			cancelStream()
			if isSusp && s.idleDue(jobID) && sb.Suspend(s.ctx, rec.BackendRef) == nil {
				_ = s.gwEvent(bg, jobID, "task_suspended", "waiting_for_input")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

func (s *Server) wakeIfSuspended(ctx context.Context, id, reason string) error {
	rec, err := s.reg.GetJob(ctx, id)
	if err != nil {
		return err
	}
	b := s.backends[rec.Backend]
	sb, ok := b.(Suspender)
	if !ok || (rec.Status != jobs.Suspended && !(isTerminal(rec.Status) && rec.StatusReason == "finished" && reason == "continue")) {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, s.cfg.ResumeTimeout)
	defer cancel()
	if err := sb.Resume(rctx, rec.BackendRef); err != nil {
		return err
	}
	err = s.gwEvent(ctx, id, "task_resumed", reason)
	s.ensureWatcher(id, b)
	return err
}

func (s *Server) remapPerm(ctx context.Context, id, permID string, all []jobs.PermissionInfo) (string, error) {
	i := slices.IndexFunc(all, func(p jobs.PermissionInfo) bool { return p.ID == permID })
	rctx, cancel := context.WithTimeout(ctx, s.cfg.ResumeTimeout)
	defer cancel()
	for i >= 0 {
		perms, _ := s.reg.Permissions(rctx, id, "pending")
		if j := slices.IndexFunc(perms, func(p jobs.PermissionInfo) bool {
			return p.ID != permID && ((p.Request.Tool == all[i].Request.Tool && p.Request.Target == all[i].Request.Target) || len(perms) == 1)
		}); j >= 0 {
			s.mu.Lock()
			s.remap[id+"\x00"+perms[j].ID] = permID
			s.mu.Unlock()
			return "permissions/" + perms[j].ID + "/decision", nil
		}
		if rctx.Err() != nil {
			return "", rctx.Err()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "permissions/" + permID + "/decision", nil
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
func eqToken(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func (s *Server) isAllowedLogin(login string) bool {
	return s.cfg.Auth.Mode != "github" || slices.ContainsFunc(s.cfg.Auth.AllowedLogins, func(a string) bool { return strings.EqualFold(a, login) })
}
func (s *Server) allowAuthRate(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := cmp.Or(host, r.RemoteAddr, "local")
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now().UTC()
	hits := slices.DeleteFunc(s.rateHits[ip], func(t time.Time) bool { return now.Sub(t) >= time.Minute })
	if len(hits) >= cmp.Or(s.cfg.Auth.RateLimitPerMin, DefaultAuthRateLimit) {
		s.rateHits[ip] = hits
		return false
	}
	s.rateHits[ip] = append(hits, now)
	return true
}
func (s *Server) isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(strings.ToLower(s.cfg.PublicURL), "https://") || (s.cfg.TrustForwardedProto && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}
func (s *Server) callbackURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL + "/auth/github/callback"
	}
	scheme := "http://"
	if s.isHTTPS(r) {
		scheme = "https://"
	}
	return scheme + cmp.Or(r.Host, "127.0.0.1") + "/auth/github/callback"
}
func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, name, val string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: val, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: s.isHTTPS(r), SameSite: http.SameSiteLaxMode})
}
func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if !s.allowAuthRate(r) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many authentication requests")
		return
	}
	state := randHex(32)
	s.setCookie(w, r, "glue_oauth_state", state, 600)
	q := url.Values{"client_id": {s.cfg.Auth.ClientID}, "redirect_uri": {s.callbackURL(r)}, "state": {state}}
	http.Redirect(w, r, s.cfg.Auth.AuthorizeURL+"?"+q.Encode(), http.StatusFound)
}
func fetchJSON(ctx context.Context, method, rawURL, auth, body string, dst any) error {
	req, _ := http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst)
}
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if !s.allowAuthRate(r) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many authentication requests")
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	c, err := r.Cookie("glue_oauth_state")
	s.setCookie(w, r, "glue_oauth_state", "", -1)
	if err != nil || !eqToken(state, c.Value) || code == "" {
		writeErr(w, http.StatusBadRequest, "invalid_oauth_state", "missing or mismatched OAuth state")
		return
	}
	form := url.Values{"client_id": {s.cfg.Auth.ClientID}, "client_secret": {s.cfg.Auth.ClientSecret}, "code": {code}, "redirect_uri": {s.callbackURL(r)}, "state": {state}}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if fetchJSON(r.Context(), http.MethodPost, s.cfg.Auth.TokenURL, "", form.Encode(), &tok) != nil || tok.AccessToken == "" || fetchJSON(r.Context(), http.MethodGet, s.cfg.Auth.UserURL, tok.AccessToken, "", &user) != nil || user.Login == "" || user.ID <= 0 {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "OAuth exchange or user fetch failed")
		return
	}
	if !s.isAllowedLogin(user.Login) {
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>Access Denied — Glue</title><link rel="stylesheet" href="/static/app.css"></head><body><main class="container"><section class="card" data-testid="access-denied-view"><h2>Access Denied</h2><p class="error-text">Your GitHub account is not in the gateway allowlist (<code>allowed_logins</code>).</p><p><a class="btn btn-secondary" href="/">Back to sign in</a></p></section></main></body></html>`)
			return
		}
		writeErr(w, http.StatusForbidden, "forbidden", "GitHub login is not in allowed_logins")
		return
	}
	if sess, err := s.reg.CreateSession(r.Context(), user.Login, user.ID, s.cfg.Auth.SessionAbsoluteTTL); err == nil {
		s.setCookie(w, r, "glue_session", sess.ID, int(s.cfg.Auth.SessionAbsoluteTTL.Seconds()))
		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (Session, bool) {
	if got := r.Header.Get("Authorization"); got != "" {
		raw := strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
		if s.cfg.Auth.Mode == "dev-token" && eqToken(got, "Bearer "+s.cfg.Auth.Token) {
			return Session{Login: "dev-token"}, true
		}
		if strings.HasPrefix(got, "Bearer ") && raw != "" {
			if tok, err := s.reg.AuthenticateToken(r.Context(), raw); err == nil && s.isAllowedLogin(tok.Owner) {
				return Session{Login: tok.Owner}, true
			}
		}
	} else if c, err := r.Cookie("glue_session"); err == nil {
		if sess, err := s.reg.AuthenticateSession(r.Context(), c.Value, s.cfg.Auth.SessionIdleTTL); err == nil && s.isAllowedLogin(sess.Login) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions || eqToken(r.Header.Get("X-Glue-CSRF"), sess.CSRFToken) {
				return sess, true
			}
			writeErr(w, http.StatusForbidden, "forbidden", "missing or invalid CSRF token")
			return Session{}, false
		}
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"auth_mode": s.cfg.Auth.Mode, "error": map[string]any{"code": "unauthorized", "message": "missing or invalid credentials", "retryable": false}})
	return Session{}, false
}

// NewHTTPServer returns an http.Server configured with public-facing timeouts.
func NewHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func isTooLarge(w http.ResponseWriter, err error) bool {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
		return true
	}
	return false
}

// ServeHTTP routes browser UI, session, health, backend list, and job requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.cfg.Now()
	recW := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	w = recW
	defer func() {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		log.Printf("gateway: access method=%s path=%s status=%d duration_ms=%d remote_ip=%s", r.Method, r.URL.Path, recW.status, s.cfg.Now().Sub(start).Milliseconds(), cmp.Or(host, r.RemoteAddr, "local"))
	}()
	if s.isHTTPS(r) {
		w.Header().Set("Strict-Transport-Security", HSTSHeader)
	}
	limit := cmp.Or(s.cfg.MaxBodyBytes, int64(DefaultMaxBodyBytes))
	if r.ContentLength > limit {
		writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
	if r.URL.Path == "/" || r.URL.Path == "/index.html" || strings.HasPrefix(r.URL.Path, "/static/") {
		webui.Handler().ServeHTTP(w, r)
		return
	}
	switch route := r.Method + " " + r.URL.Path; route {
	case "GET /healthz":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	case "GET /auth/github/login":
		s.handleGitHubLogin(w, r)
		return
	case "GET /auth/github/callback":
		s.handleGitHubCallback(w, r)
		return
	case "GET /v1/health":
		if s.cfg.Auth.Mode != "dev-token" {
			if _, ok := s.authorize(w, r); !ok {
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": 2})
		return
	case "POST /v1/session/login":
		if !s.allowAuthRate(r) {
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many authentication requests")
			return
		}
		var body struct{ Token string }
		err := json.NewDecoder(r.Body).Decode(&body)
		if isTooLarge(w, err) {
			return
		}
		if s.cfg.Auth.Mode != "dev-token" || err != nil || !eqToken(body.Token, s.cfg.Auth.Token) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		sess, err := s.reg.CreateSession(r.Context(), "dev-token", 0, s.cfg.Auth.SessionAbsoluteTTL)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		s.setCookie(w, r, "glue_session", sess.ID, int(s.cfg.Auth.SessionAbsoluteTTL.Seconds()))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authenticated": true, "auth_mode": s.cfg.Auth.Mode, "login": sess.Login, "csrf_token": sess.CSRFToken})
		return
	}
	sess, ok := s.authorize(w, r)
	if !ok {
		return
	}
	r = r.WithContext(WithOwner(r.Context(), sess.Login))
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/session":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authenticated": true, "auth_mode": s.cfg.Auth.Mode, "login": sess.Login, "github_id": sess.GitHubID, "csrf_token": sess.CSRFToken})
		return
	case "POST /v1/session/logout":
		if c, err := r.Cookie("glue_session"); err == nil {
			_ = s.reg.DeleteSession(r.Context(), c.Value)
		}
		s.setCookie(w, r, "glue_session", "", -1)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	case "GET /v1/tokens":
		toks, err := s.reg.ListTokens(r.Context(), sess.Login)
		respond(w, http.StatusOK, map[string]any{"tokens": toks}, err)
		return
	case "POST /v1/tokens":
		var body struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&body); isTooLarge(w, err) {
			return
		}
		tok, err := s.reg.CreateToken(r.Context(), sess.Login, body.Name)
		respond(w, http.StatusCreated, tok, err)
		return
	case "GET /v1/backends":
		writeJSON(w, http.StatusOK, map[string]any{"backends": s.cfg.Backends, "default_backend": s.cfg.DefaultBackend})
		return
	case "GET /v1/diagnostics":
		writeJSON(w, http.StatusOK, map[string]any{"orphan_tasks": s.reconcileOrphans(r.Context())})
		return
	case "GET /v1/jobs":
		s.sweepRetention(r.Context())
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		list, err := s.reg.QueryJobs(r.Context(), jobs.Filter{Status: jobs.Status(r.URL.Query().Get("status")), Limit: cmp.Or(limit, 50), Before: r.URL.Query().Get("before")})
		respond(w, http.StatusOK, map[string]any{"jobs": list}, err)
		return
	case "POST /v1/jobs":
		var spec jobs.Spec
		err := json.NewDecoder(r.Body).Decode(&spec)
		if isTooLarge(w, err) {
			return
		}
		name := cmp.Or(strings.TrimSpace(r.URL.Query().Get("backend")), strings.TrimSpace(spec.Backend), s.cfg.DefaultBackend)
		b := s.backends[name]
		if err != nil || b == nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", "invalid job spec or backend")
			return
		}
		id, _ := jobs.NewID()
		created, err := b.Create(r.Context(), cmp.Or(strings.TrimSpace(spec.ID), id), spec)
		_, isAX := b.(*axbackend.Backend)
		if err == nil && !isAX {
			_, err = s.reg.CreateJob(r.Context(), Record{ID: created.ID, Backend: name, BackendRef: created.ID, Owner: sess.Login, Spec: created.Spec, Status: created.Status, CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt})
		}
		if err == nil {
			if start, _ := strconv.ParseBool(r.URL.Query().Get("start")); start {
				created, err = b.Start(r.Context(), created.ID)
			}
			if err == nil {
				s.ensureWatcher(created.ID, b)
			}
		}
		created.Spec.Backend = name
		respond(w, http.StatusCreated, map[string]any{"job": created}, err)
		return
	}
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/tokens/") {
		id := strings.TrimPrefix(r.URL.Path, "/v1/tokens/")
		respond(w, http.StatusOK, map[string]any{"id": id, "revoked": true}, s.reg.RevokeToken(r.Context(), sess.Login, id))
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/jobs/") {
		writeErr(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/"), "/")
	id, sub := parts[0], strings.Join(parts[1:], "/")
	if r.Method == http.MethodGet && sub == "" {
		s.sweepRetention(r.Context())
	}
	rec, err := s.reg.GetJob(r.Context(), id)
	if err == nil && rec.Owner != "" && rec.Owner != sess.Login {
		err = sql.ErrNoRows
	}
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	b := s.backends[rec.Backend]
	switch r.Method + " " + sub {
	case "GET ":
		if _, isAX := b.(*axbackend.Backend); isAX && !isTerminal(rec.Status) && rec.Status != jobs.Suspended && !rec.TaskDeleted {
			if st, serr := b.Status(r.Context(), id); serr == nil && st != "" && st != rec.Status {
				_ = s.reg.UpsertStatus(r.Context(), id, rec.Backend, rec.Spec, st)
			}
		}
		detail, err := s.reg.Inspect(r.Context(), id)
		respond(w, http.StatusOK, detail, err)
	case "DELETE ":
		if purge, _ := strconv.ParseBool(r.URL.Query().Get("purge")); purge {
			if !rec.TaskDeleted {
				if td, ok := b.(TaskDeleter); ok {
					if err := td.DeleteTask(r.Context(), rec.BackendRef); err != nil {
						respond(w, 0, nil, err)
						return
					}
				}
			}
			respond(w, http.StatusOK, map[string]any{"job_id": id, "deleted": true}, s.reg.PurgeJob(r.Context(), id))
			return
		}
		if err := s.wakeIfSuspended(r.Context(), id, "cancel"); err != nil {
			respond(w, 0, nil, err)
			return
		}
		s.ensureWatcher(id, b)
		respond(w, http.StatusAccepted, map[string]any{"job_id": id, "canceled": true}, b.Cancel(r.Context(), rec.BackendRef))
	case "POST start":
		job, err := b.Start(r.Context(), rec.BackendRef)
		if err == nil {
			s.ensureWatcher(id, b)
		}
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
		if err == nil {
			_ = s.reg.SaveResult(r.Context(), id, res)
		}
		respond(w, http.StatusOK, res, err)
	case "GET events":
		q := r.URL.Query()
		after, err := strconv.ParseInt(cmp.Or(q.Get("after"), r.Header.Get("Last-Event-ID"), "0"), 10, 64)
		flusher, ok := w.(http.Flusher)
		if err != nil || after < 0 || !ok {
			writeErr(w, http.StatusBadRequest, "invalid_request", "invalid stream request")
			return
		}
		cached := q.Get("cached") == "1" || q.Get("cached") == "true" || q.Get("follow") == "false" || (rec.Status == jobs.Suspended && !strings.Contains(r.Header.Get("Accept"), "text/event-stream") && q.Get("live") != "1")
		if !cached {
			if err := s.wakeIfSuspended(r.Context(), id, "stream_attach"); err != nil {
				respond(w, 0, nil, err)
				return
			}
			s.addViewer(id, 1)
			defer s.addViewer(id, -1)
			s.ensureWatcher(id, b)
		}
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
			if cached || isTerminal(cur.Status) {
				return
			}
			select {
			case <-notify:
			case <-r.Context().Done():
				return
			}
		}
	default:
		if r.Method == http.MethodPost && (sub == "messages" || sub == "continue" || sub == "answers" || (len(parts) == 4 && parts[1] == "permissions" && parts[3] == "decision")) {
			body, err := io.ReadAll(r.Body)
			if isTooLarge(w, err) {
				return
			}
			allPerms, _ := s.reg.Permissions(r.Context(), id, "")
			if err := s.wakeIfSuspended(r.Context(), id, sub); err != nil {
				respond(w, 0, nil, err)
				return
			}
			s.ensureWatcher(id, b)
			if len(parts) == 4 && rec.Status == jobs.Suspended {
				if sub, err = s.remapPerm(r.Context(), id, parts[2], allPerms); err != nil {
					respond(w, 0, nil, err)
					return
				}
			}
			if sub == "messages" && r.URL.Query().Get("interrupt") == "true" {
				sub += "?interrupt=true"
			}
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
