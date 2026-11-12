// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erain/glue/jobs"
)

func TestUIServesIndexWithSecurityHeaders(t *testing.T) {
	s, _, _ := jobHTTPFixture(t, nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") ||
		w.Header().Get("X-Content-Type-Options") != "nosniff" ||
		w.Header().Get("Referrer-Policy") != "no-referrer" ||
		w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("unexpected index response: %d %v", w.Code, w.Header())
	}
}

func TestStaticContentTypes(t *testing.T) {
	s, _, _ := jobHTTPFixture(t, nil)
	for path, wantType := range map[string]string{"/static/app.js": "application/javascript", "/static/app.css": "text/css"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), wantType) {
			t.Fatalf("GET %s = %d (%q)", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
}

func TestLoginSetsCookie(t *testing.T) {
	s, _, _ := jobHTTPFixture(t, nil)
	wBad := httptest.NewRecorder()
	s.ServeHTTP(wBad, httptest.NewRequest(http.MethodPost, "/v1/session/login", strings.NewReader(`{"token":"wrong"}`)))
	if wBad.Code != http.StatusUnauthorized || len(wBad.Result().Cookies()) != 0 {
		t.Fatalf("invalid login = %d, cookies = %v", wBad.Code, wBad.Result().Cookies())
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/session/login", strings.NewReader(`{"token":"token"}`)))
	if w.Code != http.StatusOK || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login = %d, cookies = %v", w.Code, w.Result().Cookies())
	}
	c := w.Result().Cookies()[0]
	if c.Name != sessionCookieName || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure {
		t.Fatalf("unexpected cookie %+v", c)
	}

	wTLS := httptest.NewRecorder()
	reqTLS := httptest.NewRequest(http.MethodPost, "/v1/session/login", strings.NewReader(`{"token":"token"}`))
	reqTLS.Header.Set("X-Forwarded-Proto", "https")
	s.ServeHTTP(wTLS, reqTLS)
	if wTLS.Code != http.StatusOK || !wTLS.Result().Cookies()[0].Secure {
		t.Fatalf("expected Secure cookie over TLS: %+v", wTLS.Result().Cookies())
	}

	wSess := httptest.NewRecorder()
	reqSess := httptest.NewRequest(http.MethodGet, "/v1/session", nil)
	reqSess.AddCookie(c)
	s.ServeHTTP(wSess, reqSess)
	var sess map[string]any
	if wSess.Code != http.StatusOK || json.Unmarshal(wSess.Body.Bytes(), &sess) != nil || sess["authenticated"] != true || sess["csrf_token"] == "" {
		t.Fatalf("unexpected session: %d %s", wSess.Code, wSess.Body.String())
	}
}

func TestCookieAuthRequiresCSRFOnPost(t *testing.T) {
	s, _, repo := jobHTTPFixture(t, nil)
	wLogin := httptest.NewRecorder()
	s.ServeHTTP(wLogin, httptest.NewRequest(http.MethodPost, "/v1/session/login", strings.NewReader(`{"token":"token"}`)))
	cookie := wLogin.Result().Cookies()[0]

	wSess := httptest.NewRecorder()
	reqSess := httptest.NewRequest(http.MethodGet, "/v1/session", nil)
	reqSess.AddCookie(cookie)
	s.ServeHTTP(wSess, reqSess)
	var sess struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(wSess.Body.Bytes(), &sess); err != nil || sess.CSRFToken == "" {
		t.Fatalf("session decode: %v", err)
	}

	wGet := httptest.NewRecorder()
	reqGet := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	reqGet.AddCookie(cookie)
	s.ServeHTTP(wGet, reqGet)
	if wGet.Code != http.StatusOK {
		t.Fatalf("cookie GET /v1/jobs = %d", wGet.Code)
	}

	specBytes, _ := json.Marshal(jobs.Spec{Objective: "csrf test", Repo: jobs.RepoSpec{Path: repo}})
	for _, csrf := range []string{"", "wrong-csrf"} {
		wPost := httptest.NewRecorder()
		reqPost := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(string(specBytes)))
		reqPost.AddCookie(cookie)
		if csrf != "" {
			reqPost.Header.Set(csrfHeaderName, csrf)
		}
		s.ServeHTTP(wPost, reqPost)
		if wPost.Code != http.StatusForbidden {
			t.Fatalf("POST /v1/jobs with csrf=%q = %d, want 403", csrf, wPost.Code)
		}
	}

	wOK := httptest.NewRecorder()
	reqOK := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(string(specBytes)))
	reqOK.AddCookie(cookie)
	reqOK.Header.Set(csrfHeaderName, sess.CSRFToken)
	s.ServeHTTP(wOK, reqOK)
	if wOK.Code != http.StatusCreated {
		t.Fatalf("POST /v1/jobs with valid CSRF = %d: %s", wOK.Code, wOK.Body.String())
	}
	var created struct {
		Job jobs.Job `json:"job"`
	}
	if err := json.Unmarshal(wOK.Body.Bytes(), &created); err != nil || created.Job.ID == "" {
		t.Fatalf("decode created job: %v", err)
	}
	wDelNoCSRF := httptest.NewRecorder()
	reqDelNoCSRF := httptest.NewRequest(http.MethodDelete, "/v1/jobs/"+created.Job.ID, nil)
	reqDelNoCSRF.AddCookie(cookie)
	s.ServeHTTP(wDelNoCSRF, reqDelNoCSRF)
	if wDelNoCSRF.Code != http.StatusForbidden {
		t.Fatalf("DELETE /v1/jobs/%s without CSRF = %d, want 403", created.Job.ID, wDelNoCSRF.Code)
	}
	wDelOK := httptest.NewRecorder()
	reqDelOK := httptest.NewRequest(http.MethodDelete, "/v1/jobs/"+created.Job.ID, nil)
	reqDelOK.AddCookie(cookie)
	reqDelOK.Header.Set(csrfHeaderName, sess.CSRFToken)
	s.ServeHTTP(wDelOK, reqDelOK)
	if wDelOK.Code != http.StatusAccepted {
		t.Fatalf("DELETE /v1/jobs/%s with CSRF = %d, want 202", created.Job.ID, wDelOK.Code)
	}
}

func TestBearerStillWorks(t *testing.T) {
	s, _, repo := jobHTTPFixture(t, nil)
	jobHTTP(t, s, http.MethodGet, "/v1/status", "", http.StatusOK)
	jobHTTP(t, s, http.MethodGet, "/v1/session", "", http.StatusOK)
	if id := createHTTPJob(t, s, repo, false); id == "" {
		t.Fatal("expected non-empty job id with bearer auth and no CSRF header")
	}
}

func TestUIBearerStillWorks(t *testing.T) { TestBearerStillWorks(t) }

func TestHashedTokenAuthenticatesBearerOnly(t *testing.T) {
	const raw = "per-job-bearer-token"
	hashed := HashToken(raw)
	if !strings.HasPrefix(hashed, TokenHashPrefix) || strings.Contains(hashed, raw) {
		t.Fatalf("HashToken = %q", hashed)
	}
	s, err := New(Options{Host: sessionOnlyHost{}, Token: hashed})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, tc := range []struct {
		bearer string
		want   int
	}{{raw, http.StatusOK}, {hashed, http.StatusUnauthorized}, {"wrong", http.StatusUnauthorized}} {
		req := httptest.NewRequest(http.MethodGet, "/v1/session", nil)
		req.Header.Set("Authorization", "Bearer "+tc.bearer)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("bearer %q: status %d, want %d", tc.bearer, w.Code, tc.want)
		}
	}
	for body, want := range map[string]int{`{"token":"` + raw + `"}`: http.StatusOK, `{"token":"` + hashed + `"}`: http.StatusUnauthorized} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/session/login", strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("login %s: status %d, want %d", body, w.Code, want)
		}
	}
	if _, err := New(Options{Host: sessionOnlyHost{}, Token: TokenHashPrefix + "nothex"}); err == nil {
		t.Fatal("malformed token hash accepted")
	}
}
