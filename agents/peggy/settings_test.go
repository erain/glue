package peggy

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestLoadSettings_HappyPathWithDefaults(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv(XDGConfigEnv, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{
		"provider": "openrouter",
		"model":    "ring-2.6-1t:free",
	})
	s, used, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if used != path {
		t.Errorf("resolved path = %q want %q", used, path)
	}
	if s.Provider != "openrouter" || s.Model != "ring-2.6-1t:free" {
		t.Errorf("settings: %+v", s)
	}
	// Defaults filled in.
	if s.Store.Type != "sqlite" {
		t.Errorf("store type default = %q", s.Store.Type)
	}
	if s.Compaction.TargetTokens != 8000 {
		t.Errorf("target_tokens default = %d", s.Compaction.TargetTokens)
	}
	if s.Compaction.KeepRecent != 8 {
		t.Errorf("keep_recent default = %d", s.Compaction.KeepRecent)
	}
}

func TestLoadSettings_ExplicitPathMissingIsError(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	_, _, err := LoadSettings(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("expected error for explicit missing path")
	}
}

func TestLoadSettings_NoFileFallsBackToDefaults(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir()) // empty dir
	t.Setenv("HOME", t.TempDir())
	s, used, err := LoadSettings("")
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if used != "" {
		t.Errorf("used path should be empty when no file found, got %q", used)
	}
	if s.Provider != DefaultProvider {
		t.Errorf("default provider = %q", s.Provider)
	}
	if !strings.HasSuffix(s.Store.Path, "peggy.db") {
		t.Errorf("sqlite default path = %q (should end in peggy.db)", s.Store.Path)
	}
}

func TestLoadSettings_EnvPathPicked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"provider": "nvidia"})
	t.Setenv(EnvConfigPath, path)
	s, used, err := LoadSettings("")
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if used != path {
		t.Errorf("resolved = %q want %q", used, path)
	}
	if s.Provider != "nvidia" {
		t.Errorf("provider = %q", s.Provider)
	}
}

func TestLoadSettings_EnvPathMissingIsError(t *testing.T) {
	t.Setenv(EnvConfigPath, filepath.Join(t.TempDir(), "nope.json"))
	_, _, err := LoadSettings("")
	if err == nil {
		t.Fatal("expected error when PEGGY_CONFIG points at missing file")
	}
}

func TestLoadSettings_XDGFallback(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	xdg := t.TempDir()
	t.Setenv(XDGConfigEnv, xdg)
	path := filepath.Join(xdg, "peggy", "settings.json")
	writeJSON(t, path, map[string]any{"provider": "gemini"})
	s, used, err := LoadSettings("")
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if used != path {
		t.Errorf("resolved = %q want %q", used, path)
	}
	if s.Provider != "gemini" {
		t.Errorf("provider = %q", s.Provider)
	}
}

func TestLoadSettings_TildeExpansionInStorePath(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(XDGConfigEnv, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	writeJSON(t, path, map[string]any{
		"store": map[string]any{"type": "sqlite", "path": "~/.peggy/x.db"},
	})
	s, _, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	want := filepath.Join(home, ".peggy", "x.db")
	if s.Store.Path != want {
		t.Errorf("path = %q want %q", s.Store.Path, want)
	}
}

func TestLoadSettings_InvalidJSONErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadSettings(path)
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}

func TestLoadSettings_CodingDefaultsAndWorkDirExpansion(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(XDGConfigEnv, "")
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSON(t, path, map[string]any{
		"coding": map[string]any{
			"enabled":  true,
			"work_dir": "~",
		},
	})

	s, _, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if !s.Coding.Enabled {
		t.Fatal("coding.enabled = false, want true")
	}
	if s.Coding.WorkDir != home {
		t.Fatalf("coding.work_dir = %q, want %q", s.Coding.WorkDir, home)
	}
	if len(s.Coding.AllowedBinaries) == 0 {
		t.Fatal("coding.allowed_binaries default is empty")
	}
}

func TestExpandPath_HomeAndTilde(t *testing.T) {
	t.Setenv("HOME", "/tmp/peggy-home")
	got, err := expandPath("~/.cache")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/peggy-home/.cache" {
		t.Errorf("got %q", got)
	}
	got, err = expandPath("${HOME}/data")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/peggy-home/data" {
		t.Errorf("got %q", got)
	}
}

func TestLoadSoul_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SOUL.md")
	if err := os.WriteFile(path, []byte("# Identity\nI am Peggy."), 0o600); err != nil {
		t.Fatal(err)
	}
	got, used, err := LoadSoul(path)
	if err != nil {
		t.Fatalf("LoadSoul: %v", err)
	}
	if !strings.Contains(got, "I am Peggy") {
		t.Errorf("contents not loaded: %q", got)
	}
	if used != path {
		t.Errorf("used = %q want %q", used, path)
	}
}

func TestLoadSoul_MissingDefaultNonFatal(t *testing.T) {
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir()) // empty
	t.Setenv("HOME", t.TempDir())
	got, used, err := LoadSoul("")
	if err != nil {
		t.Fatalf("LoadSoul: %v", err)
	}
	if got != "" || used != "" {
		t.Errorf("expected empty result, got %q / %q", got, used)
	}
}

func TestLoadSoul_ExplicitPathMissingIsError(t *testing.T) {
	t.Setenv(EnvSoulPath, "")
	_, _, err := LoadSoul(filepath.Join(t.TempDir(), "missing.md"))
	if err == nil {
		t.Fatal("expected error for explicit missing path")
	}
	if errors.Is(err, fs.ErrNotExist) {
		// good — error includes the missing-file root cause
	}
}

func TestLoadSoul_EnvPathMissingIsError(t *testing.T) {
	t.Setenv(EnvSoulPath, filepath.Join(t.TempDir(), "nope.md"))
	_, _, err := LoadSoul("")
	if err == nil {
		t.Fatal("expected error when PEGGY_SOUL points at missing file")
	}
}

func TestFillDefaults_NoOpWhenAllSet(t *testing.T) {
	s := Settings{
		Provider: "x",
		Model:    "y",
		Store: StoreSettings{
			Type: "sqlite",
			Path: "/tmp/x.db",
		},
		Compaction: CompactionSettings{
			TargetTokens: 100,
			KeepRecent:   2,
			Threshold:    10,
		},
	}
	got := fillDefaults(s)
	if got.Provider != s.Provider || got.Model != s.Model || got.Store != s.Store || got.Compaction != s.Compaction {
		t.Errorf("fillDefaults mutated set values: %+v", got)
	}
}
