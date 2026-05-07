// Package prompts provides a small versioned-prompt loader that wraps
// an embed.FS rooted at a directory of `<name>.md` files. It exists so
// agents that A/B-test prompts (or want to roll back without rebuilding)
// don't have to re-implement the same loader each time.
//
// Typical usage:
//
//	//go:embed prompts/*.md
//	var promptFS embed.FS
//
//	cat, err := prompts.NewCatalog(promptFS, "prompts", "v2")
//	if err != nil { ... }
//	body, err := cat.Get("v1")  // or cat.Get("") for the default
package prompts

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Catalog is a frozen view over an embed.FS of `<version>.md` prompts.
// Construct with NewCatalog. Lookups are read-only and concurrency-safe.
type Catalog struct {
	fsys           fs.FS
	dir            string
	defaultVersion string
	versions       []string
}

// NewCatalog wraps fsys rooted at dir. Files matching `*.md` directly
// under dir become available versions (the file stem is the version
// name). defaultVersion must exist in the catalog or NewCatalog returns
// an error — fail-fast on misconfiguration is preferred to silent
// fallback at lookup time.
func NewCatalog(fsys fs.FS, dir, defaultVersion string) (*Catalog, error) {
	versions, err := listVersions(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("prompts: read %s: %w", dir, err)
	}
	if defaultVersion == "" {
		return nil, fmt.Errorf("prompts: default version is required (available: %s)", strings.Join(versions, ", "))
	}
	if !contains(versions, defaultVersion) {
		return nil, fmt.Errorf("prompts: default version %q not found in %s (available: %s)", defaultVersion, dir, strings.Join(versions, ", "))
	}
	return &Catalog{
		fsys:           fsys,
		dir:            dir,
		defaultVersion: defaultVersion,
		versions:       versions,
	}, nil
}

// Get returns the prompt body for the requested version. Empty version
// falls back to the default. Unknown versions return an error that
// lists every available version verbatim — silent fallback would hide
// A/B test misconfiguration.
//
// The returned body is the file content trimmed of trailing newlines
// and re-suffixed with a single newline, so callers can append further
// instructions without double-blank-line drift.
func (c *Catalog) Get(version string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("prompts: nil catalog")
	}
	if strings.TrimSpace(version) == "" {
		version = c.defaultVersion
	}
	data, err := fs.ReadFile(c.fsys, path.Join(c.dir, version+".md"))
	if err != nil {
		return "", fmt.Errorf("prompts: unknown version %q (available: %s)", version, strings.Join(c.versions, ", "))
	}
	return strings.TrimRight(string(data), "\n") + "\n", nil
}

// Versions returns every version available in the catalog, sorted.
func (c *Catalog) Versions() []string {
	if c == nil {
		return nil
	}
	out := make([]string, len(c.versions))
	copy(out, c.versions)
	return out
}

// Default returns the default version supplied to NewCatalog.
func (c *Catalog) Default() string {
	if c == nil {
		return ""
	}
	return c.defaultVersion
}

func listVersions(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ".md"))
	}
	sort.Strings(out)
	return out, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
