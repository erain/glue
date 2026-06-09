package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Constants mirroring the upstream Codex CLI. See ADR-0006.
const (
	// ClientID is the OAuth client id baked into OpenAI's Hydra
	// allowlist for the Codex CLI. Third-party tools that piggyback on
	// the same subscription auth must send this exact value or refresh
	// is rejected.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// DefaultRefreshURL is the OAuth token endpoint used for refresh.
	// Overridden by CODEX_REFRESH_TOKEN_URL_OVERRIDE for tests.
	DefaultRefreshURL = "https://auth.openai.com/oauth/token"

	// RefreshCadence is how long after a successful refresh we
	// proactively refresh again, matching upstream Codex CLI's 8-day
	// window.
	RefreshCadence = 8 * 24 * time.Hour

	// authFileBasename is the upstream-defined filename.
	authFileBasename = "auth.json"

	// Environment overrides honored when resolving the auth file path.
	envGlueOverride = "GLUE_CODEX_AUTH"
	envCodexHome    = "CODEX_HOME"
	envRefreshURL   = "CODEX_REFRESH_TOKEN_URL_OVERRIDE"
)

// HTTPDoer is the subset of *http.Client used for refresh. Exposed so
// tests can inject an httptest server.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Manager owns token loading, refresh, and atomic write-back. It is
// safe for concurrent use; refresh is serialized by an internal mutex.
type Manager struct {
	// HTTPClient performs refresh requests. Nil means http.DefaultClient.
	HTTPClient HTTPDoer
	// RefreshURLOverride is used when non-empty; otherwise the env
	// override or DefaultRefreshURL applies.
	RefreshURLOverride string
	// PathOverride is the absolute auth.json path; when empty the
	// standard resolution applies.
	PathOverride string
	// Now is used in tests to control time. Nil means time.Now.
	Now func() time.Time

	mu     sync.Mutex
	path   string // resolved on first Load
	apiKey *string
	tokens *Tokens
}

// NewManager returns a Manager with default behavior.
func NewManager() *Manager { return &Manager{} }

// AuthFilePath resolves the location of auth.json without reading it.
// Resolution order: PathOverride > $GLUE_CODEX_AUTH > $CODEX_HOME/auth.json
// > ~/.codex/auth.json. The returned path may or may not exist.
func (m *Manager) AuthFilePath() (string, error) {
	if m.PathOverride != "" {
		return m.PathOverride, nil
	}
	if p := os.Getenv(envGlueOverride); p != "" {
		return p, nil
	}
	if home := os.Getenv(envCodexHome); home != "" {
		return filepath.Join(home, authFileBasename), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("auth: resolve $HOME: %w", err)
	}
	return filepath.Join(home, ".codex", authFileBasename), nil
}

// LoadTokens reads auth.json and parses it into Tokens. It returns
// ErrNoAuthFile (wrapped with the resolved path) if the file does not
// exist. The Manager caches the loaded tokens for subsequent
// EnsureFresh calls.
func (m *Manager) LoadTokens(ctx context.Context) (*Tokens, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadLocked()
}

func (m *Manager) loadLocked() (*Tokens, error) {
	path, err := m.AuthFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w (looked at %s)", ErrNoAuthFile, path)
		}
		return nil, fmt.Errorf("auth: read %s: %w", path, err)
	}
	// Pull OPENAI_API_KEY raw so we preserve it on write-back.
	var raw authFile
	_ = json.Unmarshal(data, &raw)
	t, err := ParseAuthFile(data)
	if err != nil {
		return nil, err
	}
	m.path = path
	m.tokens = t
	m.apiKey = raw.OpenAIAPIKey
	return cloneTokens(t), nil
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// EnsureFresh returns a Tokens whose AccessToken is not expired and
// whose LastRefresh is within RefreshCadence of now. When the cached
// tokens are stale it calls Refresh and atomically writes the result
// back to disk.
//
// The argument may be nil; in that case LoadTokens is invoked first.
func (m *Manager) EnsureFresh(ctx context.Context, in *Tokens) (*Tokens, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if in == nil && m.tokens == nil {
		if _, err := m.loadLocked(); err != nil {
			return nil, err
		}
	} else if in != nil {
		m.tokens = cloneTokens(in)
	}
	if m.tokens == nil {
		return nil, ErrNoAuthFile
	}
	if !m.isStale(m.tokens) {
		return cloneTokens(m.tokens), nil
	}
	if m.tokens.RefreshToken == "" {
		return nil, fmt.Errorf("auth: cannot refresh: no refresh_token in auth.json")
	}
	resp, err := m.refreshLocked(ctx, m.tokens.RefreshToken)
	if err != nil {
		return nil, err
	}
	merged := mergeRefresh(m.tokens, resp)
	merged.LastRefresh = m.now().UTC()
	if err := m.writeLocked(merged); err != nil {
		return nil, err
	}
	m.tokens = merged
	return cloneTokens(merged), nil
}

func (m *Manager) isStale(t *Tokens) bool {
	now := m.now()
	if exp, err := AccessTokenExpiry(t.AccessToken); err == nil {
		if !now.Before(exp) {
			return true
		}
	}
	if t.LastRefresh.IsZero() {
		return true
	}
	return now.Sub(t.LastRefresh) >= RefreshCadence
}

// RefreshResponse is the decoded body of a successful refresh.
type RefreshResponse struct {
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// Refresh exchanges a refresh_token for new tokens. It is safe to call
// without holding the Manager (no caching side-effects); EnsureFresh
// is the usual entry point.
func (m *Manager) Refresh(ctx context.Context, refreshToken string) (RefreshResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshLocked(ctx, refreshToken)
}

func (m *Manager) refreshLocked(ctx context.Context, refreshToken string) (RefreshResponse, error) {
	url := m.refreshURL()
	body, _ := json.Marshal(map[string]string{
		"client_id":     ClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return RefreshResponse{}, fmt.Errorf("auth: build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := m.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	httpResp, err := client.Do(req)
	if err != nil {
		return RefreshResponse{}, fmt.Errorf("auth: refresh transport: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == http.StatusOK {
		var rr RefreshResponse
		if err := json.NewDecoder(httpResp.Body).Decode(&rr); err != nil {
			return RefreshResponse{}, fmt.Errorf("auth: decode refresh body: %w", err)
		}
		return rr, nil
	}

	// Non-2xx: classify.
	respBody, _ := io.ReadAll(httpResp.Body)
	var errBody struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(respBody, &errBody)

	switch errBody.Error {
	case "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
		return RefreshResponse{}, fmt.Errorf("%w: %s", ErrRefreshPermanent, errBody.Error)
	}
	if httpResp.StatusCode == http.StatusUnauthorized || httpResp.StatusCode == http.StatusForbidden {
		// Treat unclassified 401/403 from the auth endpoint as permanent.
		return RefreshResponse{}, fmt.Errorf("%w: http %d", ErrRefreshPermanent, httpResp.StatusCode)
	}
	return RefreshResponse{}, fmt.Errorf("auth: refresh failed: http %d (%s)", httpResp.StatusCode, errBody.Error)
}

func (m *Manager) refreshURL() string {
	if m.RefreshURLOverride != "" {
		return m.RefreshURLOverride
	}
	if v := os.Getenv(envRefreshURL); v != "" {
		return v
	}
	return DefaultRefreshURL
}

// mergeRefresh applies a RefreshResponse to a Tokens, preserving
// previous values for any field the upstream omits.
func mergeRefresh(prev *Tokens, r RefreshResponse) *Tokens {
	out := cloneTokens(prev)
	if r.IDToken != "" {
		out.IDToken = r.IDToken
		// Re-extract account id when id_token rotates.
		if id, err := AccountIDFromIDToken(r.IDToken); err == nil {
			out.AccountID = id
		}
	}
	if r.AccessToken != "" {
		out.AccessToken = r.AccessToken
	}
	if r.RefreshToken != "" {
		out.RefreshToken = r.RefreshToken
	}
	return out
}

// writeLocked persists tokens to disk via temp-file + atomic rename.
func (m *Manager) writeLocked(t *Tokens) error {
	if m.path == "" {
		p, err := m.AuthFilePath()
		if err != nil {
			return err
		}
		m.path = p
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: mkdir %s: %w", dir, err)
	}
	data, err := encodeAuthFile(t, m.apiKey)
	if err != nil {
		return fmt.Errorf("auth: encode: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".auth.json.*")
	if err != nil {
		return fmt.Errorf("auth: tempfile: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("auth: write tempfile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("auth: chmod tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("auth: close tempfile: %w", err)
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		cleanup()
		return fmt.Errorf("auth: rename to %s: %w", m.path, err)
	}
	return nil
}

func cloneTokens(t *Tokens) *Tokens {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
