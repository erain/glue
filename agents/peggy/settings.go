package peggy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Settings is the on-disk JSON shape of Peggy's config. All fields
// are optional except where noted. The loader applies sensible
// defaults via fillDefaults so a minimal `{}` settings.json works.
type Settings struct {
	// Provider names the model backend. One of: codex, gemini,
	// openrouter, nvidia. Default: "codex".
	Provider string `json:"provider"`

	// Model is the model id (e.g. "gpt-5-codex"). Empty falls back
	// to the provider's DefaultModel.
	Model string `json:"model"`

	// Store configures session persistence. Type is one of
	// "sqlite" (default) or "file". Path is the DB file (sqlite) or
	// directory (file).
	Store StoreSettings `json:"store"`

	// Compaction tunes the SummarizingCompactor. Zero values fall
	// back to the framework defaults (target 8000 / keep 8).
	Compaction CompactionSettings `json:"compaction"`
}

// StoreSettings configures session persistence.
type StoreSettings struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// CompactionSettings configures the SummarizingCompactor knobs.
type CompactionSettings struct {
	TargetTokens int `json:"target_tokens"`
	KeepRecent   int `json:"keep_recent"`
	// Threshold is the message-count gate before the compactor runs
	// at all. Zero disables compaction even when TargetTokens is set.
	Threshold int `json:"threshold"`
}

// Environment variables consulted when paths aren't given explicitly.
const (
	EnvConfigPath = "PEGGY_CONFIG"
	EnvSoulPath   = "PEGGY_SOUL"
	XDGConfigEnv  = "XDG_CONFIG_HOME"
)

// DefaultProvider is the registry-default provider Peggy uses when
// no settings file is present.
const DefaultProvider = "codex"

// LoadSettings reads and parses settings.json. When path is empty,
// the loader walks the resolution chain $PEGGY_CONFIG →
// $XDG_CONFIG_HOME/peggy/settings.json → ~/.config/peggy/settings.json.
//
// The function returns (Settings, "", nil) with defaults applied
// when no config file is found at any candidate path — Peggy still
// runs with built-in defaults, just without user overrides.
//
// Returns the parsed Settings, the path the loader read from (empty
// when no file existed), and an error only on read or parse
// failures.
func LoadSettings(path string) (Settings, string, error) {
	resolved, ok, err := resolveConfigPath(path)
	if err != nil {
		return Settings{}, "", err
	}
	if !ok {
		s := Settings{}
		s = fillDefaults(s)
		return s, "", nil
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return Settings{}, resolved, fmt.Errorf("peggy: read %s: %w", resolved, err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, resolved, fmt.Errorf("peggy: parse %s: %w", resolved, err)
	}
	s = fillDefaults(s)
	if s.Store.Path != "" {
		expanded, err := expandPath(s.Store.Path)
		if err != nil {
			return Settings{}, resolved, err
		}
		s.Store.Path = expanded
	}
	return s, resolved, nil
}

func resolveConfigPath(explicit string) (string, bool, error) {
	if explicit != "" {
		expanded, err := expandPath(explicit)
		if err != nil {
			return "", false, err
		}
		if _, err := os.Stat(expanded); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", false, fmt.Errorf("peggy: settings %s does not exist", expanded)
			}
			return "", false, err
		}
		return expanded, true, nil
	}
	if p := os.Getenv(EnvConfigPath); p != "" {
		expanded, err := expandPath(p)
		if err != nil {
			return "", false, err
		}
		if _, err := os.Stat(expanded); err == nil {
			return expanded, true, nil
		}
		// PEGGY_CONFIG explicitly set but missing → error: caller
		// wanted that file. Mirrors the explicit-path behavior.
		return "", false, fmt.Errorf("peggy: PEGGY_CONFIG=%s does not exist", expanded)
	}
	// XDG / HOME fallbacks: missing file is fine, we'll run with
	// built-in defaults.
	for _, candidate := range []string{
		filepath.Join(xdgConfigHome(), "peggy", "settings.json"),
	} {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true, nil
		}
	}
	return "", false, nil
}

// xdgConfigHome returns $XDG_CONFIG_HOME or ~/.config when unset.
// Returns "" when even $HOME is unresolvable.
func xdgConfigHome() string {
	if v := os.Getenv(XDGConfigEnv); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}

// expandPath resolves leading "~" and "$HOME" / "${HOME}" placeholders
// in p. Other env vars are left untouched — opinionated to keep the
// surface predictable.
func expandPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("peggy: resolve ~: %w", err)
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	p = strings.ReplaceAll(p, "${HOME}", os.Getenv("HOME"))
	p = strings.ReplaceAll(p, "$HOME", os.Getenv("HOME"))
	return p, nil
}

// fillDefaults applies the documented defaults for any zero-valued
// settings field. Exposed via Settings only — callers pass an
// unmodified parsed struct in and get a fully-populated one back.
func fillDefaults(s Settings) Settings {
	if s.Provider == "" {
		s.Provider = DefaultProvider
	}
	if s.Store.Type == "" {
		s.Store.Type = "sqlite"
	}
	if s.Store.Path == "" {
		home, _ := os.UserHomeDir()
		switch s.Store.Type {
		case "sqlite":
			s.Store.Path = filepath.Join(home, ".peggy", "peggy.db")
		case "file":
			s.Store.Path = filepath.Join(home, ".peggy", "sessions")
		}
	}
	if s.Compaction.Threshold == 0 {
		// 200 messages is "long-running session" without being so
		// aggressive that every prompt triggers compaction. Adjustable.
		s.Compaction.Threshold = 200
	}
	if s.Compaction.TargetTokens == 0 {
		s.Compaction.TargetTokens = 8000
	}
	if s.Compaction.KeepRecent == 0 {
		s.Compaction.KeepRecent = 8
	}
	return s
}
