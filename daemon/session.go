// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package daemon

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

const (
	sessionCookieName = "glue_session"
	csrfHeaderName    = "X-Glue-CSRF"
)

// TokenHashPrefix marks a token value that holds only the SHA-256 of the
// bearer token. A daemon given such a value can verify the token without
// ever holding it, so a token file on shared disk is useless to read.
const TokenHashPrefix = "sha256:"

// HashToken returns the TokenHashPrefix form of token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return TokenHashPrefix + hex.EncodeToString(sum[:])
}

// tokenDigest returns the SHA-256 digest that a configured token value
// stands for: the decoded hash for TokenHashPrefix values, else the hash
// of the raw token.
func tokenDigest(value string) ([]byte, error) {
	if h, ok := strings.CutPrefix(value, TokenHashPrefix); ok {
		d, err := hex.DecodeString(h)
		if err != nil || len(d) != sha256.Size {
			return nil, errors.New("daemon: token hash must be sha256: followed by 64 hex characters")
		}
		return d, nil
	}
	sum := sha256.Sum256([]byte(value))
	return sum[:], nil
}

// tokenMatches reports whether presented is the bearer token.
func (s *Server) tokenMatches(presented string) bool {
	sum := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(sum[:], s.tokenDigest) == 1
}

func (s *Server) sessionCookieValue() string {
	sum := sha256.Sum256([]byte("glue-session-v1:" + hex.EncodeToString(s.tokenDigest)))
	return hex.EncodeToString(sum[:])
}

func (s *Server) csrfToken() string {
	sum := sha256.Sum256([]byte("glue-csrf-v1:" + hex.EncodeToString(s.tokenDigest)))
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
	if !s.tokenMatches(body.Token) {
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
		if tok, ok := strings.CutPrefix(got, "Bearer "); ok && s.tokenMatches(tok) {
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
