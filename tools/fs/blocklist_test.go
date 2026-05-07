package fs

import "testing"

func TestBlocklistDefault_RejectsCommonSecrets(t *testing.T) {
	bl := Default()
	hits := []string{
		".env",
		".env.production",
		"id_rsa",
		"id_ed25519.pub",
		"server.pem",
		"deploy.key",
		"credentials.json",
		"service-account-prod.json",
		"deep/path/to/.env",
		"app/secrets/db.yaml",
		".aws/credentials",
		".AWS/CREDENTIALS",
	}
	for _, p := range hits {
		t.Run("hit:"+p, func(t *testing.T) {
			ok, pat := bl.Match(p)
			if !ok {
				t.Fatalf("expected %q to match a default pattern; pattern=%q", p, pat)
			}
		})
	}
}

func TestBlocklistDefault_AllowsOrdinaryPaths(t *testing.T) {
	bl := Default()
	ok := []string{
		"main.go",
		"docs/design.md",
		"README.md",
		"src/handler.go",
		"package.json",
	}
	for _, p := range ok {
		t.Run("allow:"+p, func(t *testing.T) {
			if blocked, pat := bl.Match(p); blocked {
				t.Fatalf("expected %q to be allowed; matched pattern %q", p, pat)
			}
		})
	}
}

func TestBlocklistMerge_AddsExtras(t *testing.T) {
	bl := Default().Merge("vault.json", "vault.json", "  ", "*.token")
	ok, pat := bl.Match("vault.json")
	if !ok || pat != "vault.json" {
		t.Fatalf("merged extra not active: ok=%v pat=%q", ok, pat)
	}
	ok, pat = bl.Match("session.token")
	if !ok || pat != "*.token" {
		t.Fatalf("merged glob not active: ok=%v pat=%q", ok, pat)
	}
}

func TestBlocklistMerge_DoesNotMutateReceiver(t *testing.T) {
	bl := Default()
	before := len(bl)
	_ = bl.Merge("foo")
	if len(bl) != before {
		t.Fatalf("Merge mutated receiver: before=%d after=%d", before, len(bl))
	}
}

func TestBlocklistMatch_EmptyPath(t *testing.T) {
	if ok, _ := Default().Match(""); ok {
		t.Fatal("empty path must not match")
	}
}
