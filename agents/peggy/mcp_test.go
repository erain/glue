package peggy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
	filestore "github.com/erain/glue/stores/file"
)

func TestLoadSettingsMCPServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(t.TempDir(), "settings.json")
	raw := []byte(`{
  "store": {"type": "file", "path": "$HOME/sessions"},
  "mcp": {
    "servers": {
      "fake": {
        "enabled": true,
        "transport": "stdio",
        "command": "fake-mcp",
        "args": ["--root", "$HOME/work"],
        "env": ["LOG_LEVEL=warn"],
        "work_dir": "$HOME/work",
        "timeout_seconds": 7
      },
      "future": {
        "enabled": false,
        "transport": "http",
        "url": "https://example.invalid/mcp",
        "headers_env": {"Authorization": "FAKE_AUTH_HEADER"}
      }
    }
  }
}`)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	settings, _, err := LoadSettings(cfgPath)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	fake := settings.MCP.Servers["fake"]
	if !fake.Enabled || fake.Transport != "stdio" || fake.Command != "fake-mcp" {
		t.Fatalf("fake server = %+v", fake)
	}
	if got := fake.WorkDir; got != filepath.Join(home, "work") {
		t.Fatalf("work_dir = %q", got)
	}
	if fake.TimeoutSeconds != 7 || len(fake.Env) != 1 || fake.Env[0] != "LOG_LEVEL=warn" {
		t.Fatalf("fake server = %+v", fake)
	}
	future := settings.MCP.Servers["future"]
	if future.Enabled || future.Transport != "http" || future.HeadersEnv["Authorization"] != "FAKE_AUTH_HEADER" {
		t.Fatalf("future server = %+v", future)
	}
}

func TestMCPServerConfigsRejectsUnsupportedEnabledTransport(t *testing.T) {
	_, _, err := MCPServerConfigs(MCPSettings{Servers: map[string]MCPServerSettings{
		"remote": {Enabled: true, Transport: "http", URL: "https://example.invalid/mcp"},
	}})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("MCPServerConfigs error = %v, want unsupported transport", err)
	}
}

func TestPeggyRegistersMCPToolsWhenEnabled(t *testing.T) {
	provider := &fakeProvider{text: "ok"}
	p := newMCPTestPeggy(t, provider, glue.AllowAll{}, mcpTestServer("tools", ""))

	if _, err := p.Prompt(context.Background(), "s", "what tools are available?", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(provider.requests) == 0 {
		t.Fatal("provider not called")
	}
	var names []string
	for _, tool := range provider.requests[0].Tools {
		names = append(names, tool.Name)
	}
	if !containsString(names, "mcp_fake_echo") {
		t.Fatalf("tools = %v, missing mcp_fake_echo", names)
	}
}

func TestPeggyMCPToolUsesPermissionPath(t *testing.T) {
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		toolCallTurn("call_1", "mcp_fake_echo", `{"text":"hi"}`),
		peggyTextTurn("done"),
	}}
	perm := &recordingPermission{decision: glue.PermissionDecision{Allow: true}}
	p := newMCPTestPeggy(t, provider, perm, mcpTestServer("tools", ""))

	if _, err := p.Prompt(context.Background(), "s", "call echo", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(perm.requests) != 1 {
		t.Fatalf("permission requests = %d, want 1", len(perm.requests))
	}
	req := perm.requests[0]
	if req.Tool != "mcp_fake_echo" || req.Action != "mcp_call" || req.Target != "fake.echo" {
		t.Fatalf("permission request = %+v", req)
	}
	if string(req.Args) != `{"text":"hi"}` {
		t.Fatalf("permission args = %s", req.Args)
	}
}

func TestPeggyMCPReadOnlyTierDeniesBeforeExecution(t *testing.T) {
	callFile := filepath.Join(t.TempDir(), "called")
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{
		toolCallTurn("call_1", "mcp_fake_echo", `{"text":"hi"}`),
		peggyTextTurn("done"),
	}}
	inner := &recordingPermission{decision: glue.PermissionDecision{Allow: true}}
	perm := NewTieredPermission(inner, PermissionTierReadOnly, PermissionChannelCLI)
	p := newMCPTestPeggy(t, provider, perm, mcpTestServerWithFiles("tools", "", callFile))

	if _, err := p.Prompt(context.Background(), "s", "call echo", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(inner.requests) != 0 {
		t.Fatalf("inner permission requests = %d, want 0", len(inner.requests))
	}
	if _, err := os.Stat(callFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("call marker err = %v, want not exist", err)
	}
}

func TestPeggyCloseClosesMCPManager(t *testing.T) {
	closeFile := filepath.Join(t.TempDir(), "closed")
	p := newMCPTestPeggy(t, &fakeProvider{text: "ok"}, glue.AllowAll{}, mcpTestServer("tools", closeFile))

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if raw, err := os.ReadFile(closeFile); err == nil && string(raw) == "closed" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("close marker %s was not written", closeFile)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newMCPTestPeggy(t *testing.T, provider glue.Provider, permission glue.Permission, server MCPServerSettings) *Peggy {
	t.Helper()
	p, err := New(Options{
		Settings: Settings{
			MCP: MCPSettings{Servers: map[string]MCPServerSettings{"fake": server}},
		},
		Provider:   provider,
		Store:      filestore.New(filepath.Join(t.TempDir(), "sessions")),
		Permission: permission,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func mcpTestServer(scenario, closeFile string) MCPServerSettings {
	return mcpTestServerWithFiles(scenario, closeFile, "")
}

func mcpTestServerWithFiles(scenario, closeFile, callFile string) MCPServerSettings {
	env := []string{
		"PEGGY_MCP_HELPER=1",
		"PEGGY_MCP_SCENARIO=" + scenario,
	}
	if closeFile != "" {
		env = append(env, "PEGGY_MCP_CLOSE_FILE="+closeFile)
	}
	if callFile != "" {
		env = append(env, "PEGGY_MCP_CALL_FILE="+callFile)
	}
	return MCPServerSettings{
		Enabled:        true,
		Transport:      "stdio",
		Command:        os.Args[0],
		Args:           []string{"-test.run=TestPeggyMCPHelperProcess", "--"},
		Env:            env,
		TimeoutSeconds: 2,
	}
}

func TestPeggyMCPHelperProcess(t *testing.T) {
	if os.Getenv("PEGGY_MCP_HELPER") != "1" {
		return
	}
	closeFile := os.Getenv("PEGGY_MCP_CLOSE_FILE")
	err := runPeggyMCPHelper()
	if closeFile != "" {
		_ = os.WriteFile(closeFile, []byte("closed"), 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

type peggyMCPHelperRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func runPeggyMCPHelper() error {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)

	var initReq peggyMCPHelperRequest
	if err := dec.Decode(&initReq); err != nil {
		return err
	}
	if initReq.Method != "initialize" {
		return fmt.Errorf("first method = %q, want initialize", initReq.Method)
	}
	if err := writePeggyMCPResult(enc, initReq.ID, map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"serverInfo": map[string]string{
			"name":    "fake-peggy-mcp",
			"version": "0.1.0",
		},
	}); err != nil {
		return err
	}

	var initialized peggyMCPHelperRequest
	if err := dec.Decode(&initialized); err != nil {
		return err
	}
	if initialized.Method != "notifications/initialized" {
		return fmt.Errorf("method = %q, want notifications/initialized", initialized.Method)
	}

	for {
		var req peggyMCPHelperRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch req.Method {
		case "tools/list":
			if err := writePeggyMCPResult(enc, req.ID, map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "echoes text",
					"inputSchema": json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"additionalProperties":false}`),
				}},
			}); err != nil {
				return err
			}
		case "tools/call":
			if callFile := os.Getenv("PEGGY_MCP_CALL_FILE"); callFile != "" {
				_ = os.WriteFile(callFile, []byte("called"), 0o600)
			}
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments,omitempty"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return err
			}
			if params.Name != "echo" {
				return fmt.Errorf("tool call name = %q, want echo", params.Name)
			}
			text, _ := params.Arguments["text"].(string)
			if err := writePeggyMCPResult(enc, req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("method = %q, want tools/list or tools/call", req.Method)
		}
	}
}

func writePeggyMCPResult(enc *json.Encoder, id json.RawMessage, result any) error {
	return enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})
}
