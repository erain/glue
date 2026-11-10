// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package daemon

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	sessionCookieName = "glue_session"
	csrfHeaderName    = "X-Glue-CSRF"
)

func (s *Server) sessionCookieValue() string {
	sum := sha256.Sum256([]byte("glue-session-v1:" + s.token))
	return hex.EncodeToString(sum[:])
}

func (s *Server) csrfToken() string {
	sum := sha256.Sum256([]byte("glue-csrf-v1:" + s.token))
	return hex.EncodeToString(sum[:])
}

func (s *Server) handleSessionLogin(w http.ResponseWriter, r *http.Request) {
	if !jobMethod(w, r, http.MethodPost) {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !jobBody(w, r, &body) {
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.token)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token", false)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    s.sessionCookieValue(),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authenticated": true, "csrf_token": s.csrfToken()})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if !jobMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authenticated": true, "csrf_token": s.csrfToken()})
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if got := r.Header.Get("Authorization"); got != "" {
		if subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+s.token)) == 1 {
			return true
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token", false)
		return false
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.sessionCookieValue())) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token", false)
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeaderName)), []byte(s.csrfToken())) != 1 {
			writeError(w, http.StatusForbidden, "forbidden", "missing or invalid CSRF token", false)
			return false
		}
	}
	return true
}
