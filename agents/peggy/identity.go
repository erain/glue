package peggy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SoulBasename is the conventional filename for Peggy's identity file.
const SoulBasename = "SOUL.md"

// LoadSoul reads the identity Markdown file. When path is empty the
// loader walks $PEGGY_SOUL → $XDG_CONFIG_HOME/peggy/SOUL.md →
// ~/.config/peggy/SOUL.md.
//
// Missing files are *not* an error: the function returns ("", "", nil)
// so callers can run Peggy without an identity (a warning to stderr
// is appropriate at the call site). Only read errors and explicit
// environment-pointed misses are surfaced.
func LoadSoul(path string) (contents string, resolved string, err error) {
	if path != "" {
		expanded, err := expandPath(path)
		if err != nil {
			return "", "", err
		}
		return readSoul(expanded, true)
	}
	if p := os.Getenv(EnvSoulPath); p != "" {
		expanded, err := expandPath(p)
		if err != nil {
			return "", "", err
		}
		// Explicit env points at a specific path; missing is an error.
		return readSoul(expanded, true)
	}
	candidate := filepath.Join(xdgConfigHome(), "peggy", SoulBasename)
	if candidate == "" {
		return "", "", nil
	}
	return readSoul(candidate, false)
}

func readSoul(path string, strict bool) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !strict {
			return "", "", nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("peggy: SOUL.md %s does not exist", path)
		}
		return "", "", fmt.Errorf("peggy: read SOUL.md %s: %w", path, err)
	}
	return string(data), path, nil
}
