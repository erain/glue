package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeFile writes data to path with 0o600.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeAuthFile writes an auth.json with the given tokens and last_refresh
// (RFC3339 or empty for none).
func writeAuthFile(t *testing.T, path string, tk *Tokens, lastRefresh string) {
	t.Helper()
	body := map[string]any{
		"tokens": map[string]any{
			"id_token":      tk.IDToken,
			"access_token":  tk.AccessToken,
			"refresh_token": tk.RefreshToken,
			"account_id":    tk.AccountID,
		},
	}
	if lastRefresh != "" {
		body["last_refresh"] = lastRefresh
	}
	raw, _ := json.MarshalIndent(body, "", "  ")
	writeFile(t, path, raw)
}

// freshAccessToken returns a JWT with an exp in the future.
func freshAccessToken(t *testing.T) string {
	t.Helper()
	return makeJWT(t, map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})
}

// expiredAccessToken returns a JWT with an exp in the past.
func expiredAccessToken(t *testing.T) string {
	t.Helper()
	return makeJWT(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})
}

// freshIDToken returns an id_token with an account_id claim.
func freshIDToken(t *testing.T, account string) string {
	t.Helper()
	return makeJWT(t, map[string]any{"chatgpt_account_id": account})
}

func TestAuthFilePath_Resolution(t *testing.T) {
	t.Run("path-override-wins", func(t *testing.T) {
		t.Setenv(envGlueOverride, "/env/override")
		t.Setenv(envCodexHome, "/codex/home")
		m := &Manager{PathOverride: "/explicit/path"}
		got, _ := m.AuthFilePath()
		if got != "/explicit/path" {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("env-override-wins-over-codex-home", func(t *testing.T) {
		t.Setenv(envGlueOverride, "/env/override")
		t.Setenv(envCodexHome, "/codex/home")
		m := &Manager{}
		got, _ := m.AuthFilePath()
		if got != "/env/override" {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("codex-home-wins-over-home", func(t *testing.T) {
		t.Setenv(envGlueOverride, "")
		t.Setenv(envCodexHome, "/codex/home")
		m := &Manager{}
		got, _ := m.AuthFilePath()
		want := filepath.Join("/codex/home", "auth.json")
		if got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	})
	t.Run("home-default", func(t *testing.T) {
		t.Setenv(envGlueOverride, "")
		t.Setenv(envCodexHome, "")
		home := t.TempDir()
		t.Setenv(homeEnv(), home)
		m := &Manager{}
		got, _ := m.AuthFilePath()
		want := filepath.Join(home, ".codex", "auth.json")
		if got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	})
}

func homeEnv() string {
	if runtime.GOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}

func TestLoadTokens_NoFile(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{PathOverride: filepath.Join(dir, "missing.json")}
	_, err := m.LoadTokens(context.Background())
	if !errors.Is(err, ErrNoAuthFile) {
		t.Fatalf("want ErrNoAuthFile, got %v", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error should mention resolved path; got %q", err)
	}
}

func TestLoadTokens_HappyAndCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	tk := &Tokens{
		IDToken:      freshIDToken(t, "acct-X"),
		AccessToken:  freshAccessToken(t),
		RefreshToken: "rtk",
		AccountID:    "acct-X",
	}
	writeAuthFile(t, path, tk, "2026-05-15T00:00:00Z")

	m := &Manager{PathOverride: path}
	out, err := m.LoadTokens(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if out.AccessToken != tk.AccessToken || out.RefreshToken != "rtk" || out.AccountID != "acct-X" {
		t.Fatalf("loaded mismatch: %+v", out)
	}
	if out.LastRefresh.IsZero() {
		t.Errorf("expected last_refresh populated")
	}
}

func TestEnsureFresh_NotStale_NoCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	tk := &Tokens{
		AccessToken:  freshAccessToken(t),
		RefreshToken: "rtk",
		AccountID:    "acct",
	}
	writeAuthFile(t, path, tk, time.Now().UTC().Format(time.RFC3339))

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls++
	}))
	defer srv.Close()

	m := &Manager{PathOverride: path, RefreshURLOverride: srv.URL}
	out, err := m.EnsureFresh(context.Background(), nil)
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no refresh, got %d calls", calls)
	}
	if out.AccessToken != tk.AccessToken {
		t.Errorf("token rotated unexpectedly")
	}
}

func TestEnsureFresh_JWTExpired_Refreshes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	expired := expiredAccessToken(t)
	tk := &Tokens{
		IDToken:      freshIDToken(t, "acct"),
		AccessToken:  expired,
		RefreshToken: "old-rtk",
		AccountID:    "acct",
	}
	writeAuthFile(t, path, tk, time.Now().UTC().Format(time.RFC3339))

	newAccess := freshAccessToken(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertRefreshRequest(t, r)
		_ = json.NewEncoder(w).Encode(RefreshResponse{AccessToken: newAccess, RefreshToken: "new-rtk"})
	}))
	defer srv.Close()

	m := &Manager{PathOverride: path, RefreshURLOverride: srv.URL}
	out, err := m.EnsureFresh(context.Background(), nil)
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if out.AccessToken != newAccess {
		t.Errorf("access not rotated")
	}
	if out.RefreshToken != "new-rtk" {
		t.Errorf("refresh not rotated")
	}
	// File rewritten?
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o want 0600", st.Mode().Perm())
	}
	on, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(on), newAccess) {
		t.Errorf("file does not contain rotated access token")
	}
	if !strings.Contains(string(on), "new-rtk") {
		t.Errorf("file does not contain rotated refresh token")
	}
}

func TestEnsureFresh_StaleCadence_Refreshes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	tk := &Tokens{
		AccessToken:  freshAccessToken(t), // not JWT-expired
		RefreshToken: "rtk-1",
	}
	old := time.Now().Add(-9 * 24 * time.Hour).UTC().Format(time.RFC3339)
	writeAuthFile(t, path, tk, old)

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assertRefreshRequest(t, r)
		_ = json.NewEncoder(w).Encode(RefreshResponse{AccessToken: freshAccessToken(t)})
	}))
	defer srv.Close()

	m := &Manager{PathOverride: path, RefreshURLOverride: srv.URL}
	if _, err := m.EnsureFresh(context.Background(), nil); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if !called {
		t.Fatal("expected refresh due to >8d cadence")
	}
}

func TestRefresh_PartialResponsePreservesRefreshToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	tk := &Tokens{
		AccessToken:  expiredAccessToken(t),
		RefreshToken: "old-rtk",
	}
	writeAuthFile(t, path, tk, time.Now().UTC().Format(time.RFC3339))

	newAccess := freshAccessToken(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Only access_token returned; no refresh_token.
		_ = json.NewEncoder(w).Encode(RefreshResponse{AccessToken: newAccess})
	}))
	defer srv.Close()

	m := &Manager{PathOverride: path, RefreshURLOverride: srv.URL}
	out, err := m.EnsureFresh(context.Background(), nil)
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if out.AccessToken != newAccess {
		t.Errorf("access not rotated")
	}
	if out.RefreshToken != "old-rtk" {
		t.Errorf("refresh should be preserved when omitted; got %q", out.RefreshToken)
	}
}

func TestRefresh_PermanentFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	tk := &Tokens{AccessToken: expiredAccessToken(t), RefreshToken: "rtk"}
	writeAuthFile(t, path, tk, time.Now().UTC().Format(time.RFC3339))

	cases := []string{"refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated"}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
			}))
			defer srv.Close()
			m := &Manager{PathOverride: path, RefreshURLOverride: srv.URL}
			_, err := m.EnsureFresh(context.Background(), nil)
			if !IsPermanentRefreshFailure(err) {
				t.Fatalf("want permanent, got %v", err)
			}
		})
	}
}

func TestRefresh_TransientFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	tk := &Tokens{AccessToken: expiredAccessToken(t), RefreshToken: "rtk"}
	writeAuthFile(t, path, tk, time.Now().UTC().Format(time.RFC3339))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"server_error"}`)
	}))
	defer srv.Close()

	m := &Manager{PathOverride: path, RefreshURLOverride: srv.URL}
	_, err := m.EnsureFresh(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if IsPermanentRefreshFailure(err) {
		t.Fatalf("5xx should not be classified permanent: %v", err)
	}
}

func TestRefresh_UnauthorizedClassifiedPermanent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	tk := &Tokens{AccessToken: expiredAccessToken(t), RefreshToken: "rtk"}
	writeAuthFile(t, path, tk, time.Now().UTC().Format(time.RFC3339))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	m := &Manager{PathOverride: path, RefreshURLOverride: srv.URL}
	_, err := m.EnsureFresh(context.Background(), nil)
	if !IsPermanentRefreshFailure(err) {
		t.Fatalf("want permanent for unclassified 401, got %v", err)
	}
}

func TestRefreshURL_EnvOverride(t *testing.T) {
	t.Setenv(envRefreshURL, "https://override.example/oauth")
	m := &Manager{}
	if got := m.refreshURL(); got != "https://override.example/oauth" {
		t.Fatalf("env override ignored: %q", got)
	}
	m.RefreshURLOverride = "https://field.example/oauth"
	if got := m.refreshURL(); got != "https://field.example/oauth" {
		t.Fatalf("field override should win: %q", got)
	}
}

func TestEnsureFresh_NoRefreshToken_Errors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	tk := &Tokens{AccessToken: expiredAccessToken(t)} // no refresh
	writeAuthFile(t, path, tk, "")
	m := &Manager{PathOverride: path}
	_, err := m.EnsureFresh(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "no refresh_token") {
		t.Fatalf("want refresh-missing error, got %v", err)
	}
}

func TestEnsureFresh_PreservesOpenAIAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	// Write a file that includes OPENAI_API_KEY; we should preserve it
	// across refresh.
	full := map[string]any{
		"OPENAI_API_KEY": "sk-preserved",
		"tokens": map[string]any{
			"access_token":  expiredAccessToken(t),
			"refresh_token": "rtk",
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	}
	raw, _ := json.MarshalIndent(full, "", "  ")
	writeFile(t, path, raw)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(RefreshResponse{AccessToken: freshAccessToken(t)})
	}))
	defer srv.Close()

	m := &Manager{PathOverride: path, RefreshURLOverride: srv.URL}
	if _, err := m.EnsureFresh(context.Background(), nil); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	on, _ := os.ReadFile(path)
	if !strings.Contains(string(on), "sk-preserved") {
		t.Fatalf("OPENAI_API_KEY not preserved on writeback: %s", on)
	}
}

// assertRefreshRequest verifies the request shape we send to the
// upstream OAuth endpoint matches ADR-0006 §2.
func assertRefreshRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Errorf("method = %s", r.Method)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	body, _ := io.ReadAll(r.Body)
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if got["client_id"] != ClientID {
		t.Errorf("client_id = %q", got["client_id"])
	}
	if got["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %q", got["grant_type"])
	}
	if got["refresh_token"] == "" {
		t.Errorf("refresh_token empty")
	}
}
