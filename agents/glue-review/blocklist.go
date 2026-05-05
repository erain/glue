package main

import (
	"path/filepath"
	"strings"
)

// defaultBlockedPatterns lists basename and path globs that read_file
// refuses to open. The set targets the most common secret-shaped files
// — credentials, key material, dotfiles known to carry tokens — so a
// PR that accidentally adds one cannot trick the agent into quoting
// the contents into a public review comment.
//
// Patterns use Go's filepath.Match semantics (`*`, `?`, character
// classes). They are matched against:
//   - the relative path passed to read_file
//   - each path component (so `secrets/foo.txt` matches `secrets`)
//   - the basename
//
// Whichever match wins, the path is rejected.
//
// To extend the list per-deployment, pass --blocked-paths (CLI) or
// `extra-blocked-paths` (Action input). User-supplied patterns merge
// with the defaults; you cannot subtract a default.
func defaultBlockedPatterns() []string {
	return []string{
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
		"secret.*",  // e.g. secret.json
		"secrets.*", // e.g. secrets.yaml
		"secrets",

		// AWS CLI / GCloud / Azure credential dirs
		".aws",
		".gcloud",
		".azure",
	}
}

// pathBlocked reports whether the given relative path should be
// refused. A non-empty reason string explains which pattern matched.
//
// `rel` is expected to be the path the user supplied to read_file
// (already cleaned + traversal-rejected by safeJoin upstream). The
// matcher is case-insensitive on basename and component matches to
// catch `.ENV` / `Credentials.json` variants.
func pathBlocked(rel string, patterns []string) (bool, string) {
	clean := strings.TrimSpace(rel)
	if clean == "" {
		return false, ""
	}
	clean = filepath.ToSlash(clean)
	base := filepath.Base(clean)
	parts := strings.Split(clean, "/")

	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		// 1) Whole-path glob.
		if ok, _ := filepath.Match(pat, clean); ok {
			return true, pat
		}
		// 2) Basename glob (case-insensitive — model-typed paths drift).
		if ok, _ := filepath.Match(strings.ToLower(pat), strings.ToLower(base)); ok {
			return true, pat
		}
		// 3) Each path component (so `secrets/foo` matches pattern `secrets`).
		for _, p := range parts {
			if ok, _ := filepath.Match(strings.ToLower(pat), strings.ToLower(p)); ok {
				return true, pat
			}
		}
	}
	return false, ""
}

// mergeBlocklist merges defaults with user-supplied extras, deduping
// and trimming. Empty extras yields exactly the defaults.
func mergeBlocklist(extras []string) []string {
	out := defaultBlockedPatterns()
	seen := map[string]struct{}{}
	for _, p := range out {
		seen[p] = struct{}{}
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

// splitCommaList parses a comma-separated CLI input into a non-empty
// trimmed slice. Useful for both --blocked-paths and the Action's
// `extra-blocked-paths` input.
func splitCommaList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
