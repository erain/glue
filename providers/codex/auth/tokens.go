package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Tokens is the in-memory shape of the upstream Codex CLI's auth.json
// "tokens" object plus the last-refresh timestamp.
//
// Fields are populated from auth.json and are never logged in full. Use
// SafeString for debugging output.
type Tokens struct {
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id"`
	LastRefresh  time.Time `json:"-"`
}

// SafeString returns a redacted single-line summary suitable for logs.
// It exposes only the presence of each field and the last_refresh
// timestamp.
func (t *Tokens) SafeString() string {
	if t == nil {
		return "Tokens<nil>"
	}
	mark := func(s string) string {
		if s == "" {
			return "missing"
		}
		return "set"
	}
	return fmt.Sprintf("Tokens{id_token:%s access_token:%s refresh_token:%s account_id:%s last_refresh:%s}",
		mark(t.IDToken), mark(t.AccessToken), mark(t.RefreshToken), mark(t.AccountID), t.LastRefresh.Format(time.RFC3339))
}

// authFile is the on-disk schema of upstream Codex CLI's auth.json.
// We deliberately ignore the OPENAI_API_KEY field — subscription
// transport uses tokens.access_token, not the swapped API key. See
// ADR-0006 §2.
type authFile struct {
	OpenAIAPIKey *string      `json:"OPENAI_API_KEY,omitempty"`
	Tokens       *fileTokens  `json:"tokens,omitempty"`
	LastRefresh  *fileISOTime `json:"last_refresh,omitempty"`
}

type fileTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

// fileISOTime parses the upstream "last_refresh" field which is an
// RFC3339 timestamp; missing or unparseable values become zero.
type fileISOTime time.Time

func (f *fileISOTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*f = fileISOTime(time.Time{})
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("auth.json: last_refresh: %w", err)
	}
	*f = fileISOTime(t)
	return nil
}

func (f fileISOTime) MarshalJSON() ([]byte, error) {
	t := time.Time(f)
	if t.IsZero() {
		return json.Marshal("")
	}
	return json.Marshal(t.UTC().Format(time.RFC3339))
}

// ParseAuthFile decodes the byte contents of an auth.json file into a
// Tokens struct. It returns ErrMalformedAuthFile when the file is
// missing the tokens object.
func ParseAuthFile(data []byte) (*Tokens, error) {
	var af authFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, fmt.Errorf("auth.json: %w", err)
	}
	if af.Tokens == nil {
		return nil, ErrMalformedAuthFile
	}
	t := &Tokens{
		IDToken:      af.Tokens.IDToken,
		AccessToken:  af.Tokens.AccessToken,
		RefreshToken: af.Tokens.RefreshToken,
		AccountID:    af.Tokens.AccountID,
	}
	if af.LastRefresh != nil {
		t.LastRefresh = time.Time(*af.LastRefresh)
	}
	if t.AccountID == "" {
		if id, err := AccountIDFromIDToken(t.IDToken); err == nil {
			t.AccountID = id
		}
	}
	return t, nil
}

// MarshalAuthFile serializes Tokens back to the upstream auth.json
// schema. Existing OPENAI_API_KEY values are not preserved by this
// path because the caller (encodeForWriteback) reads-modifies-writes.
func encodeAuthFile(t *Tokens, existingAPIKey *string) ([]byte, error) {
	last := fileISOTime(t.LastRefresh.UTC())
	af := authFile{
		OpenAIAPIKey: existingAPIKey,
		Tokens: &fileTokens{
			IDToken:      t.IDToken,
			AccessToken:  t.AccessToken,
			RefreshToken: t.RefreshToken,
			AccountID:    t.AccountID,
		},
		LastRefresh: &last,
	}
	return json.MarshalIndent(&af, "", "  ")
}

// AccessTokenExpiry decodes the unverified JWT payload of token and
// returns the exp claim as a time. It returns an error if token is not
// a JWT or has no exp claim.
//
// This is *unverified*. The token's signature is not checked here; the
// upstream is OpenAI and we treat the auth file's contents as
// authoritative. We use the exp only to decide whether to refresh.
func AccessTokenExpiry(token string) (time.Time, error) {
	if token == "" {
		return time.Time{}, errors.New("auth: empty token")
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, errors.New("auth: token is not a JWT (no payload segment)")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some implementations produce padded base64; tolerate it.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("auth: jwt payload base64: %w", err)
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("auth: jwt payload json: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("auth: jwt has no exp claim")
	}
	return time.Unix(claims.Exp, 0).UTC(), nil
}

// AccountIDFromIDToken extracts the ChatGPT account id from the
// unverified id_token JWT. Returns the first non-empty of the two
// claim names the upstream Codex CLI accepts:
//
//	https://api.openai.com/auth.chatgpt_account_id
//	chatgpt_account_id
func AccountIDFromIDToken(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", errors.New("auth: id_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", fmt.Errorf("auth: id_token base64: %w", err)
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("auth: id_token json: %w", err)
	}
	for _, key := range []string{
		"https://api.openai.com/auth.chatgpt_account_id",
		"chatgpt_account_id",
	} {
		if v, ok := claims[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s, nil
			}
			if m, ok := v.(map[string]any); ok {
				if s, ok := m["chatgpt_account_id"].(string); ok && s != "" {
					return s, nil
				}
			}
		}
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if s, ok := auth["chatgpt_account_id"].(string); ok && s != "" {
			return s, nil
		}
	}
	return "", errors.New("auth: id_token has no chatgpt_account_id claim")
}
