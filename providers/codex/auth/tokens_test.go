package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// makeJWT builds an unsigned (alg=none) JWT with the given payload
// claims. The signature segment is intentionally empty — the package
// never verifies signatures, only decodes claims.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc(header) + "." + enc(payload) + "."
}

func TestAccessTokenExpiry_Happy(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Truncate(time.Second).UTC()
	tok := makeJWT(t, map[string]any{"exp": exp.Unix()})
	got, err := AccessTokenExpiry(tok)
	if err != nil {
		t.Fatalf("AccessTokenExpiry: %v", err)
	}
	if !got.Equal(exp) {
		t.Fatalf("expiry mismatch: got %s want %s", got, exp)
	}
}

func TestAccessTokenExpiry_Errors(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"no-payload": "abcdef",
		"bad-base64": "abc.@@@.def",
		"no-exp":     makeJWT(t, map[string]any{"sub": "x"}),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := AccessTokenExpiry(tok); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestAccountIDFromIDToken(t *testing.T) {
	cases := map[string]map[string]any{
		"namespaced": {"https://api.openai.com/auth.chatgpt_account_id": "acct-1"},
		"short":      {"chatgpt_account_id": "acct-2"},
		"nested":     {"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-3"}},
	}
	want := map[string]string{"namespaced": "acct-1", "short": "acct-2", "nested": "acct-3"}
	for name, claims := range cases {
		t.Run(name, func(t *testing.T) {
			tok := makeJWT(t, claims)
			id, err := AccountIDFromIDToken(tok)
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if id != want[name] {
				t.Fatalf("got %q want %q", id, want[name])
			}
		})
	}
}

func TestAccountIDFromIDToken_Missing(t *testing.T) {
	tok := makeJWT(t, map[string]any{"sub": "user"})
	if _, err := AccountIDFromIDToken(tok); err == nil {
		t.Fatal("expected error when claim missing")
	}
}

func TestParseAuthFile_Roundtrip(t *testing.T) {
	idToken := makeJWT(t, map[string]any{"chatgpt_account_id": "acct-7"})
	src := map[string]any{
		"OPENAI_API_KEY": "sk-omitted",
		"tokens": map[string]any{
			"id_token":      idToken,
			"access_token":  "atk",
			"refresh_token": "rtk",
			"account_id":    "acct-from-file",
		},
		"last_refresh": "2026-01-02T03:04:05Z",
	}
	raw, _ := json.Marshal(src)
	got, err := ParseAuthFile(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.IDToken != idToken {
		t.Errorf("id_token: %q", got.IDToken)
	}
	if got.AccessToken != "atk" || got.RefreshToken != "rtk" {
		t.Errorf("token mismatch: %+v", got)
	}
	if got.AccountID != "acct-from-file" {
		t.Errorf("account_id: %q (expected file value)", got.AccountID)
	}
	want, _ := time.Parse(time.RFC3339, "2026-01-02T03:04:05Z")
	if !got.LastRefresh.Equal(want) {
		t.Errorf("last_refresh: %s", got.LastRefresh)
	}
}

func TestParseAuthFile_FallsBackToIDTokenForAccountID(t *testing.T) {
	idToken := makeJWT(t, map[string]any{"chatgpt_account_id": "from-id-token"})
	raw, _ := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"id_token":     idToken,
			"access_token": "x",
		},
	})
	got, err := ParseAuthFile(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.AccountID != "from-id-token" {
		t.Errorf("account_id: %q want from-id-token", got.AccountID)
	}
}

func TestParseAuthFile_Malformed(t *testing.T) {
	if _, err := ParseAuthFile([]byte(`{"OPENAI_API_KEY":null}`)); !errors.Is(err, ErrMalformedAuthFile) {
		t.Fatalf("want ErrMalformedAuthFile, got %v", err)
	}
	if _, err := ParseAuthFile([]byte(`{`)); err == nil {
		t.Fatal("want JSON parse error")
	}
}

func TestSafeStringRedacts(t *testing.T) {
	tok := &Tokens{IDToken: "id", AccessToken: "secret", RefreshToken: "rtk"}
	got := tok.SafeString()
	if strings.Contains(got, "secret") || strings.Contains(got, "rtk") {
		t.Fatalf("SafeString leaked tokens: %s", got)
	}
}
