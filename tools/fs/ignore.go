package fs

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// IgnoreMatcher answers ".gitignore says skip this" for paths under a
// fixed root. It implements the core of gitignore semantics — comments,
// blank lines, `!` negation with last-match-wins, `/` anchoring,
// trailing-`/` directory-only patterns, `*` / `?` / `[...]` globs, and
// `**` — reading the root's `.gitignore` plus any nested `.gitignore`
// files on the way down to a path. Per git's own rule, a file inside an
// ignored directory cannot be re-included, so walkers may prune ignored
// directories wholesale.
//
// Out of scope (rare in practice): `.git/info/exclude`, the user-global
// core.excludesFile, and trailing-backslash escapes.
type IgnoreMatcher struct {
	root string

	mu    sync.Mutex
	rules map[string][]ignoreRule // scope dir rel to root ("" = root) → parsed rules
}

// NewIgnoreMatcher returns a matcher rooted at root. `.gitignore` files
// are read lazily per directory and cached for the matcher's lifetime.
func NewIgnoreMatcher(root string) *IgnoreMatcher {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return &IgnoreMatcher{root: abs, rules: map[string][]ignoreRule{}}
}

// Ignored reports whether the slash-separated path rel (relative to the
// matcher's root) is gitignored, either directly or via an ignored
// ancestor directory. A nil matcher ignores nothing.
func (m *IgnoreMatcher) Ignored(rel string, isDir bool) bool {
	if m == nil {
		return false
	}
	rel = strings.Trim(path.Clean(filepath.ToSlash(rel)), "/")
	if rel == "" || rel == "." {
		return false
	}
	// An ignored ancestor directory ignores everything beneath it —
	// git does not allow re-inclusion below an excluded directory.
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if m.pathIgnored(strings.Join(parts[:i], "/"), true) {
			return true
		}
	}
	return m.pathIgnored(rel, isDir)
}

// pathIgnored applies every rule scope from the root down to the path's
// parent, in order, last match wins.
func (m *IgnoreMatcher) pathIgnored(rel string, isDir bool) bool {
	ignored := false
	scopes := ruleScopes(rel)
	for _, scope := range scopes {
		sub := rel
		if scope != "" {
			sub = strings.TrimPrefix(rel, scope+"/")
		}
		for _, r := range m.scopeRules(scope) {
			if r.dirOnly && !isDir {
				continue
			}
			if r.matches(sub) {
				ignored = !r.negate
			}
		}
	}
	return ignored
}

// ruleScopes lists the directories whose .gitignore files govern rel:
// "" (the root) followed by each ancestor directory, nearest last so
// deeper files take precedence in last-match-wins order.
func ruleScopes(rel string) []string {
	scopes := []string{""}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		scopes = append(scopes, strings.Join(parts[:i], "/"))
	}
	return scopes
}

func (m *IgnoreMatcher) scopeRules(scope string) []ignoreRule {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rules, ok := m.rules[scope]; ok {
		return rules
	}
	var rules []ignoreRule
	data, err := os.ReadFile(filepath.Join(m.root, filepath.FromSlash(scope), ".gitignore"))
	if err == nil {
		rules = parseIgnoreFile(string(data))
	}
	m.rules[scope] = rules
	return rules
}

type ignoreRule struct {
	re       *regexp.Regexp
	negate   bool
	dirOnly  bool
	baseOnly bool // pattern had no slash: match the basename at any depth
}

func (r ignoreRule) matches(sub string) bool {
	if r.baseOnly {
		return r.re.MatchString(path.Base(sub))
	}
	return r.re.MatchString(sub)
}

func parseIgnoreFile(content string) []ignoreRule {
	var rules []ignoreRule
	for _, line := range strings.Split(content, "\n") {
		if r, ok := parseIgnorePattern(line); ok {
			rules = append(rules, r)
		}
	}
	return rules
}

func parseIgnorePattern(line string) (ignoreRule, bool) {
	// Trailing spaces are insignificant unless escaped; we take the
	// simple path and trim them.
	line = strings.TrimRight(line, " \t\r")
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	rule := ignoreRule{}
	if strings.HasPrefix(line, "!") {
		rule.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		rule.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	// A slash anywhere (now that the trailing one is gone) anchors the
	// pattern to the .gitignore's own directory.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return ignoreRule{}, false
	}
	rule.baseOnly = !anchored
	re, err := regexp.Compile("^" + globToRegexp(line) + "$")
	if err != nil {
		return ignoreRule{}, false
	}
	rule.re = re
	return rule, true
}

// globToRegexp converts one gitignore glob to a regexp fragment.
// `**` crosses directory separators; `*` and `?` do not; character
// classes pass through.
func globToRegexp(glob string) string {
	var b strings.Builder
	i := 0
	for i < len(glob) {
		c := glob[i]
		switch c {
		case '*':
			if strings.HasPrefix(glob[i:], "**/") {
				// "**/foo" also matches "foo" at the scope root.
				b.WriteString(`(?:[^/]*/)*`)
				i += 3
				continue
			}
			if glob[i:] == "**" {
				b.WriteString(`.*`)
				i += 2
				continue
			}
			b.WriteString(`[^/]*`)
			i++
		case '?':
			b.WriteString(`[^/]`)
			i++
		case '[':
			// Pass a character class through, converting leading ! to ^.
			end := strings.IndexByte(glob[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				i++
				continue
			}
			class := glob[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end + 2
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	return b.String()
}
