package peggy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_Version(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"--version"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), Version) {
		t.Errorf("stdout = %q", out.String())
	}
}

// TestVersionPinned guards against accidentally publishing a binary
// whose --version still says "-dev". Bump the constant deliberately
// at release time, and update this test to match.
func TestVersionPinned(t *testing.T) {
	if Version != "0.1.0" {
		t.Fatalf("Version = %q, want %q", Version, "0.1.0")
	}
}

func TestRun_NoPromptShowsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "Usage:") {
		t.Errorf("stderr missing usage: %q", errOut.String())
	}
}

func TestRun_HelpExits0(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"--help"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errOut.String(), "Usage:") {
		t.Errorf("stderr missing usage: %q", errOut.String())
	}
}

// TestRun_DefaultsWithMissingConfigStillRuns hits the "no settings.json
// found" path so we exercise the resolution-chain fallback. We don't
// actually run a prompt because the default codex provider would
// require live auth — just confirm Run prints the diagnostic.
func TestRun_NoConfigDiagnostic(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// Build a tiny config that points the store at a temp dir and
	// uses a non-network provider. We invoke Run with --config so
	// we don't accidentally hit the user's real config.
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "settings.json")
	cfg := map[string]any{
		// openrouter has an env-var probe; we don't set the key, so
		// the provider's construction succeeds and the first Stream
		// would fail with a clear API-key error. We never get that
		// far in this test — we exercise the setup path only.
		"provider": "openrouter",
		"store": map[string]any{
			"type": "file",
			"path": filepath.Join(cfgDir, "sessions"),
		},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"--config", cfgPath, "hi"}, &out, &errOut)
	// Without a key, openrouter Stream returns an error; Run exits 1.
	// That's fine — we're verifying the setup path, not the network call.
	if code == 0 {
		t.Logf("unexpected success (key set somewhere?); stdout=%q", out.String())
	}
	if !strings.Contains(errOut.String(), "no SOUL.md") {
		t.Errorf("expected SOUL.md diagnostic; stderr=%q", errOut.String())
	}
}

func TestRun_PromptArgsJoined(t *testing.T) {
	// We can't easily run an end-to-end Run because the package's
	// Run constructs a real provider. The Prompt-level tests cover
	// the end-to-end flow with a fake. Here we just confirm
	// flag.Parse doesn't error on multi-word prompts (would surface
	// as exit 2 with a usage dump).
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "settings.json")
	cfg := map[string]any{
		"provider": "openrouter",
		"store":    map[string]any{"type": "file", "path": filepath.Join(cfgDir, "s")},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(cfgPath, raw, 0o600)

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"--config", cfgPath, "tell", "me", "about", "Aussies"}, &out, &errOut)
	// Should fail at the provider (no API key) with exit 1, NOT exit 2
	// (which would mean flag parsing failed or no prompt was found).
	if code == 2 {
		t.Fatalf("exit 2 indicates the prompt wasn't recognised; stderr=%q", errOut.String())
	}
}

func TestRun_CodingFlagsParseAndUseInputAwareRunner(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "settings.json")
	cfg := map[string]any{
		"provider": "bogus-provider",
		"store":    map[string]any{"type": "file", "path": filepath.Join(cfgDir, "s")},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(cfgPath, raw, 0o600)

	var out, errOut bytes.Buffer
	code := RunWithInput(context.Background(), []string{
		"--config", cfgPath,
		"--coding",
		"--workdir", t.TempDir(),
		"help",
	}, strings.NewReader("n\n"), &out, &errOut)
	if code == 2 {
		t.Fatalf("exit 2 indicates coding flags were not recognised; stderr=%q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "coding tools enabled") {
		t.Fatalf("stderr = %q, want coding diagnostic", errOut.String())
	}
}
