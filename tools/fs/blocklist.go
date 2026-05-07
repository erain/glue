package fs

import (
	"path/filepath"
	"strings"
)

// Blocklist is an ordered list of glob patterns matched against paths
// the model asks to read. The matcher is three-way: whole-path, basename,
// and per-component, all case-insensitive — so a model that types
// `secrets/foo.txt` is rejected by the `secrets` pattern even though
// the basename is `foo.txt`.
//
// Patterns use Go's filepath.Match semantics (`*`, `?`, character
// classes). Add deployment-specific extensions via Merge; you cannot
// subtract a default.
type Blocklist []string

// Default returns the built-in patterns: secret-shaped dotfiles, SSH key
// material, cloud credential bundles, and "*secret*" naming. The list
// targets files that PRs accidentally add and that an agent could be
// tricked into quoting into a public review comment.
func Default() Blocklist {
	return Blocklist{
		// Environment / secret bag dotfiles
		".env",
		".env.*",
		".envrc",
		".npmrc",
		".netrc",
		".pgpass",

		// SSH / key material
		"id_rsa", "id_rsa.*",
		"id_ed25519", "id_ed25519.*",
		"id_dsa", "id_dsa.*",
		"id_ecdsa", "id_ecdsa.*",
		"*.pem",
		"*.key",
		"*.p12",
		"*.pfx",
		"*.jks",

		// Cloud / service account credentials
		"credentials",
		"credentials.json",
		"service-account*.json",
		"client-secret*.json",
		"*.kubeconfig",

		// Generic "secret" naming
		"*_secret*",
		"*_secrets*",
		"*.secret",
		"*.secrets",
		"secret.*",
		"secrets.*",
		"secrets",

		// AWS CLI / GCloud / Azure credential dirs
		".aws",
		".gcloud",
		".azure",
	}
}

// Merge returns a new Blocklist that appends extras to the receiver,
// trimming and deduping. The receiver is not mutated.
//
// Use this when a deployment wants to add patterns: pass the user
// input through Merge to layer it on top of Default(). Passing no
// extras yields the receiver unchanged (after dedup).
func (b Blocklist) Merge(extras ...string) Blocklist {
	out := make(Blocklist, 0, len(b)+len(extras))
	seen := map[string]struct{}{}
	for _, p := range b {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, p := range extras {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// Match reports whether rel is blocked by any pattern in the list. The
// returned pattern names which entry matched, for use in error messages.
//
// rel is matched three ways: as a whole path, as a basename, and as
// each path component. All matches are case-insensitive on the
// basename/component paths so `.ENV` and `Credentials.json` match the
// lowercase patterns.
func (b Blocklist) Match(rel string) (bool, string) {
	clean := strings.TrimSpace(rel)
	if clean == "" {
		return false, ""
	}
	clean = filepath.ToSlash(clean)
	base := filepath.Base(clean)
	parts := strings.Split(clean, "/")

	for _, pat := range b {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if ok, _ := filepath.Match(pat, clean); ok {
			return true, pat
		}
		lowPat := strings.ToLower(pat)
		if ok, _ := filepath.Match(lowPat, strings.ToLower(base)); ok {
			return true, pat
		}
		for _, p := range parts {
			if ok, _ := filepath.Match(lowPat, strings.ToLower(p)); ok {
				return true, pat
			}
		}
	}
	return false, ""
}
