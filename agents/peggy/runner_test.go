package peggy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	toolsmcp "github.com/erain/glue/tools/mcp"
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
	if Version != "0.4.0" {
		t.Fatalf("Version = %q, want %q", Version, "0.4.0")
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

func TestRunStatusDefaults(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"status"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"Peggy " + Version,
		"settings: built-in defaults",
		"identity: none",
		"provider: codex (provider default)",
		"context: disabled",
		"mcp: 0 configured, 0 enabled",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout = %q, missing %q", text, want)
		}
	}
}

func TestRunStatusJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	cfgPath := writeRunnerConfig(t, map[string]any{
		"provider": "openrouter",
		"model":    "openrouter/test-model",
		"store": map[string]any{
			"type": "file",
			"path": "$HOME/peggy-sessions",
		},
		"coding": map[string]any{
			"enabled":          true,
			"work_dir":         workDir,
			"allowed_binaries": []string{"git", "go"},
			"allow_overwrite":  true,
		},
		"context": map[string]any{
			"work_dir": workDir,
		},
		"permissions": map[string]any{
			"default_tier": "prompt",
			"channels": map[string]string{
				"telegram": "trusted",
			},
		},
		"channels": map[string]any{
			"telegram": map[string]any{"enabled": true},
		},
		"mcp": MCPSettings{Servers: map[string]MCPServerSettings{
			"filesystem": {
				Enabled:   true,
				Transport: "stdio",
				Command:   "mcp-server-filesystem",
			},
			"linear": {
				Enabled:   false,
				Transport: "http",
				URL:       "https://example.invalid/mcp",
			},
		}},
	})
	soulPath := filepath.Join(t.TempDir(), "SOUL.md")
	if err := os.WriteFile(soulPath, []byte("identity"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"status", "--config", cfgPath, "--soul", soulPath, "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	var report statusReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode status: %v\nstdout=%s", err, out.String())
	}
	if !report.Settings.Found || report.Settings.Path != cfgPath {
		t.Fatalf("settings = %+v", report.Settings)
	}
	if !report.Identity.Found || report.Identity.Bytes != len("identity") {
		t.Fatalf("identity = %+v", report.Identity)
	}
	if report.Provider.Name != "openrouter" || report.Provider.Model != "openrouter/test-model" {
		t.Fatalf("provider = %+v", report.Provider)
	}
	if !report.Coding.Enabled || report.Coding.WorkDir != workDir || !report.Coding.AllowOverwrite {
		t.Fatalf("coding = %+v", report.Coding)
	}
	if !report.Context.Enabled || report.Context.WorkDir != workDir {
		t.Fatalf("context = %+v", report.Context)
	}
	if report.MCP.Configured != 2 || report.MCP.Enabled != 1 || len(report.MCP.Servers) != 2 {
		t.Fatalf("mcp = %+v", report.MCP)
	}
	if len(report.Channels) != 1 || report.Channels[0] != "telegram" {
		t.Fatalf("channels = %+v", report.Channels)
	}
}

func TestRunSkillsListsWorkspaceSkills(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	skillDir := filepath.Join(workDir, ".agents", "skills", "triage")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: triage\ndescription: Triage one issue\n---\nInvestigate the issue."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "settings.json")
	cfg := map[string]any{
		"context": map[string]any{"work_dir": workDir},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(cfgPath, raw, 0o600)

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"skills", "--config", cfgPath}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	for _, want := range []string{"triage", "description: Triage one issue"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout = %q, missing %q", out.String(), want)
		}
	}
}

func TestRunSkillsJSON(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	skillDir := filepath.Join(workDir, ".agents", "skills", "daily")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: daily\ndescription: Daily plan\n---\nPlan the day."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "settings.json")
	raw, _ := json.MarshalIndent(map[string]any{
		"context": map[string]any{"work_dir": workDir},
	}, "", "  ")
	_ = os.WriteFile(cfgPath, raw, 0o600)

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"skills", "--config", cfgPath, "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	var catalog []skillCatalogEntry
	if err := json.Unmarshal(out.Bytes(), &catalog); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, out.String())
	}
	if len(catalog) != 1 || catalog[0].Name != "daily" || catalog[0].Description != "Daily plan" {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestRunRolesListsWorkspaceRoles(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	roleDir := filepath.Join(workDir, "roles")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "reviewer.md"), []byte("---\nname: reviewer\ndescription: Reviews diffs\nmodel: role-model\n---\nReview carefully."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "settings.json")
	raw, _ := json.MarshalIndent(map[string]any{
		"context": map[string]any{"work_dir": workDir},
	}, "", "  ")
	_ = os.WriteFile(cfgPath, raw, 0o600)

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"roles", "--config", cfgPath}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	for _, want := range []string{"reviewer", "description: Reviews diffs", "model: role-model"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout = %q, missing %q", out.String(), want)
		}
	}
}

func TestRunRolesJSON(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	roleDir := filepath.Join(workDir, "roles")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "reviewer.md"), []byte("---\nname: reviewer\nmodel: role-model\n---\nReview carefully."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "settings.json")
	raw, _ := json.MarshalIndent(map[string]any{
		"context": map[string]any{"work_dir": workDir},
	}, "", "  ")
	_ = os.WriteFile(cfgPath, raw, 0o600)

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"roles", "--config", cfgPath, "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	var catalog []roleCatalogEntry
	if err := json.Unmarshal(out.Bytes(), &catalog); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, out.String())
	}
	if len(catalog) != 1 || catalog[0].Name != "reviewer" || catalog[0].Model != "role-model" {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestRunSkillFlagsParse(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	workDir := t.TempDir()
	skillDir := filepath.Join(workDir, ".agents", "skills", "triage")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("Investigate the issue."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "settings.json")
	raw, _ := json.MarshalIndent(map[string]any{
		"provider": "bogus-provider",
		"context":  map[string]any{"work_dir": workDir},
	}, "", "  ")
	_ = os.WriteFile(cfgPath, raw, 0o600)

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"skill", "--config", cfgPath, "--arg", "issue=GLUE-123", "triage"}, &out, &errOut)
	if code == 2 {
		t.Fatalf("exit 2 indicates skill flags were not recognised; stderr=%q", errOut.String())
	}
}

func TestRunMCPToolsNoServers(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "tools"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "No MCP tools configured.") {
		t.Fatalf("stdout = %q, want empty catalog message", out.String())
	}
	if !strings.Contains(errOut.String(), "no settings.json found") {
		t.Fatalf("stderr = %q, want settings diagnostic", errOut.String())
	}
}

func TestRunMCPToolsListsConfiguredTools(t *testing.T) {
	cfgPath := writeRunnerConfig(t, map[string]any{
		"mcp": MCPSettings{Servers: map[string]MCPServerSettings{
			"fake": mcpTestServer("tools", ""),
		}},
	})

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "tools", "--config", cfgPath}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"mcp_fake_echo",
		"description: MCP fake: echoes text",
		"permission: mcp_call fake.echo",
		`parameters: {"type":"object","properties":{"text":{"type":"string"}},"additionalProperties":false}`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout = %q, missing %q", text, want)
		}
	}
}

func TestRunMCPToolsJSON(t *testing.T) {
	cfgPath := writeRunnerConfig(t, map[string]any{
		"mcp": MCPSettings{Servers: map[string]MCPServerSettings{
			"fake": mcpTestServer("tools", ""),
		}},
	})

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "tools", "--config", cfgPath, "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	var catalog []mcpToolCatalogEntry
	if err := json.Unmarshal(out.Bytes(), &catalog); err != nil {
		t.Fatalf("decode catalog: %v\nstdout=%s", err, out.String())
	}
	if len(catalog) != 1 {
		t.Fatalf("catalog len = %d, want 1: %+v", len(catalog), catalog)
	}
	entry := catalog[0]
	if entry.Name != "mcp_fake_echo" || entry.PermissionAction != "mcp_call" || entry.PermissionTarget != "fake.echo" {
		t.Fatalf("catalog entry = %+v", entry)
	}
	if len(entry.Parameters) == 0 || !strings.Contains(string(entry.Parameters), `"text"`) {
		t.Fatalf("parameters = %s", string(entry.Parameters))
	}
}

func TestRunMCPResourcesNoServers(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "resources"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "No MCP resources configured.") {
		t.Fatalf("stdout = %q, want empty catalog message", out.String())
	}
	if !strings.Contains(errOut.String(), "no settings.json found") {
		t.Fatalf("stderr = %q, want settings diagnostic", errOut.String())
	}
}

func TestRunMCPResourcesListsConfiguredResources(t *testing.T) {
	cfgPath := writeRunnerConfig(t, map[string]any{
		"mcp": MCPSettings{Servers: map[string]MCPServerSettings{
			"fake": mcpTestServer("resources", ""),
		}},
	})

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "resources", "--config", cfgPath}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"file:///workspace/README.md",
		"server: fake",
		"name: readme",
		"title: Project README",
		"description: repository overview",
		"mime_type: text/markdown",
		"size: 1234",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout = %q, missing %q", text, want)
		}
	}
}

func TestRunMCPResourcesJSON(t *testing.T) {
	cfgPath := writeRunnerConfig(t, map[string]any{
		"mcp": MCPSettings{Servers: map[string]MCPServerSettings{
			"fake": mcpTestServer("resources", ""),
		}},
	})

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "resources", "--config", cfgPath, "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	var catalog []mcpResourceCatalogEntry
	if err := json.Unmarshal(out.Bytes(), &catalog); err != nil {
		t.Fatalf("decode catalog: %v\nstdout=%s", err, out.String())
	}
	if len(catalog) != 1 {
		t.Fatalf("catalog len = %d, want 1: %+v", len(catalog), catalog)
	}
	entry := catalog[0]
	if entry.Server != "fake" || entry.URI != "file:///workspace/README.md" || entry.Name != "readme" || entry.MIMEType != "text/markdown" {
		t.Fatalf("catalog entry = %+v", entry)
	}
	if entry.Size == nil || *entry.Size != 1234 {
		t.Fatalf("size = %+v", entry.Size)
	}
}

func TestRunMCPReadRequiresFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "read"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--server is required") {
		t.Fatalf("stderr = %q, want server diagnostic", errOut.String())
	}
}

func TestRunMCPReadResource(t *testing.T) {
	cfgPath := writeRunnerConfig(t, map[string]any{
		"mcp": MCPSettings{Servers: map[string]MCPServerSettings{
			"fake": mcpTestServer("resources_only", ""),
		}},
	})

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "read", "--config", cfgPath, "--server", "fake", "--uri", "file:///workspace/README.md"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"file:///workspace/README.md",
		"server: fake",
		"requested_uri: file:///workspace/README.md",
		"mime_type: text/markdown",
		"Hello from Peggy MCP resource.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout = %q, missing %q", text, want)
		}
	}
}

func TestRunMCPReadJSON(t *testing.T) {
	cfgPath := writeRunnerConfig(t, map[string]any{
		"mcp": MCPSettings{Servers: map[string]MCPServerSettings{
			"fake": mcpTestServer("resources_only", ""),
		}},
	})

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "read", "--config", cfgPath, "--server", "fake", "--uri", "file:///workspace/README.md", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	var read toolsmcp.ResourceRead
	if err := json.Unmarshal(out.Bytes(), &read); err != nil {
		t.Fatalf("decode read: %v\nstdout=%s", err, out.String())
	}
	if read.Server != "fake" || read.URI != "file:///workspace/README.md" || len(read.Contents) != 1 {
		t.Fatalf("read = %+v", read)
	}
	if read.Contents[0].Text == nil || !strings.Contains(*read.Contents[0].Text, "Peggy MCP resource") {
		t.Fatalf("contents = %+v", read.Contents)
	}
}

func TestRunMCPPromptsNoServers(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "prompts"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "No MCP prompts configured.") {
		t.Fatalf("stdout = %q, want empty catalog message", out.String())
	}
	if !strings.Contains(errOut.String(), "no settings.json found") {
		t.Fatalf("stderr = %q, want settings diagnostic", errOut.String())
	}
}

func TestRunMCPPromptsListsConfiguredPrompts(t *testing.T) {
	cfgPath := writeRunnerConfig(t, map[string]any{
		"mcp": MCPSettings{Servers: map[string]MCPServerSettings{
			"fake": mcpTestServer("prompts_only", ""),
		}},
	})

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "prompts", "--config", cfgPath}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"daily_brief",
		"server: fake",
		"title: Daily Brief",
		"description: Draft a concise daily briefing",
		"topic required",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout = %q, missing %q", text, want)
		}
	}
}

func TestRunMCPPromptRendersPrompt(t *testing.T) {
	cfgPath := writeRunnerConfig(t, map[string]any{
		"mcp": MCPSettings{Servers: map[string]MCPServerSettings{
			"fake": mcpTestServer("prompts_only", ""),
		}},
	})

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "prompt", "--config", cfgPath, "--server", "fake", "--name", "daily_brief", "--arg", "topic=Go"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"daily_brief",
		"server: fake",
		"description: Rendered daily briefing prompt",
		"role: user",
		"Brief me on Go.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout = %q, missing %q", text, want)
		}
	}
}

func TestRunMCPPromptJSON(t *testing.T) {
	cfgPath := writeRunnerConfig(t, map[string]any{
		"mcp": MCPSettings{Servers: map[string]MCPServerSettings{
			"fake": mcpTestServer("prompts_only", ""),
		}},
	})

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "prompt", "--config", cfgPath, "--server", "fake", "--name", "daily_brief", "--arg", "topic=Go", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	var prompt toolsmcp.PromptGet
	if err := json.Unmarshal(out.Bytes(), &prompt); err != nil {
		t.Fatalf("decode prompt: %v\nstdout=%s", err, out.String())
	}
	if prompt.Server != "fake" || prompt.Name != "daily_brief" || len(prompt.Messages) != 1 {
		t.Fatalf("prompt = %+v", prompt)
	}
	if !strings.Contains(string(prompt.Messages[0].Content), "Brief me on Go.") {
		t.Fatalf("message = %s", string(prompt.Messages[0].Content))
	}
}

func TestRunMCPUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "bogus"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "unknown command") || !strings.Contains(errOut.String(), "Usage:") {
		t.Fatalf("stderr = %q, want usage", errOut.String())
	}
}

func TestRunServeMetadataDisabledRequiresExplicitToken(t *testing.T) {
	t.Setenv("GLUE_DAEMON_TOKEN", "")
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	var out, errOut bytes.Buffer
	code := runWithDeps(context.Background(), []string{
		"serve",
		"--metadata", "",
	}, strings.NewReader(""), &out, &errOut, func(context.Context, serveConfig, http.Handler, io.Writer) error {
		t.Fatal("serve should not be called")
		return nil
	})
	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if !strings.Contains(errOut.String(), "metadata disabled requires") {
		t.Fatalf("stderr = %q, want metadata error", errOut.String())
	}
}

func TestRunServeBuildsPeggyDaemonConfig(t *testing.T) {
	t.Setenv("GLUE_DAEMON_TOKEN", "")
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvSoulPath, "")
	t.Setenv(XDGConfigEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())

	cfgPath := writeRunnerConfig(t, map[string]any{
		"provider": "openrouter",
		"store": map[string]any{
			"type": "file",
			"path": filepath.Join(t.TempDir(), "sessions"),
		},
	})
	soulPath := filepath.Join(t.TempDir(), "SOUL.md")
	if err := os.WriteFile(soulPath, []byte("You are Peggy."), 0o600); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()

	var captured serveConfig
	var gotHandler bool
	var out, errOut bytes.Buffer
	code := runWithDeps(context.Background(), []string{
		"serve",
		"--config", cfgPath,
		"--soul", soulPath,
		"--listen", "127.0.0.1:12345",
		"--token", "tok",
		"--metadata", "",
		"--permission-timeout", "2s",
		"--coding",
		"--workdir", workDir,
	}, strings.NewReader(""), &out, &errOut, func(_ context.Context, cfg serveConfig, handler http.Handler, _ io.Writer) error {
		captured = cfg
		gotHandler = handler != nil
		return nil
	})
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, errOut.String())
	}
	if !gotHandler {
		t.Fatal("handler was nil")
	}
	if captured.ListenAddr != "127.0.0.1:12345" || captured.Token != "tok" || captured.TokenSource != "flag" || captured.MetadataPath != "" {
		t.Fatalf("serve config = %+v", captured)
	}
	if captured.PermissionTimeout != 2*time.Second {
		t.Fatalf("permission timeout = %s", captured.PermissionTimeout)
	}
	if !strings.Contains(errOut.String(), "coding tools enabled for "+workDir) {
		t.Fatalf("stderr = %q, want coding diagnostic", errOut.String())
	}
}

func writeRunnerConfig(t *testing.T, cfg map[string]any) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "settings.json")
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}
