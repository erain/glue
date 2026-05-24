package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStdioClientInitialize(t *testing.T) {
	c, err := NewStdioClient(context.Background(), helperConfig("success"))
	if err != nil {
		t.Fatalf("NewStdioClient: %v", err)
	}
	defer c.Close()
	if got := c.InitializeResult().ProtocolVersion; got != ProtocolVersion {
		t.Fatalf("protocol = %q, want %q", got, ProtocolVersion)
	}
	if got := c.InitializeResult().ServerInfo.Name; got != "fake-mcp" {
		t.Fatalf("server name = %q", got)
	}
}

func TestStdioClientRejectsIncompatibleProtocol(t *testing.T) {
	_, err := NewStdioClient(context.Background(), helperConfig("incompatible"))
	if err == nil || !strings.Contains(err.Error(), "incompatible protocol") {
		t.Fatalf("NewStdioClient error = %v", err)
	}
}

func TestStdioClientSurfacesRPCError(t *testing.T) {
	c, err := NewStdioClient(context.Background(), helperConfig("rpc_error"))
	if err != nil {
		t.Fatalf("NewStdioClient: %v", err)
	}
	defer c.Close()

	err = c.Request(context.Background(), "tools/list", nil, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("Request error = %T %v, want RPCError", err, err)
	}
	if rpcErr.Code != -32001 || rpcErr.Message != "boom" {
		t.Fatalf("rpc error = %+v", rpcErr)
	}
}

func TestStdioClientRejectsWrongResponseID(t *testing.T) {
	_, err := NewStdioClient(context.Background(), helperConfig("wrong_id"))
	if err == nil || !strings.Contains(err.Error(), "response id") {
		t.Fatalf("NewStdioClient error = %v", err)
	}
}

func TestStdioClientCapturesStderr(t *testing.T) {
	cfg := helperConfig("stderr")
	cfg.MaxStderrBytes = 12
	c, err := NewStdioClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewStdioClient: %v", err)
	}
	defer c.Close()
	deadline := time.Now().Add(time.Second)
	for {
		if got := c.Stderr(); got == "stderr-line-" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stderr = %q", c.Stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStdioClientInitializeCancellationCleansUp(t *testing.T) {
	cfg := helperConfig("hang")
	cfg.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := NewStdioClient(context.Background(), cfg)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NewStdioClient error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation cleanup took %s", elapsed)
	}
}

func helperConfig(scenario string) ServerConfig {
	return ServerConfig{
		Name:    "fake",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPHelperProcess", "--"},
		Env: []string{
			"MCP_HELPER=1",
			"MCP_SCENARIO=" + scenario,
		},
		Timeout: 2 * time.Second,
	}
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("MCP_HELPER") != "1" {
		return
	}
	if err := runMCPHelper(os.Getenv("MCP_SCENARIO")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

type helperRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func runMCPHelper(scenario string) error {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)

	var initReq helperRequest
	if err := dec.Decode(&initReq); err != nil {
		return err
	}
	if initReq.Method != "initialize" {
		return fmt.Errorf("first method = %q, want initialize", initReq.Method)
	}
	switch scenario {
	case "wrong_id":
		return enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      99,
			"result":  initializeResult(ProtocolVersion),
		})
	case "incompatible":
		if err := writeHelperResult(enc, initReq.ID, initializeResult("2024-11-05")); err != nil {
			return err
		}
		return readInitialized(dec)
	case "hang":
		time.Sleep(10 * time.Second)
		return nil
	case "stderr":
		fmt.Fprint(os.Stderr, "stderr-line-with-extra-data")
	}

	if err := writeHelperResult(enc, initReq.ID, initializeResultForScenario(ProtocolVersion, scenario)); err != nil {
		return err
	}
	if err := readInitialized(dec); err != nil {
		return err
	}
	if scenario == "tools" || scenario == "bad_schema" || scenario == "collision" || scenario == "resources" || scenario == "resources_only" {
		return runMCPToolScenario(dec, enc, scenario)
	}
	if scenario != "rpc_error" {
		return nil
	}
	var req helperRequest
	if err := dec.Decode(&req); err != nil {
		return err
	}
	return enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(req.ID),
		"error": map[string]any{
			"code":    -32001,
			"message": "boom",
			"data":    map[string]any{"detail": "test"},
		},
	})
}

func runMCPToolScenario(dec *json.Decoder, enc *json.Encoder, scenario string) error {
	for {
		var req helperRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch req.Method {
		case "tools/list":
			if scenario == "resources_only" {
				return fmt.Errorf("resources_only server received tools/list")
			}
			if err := writeHelperResult(enc, req.ID, helperToolsList(scenario)); err != nil {
				return err
			}
		case "tools/call":
			if err := handleHelperToolCall(enc, req); err != nil {
				return err
			}
		case "resources/list":
			if scenario != "resources" && scenario != "resources_only" {
				return fmt.Errorf("method = %q, want tools/list or tools/call", req.Method)
			}
			if err := writeHelperResult(enc, req.ID, helperResourcesList()); err != nil {
				return err
			}
		case "resources/read":
			if scenario != "resources" && scenario != "resources_only" {
				return fmt.Errorf("method = %q, want tools/list or tools/call", req.Method)
			}
			if err := handleHelperResourceRead(enc, req); err != nil {
				return err
			}
		default:
			return fmt.Errorf("method = %q, want tools/list, tools/call, resources/list, or resources/read", req.Method)
		}
	}
}

func helperToolsList(scenario string) map[string]any {
	switch scenario {
	case "bad_schema":
		return map[string]any{
			"tools": []map[string]any{{
				"name":        "bad.schema",
				"description": "bad schema",
				"inputSchema": "not an object",
			}},
		}
	case "collision":
		return map[string]any{
			"tools": []map[string]any{
				{"name": "a-b", "inputSchema": json.RawMessage(`{"type":"object"}`)},
				{"name": "a_b", "inputSchema": json.RawMessage(`{"type":"object"}`)},
			},
		}
	default:
		return map[string]any{
			"tools": []map[string]any{
				{
					"name":         "weather.lookup",
					"title":        "Weather Lookup",
					"description":  "returns a short forecast",
					"inputSchema":  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
					"outputSchema": json.RawMessage(`{"type":"object","properties":{"temperature_c":{"type":"number"}}}`),
					"annotations":  map[string]any{"readOnlyHint": true},
				},
				{"name": "no_schema", "description": "returns structured content"},
				{"name": "image.tool", "inputSchema": json.RawMessage(`{"type":"object"}`)},
				{"name": "error.tool", "inputSchema": json.RawMessage(`{"type":"object"}`)},
				{"name": "rpc.fail", "inputSchema": json.RawMessage(`{"type":"object"}`)},
			},
		}
	}
}

func helperResourcesList() map[string]any {
	return map[string]any{
		"resources": []map[string]any{{
			"uri":         "file:///workspace/README.md",
			"name":        "readme",
			"title":       "Project README",
			"description": "repository overview",
			"mimeType":    "text/markdown",
			"annotations": map[string]any{"audience": []string{"assistant"}, "priority": 0.8},
			"size":        1234,
		}},
	}
}

func handleHelperResourceRead(enc *json.Encoder, req helperRequest) error {
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return err
	}
	if params.URI != "file:///workspace/README.md" {
		return fmt.Errorf("resource uri = %q, want file:///workspace/README.md", params.URI)
	}
	return writeHelperResult(enc, req.ID, map[string]any{
		"contents": []map[string]any{
			{
				"uri":      params.URI,
				"mimeType": "text/markdown",
				"text":     "# Project README\n\nHello from MCP resource.",
				"_meta":    map[string]any{"etag": "abc123"},
			},
			{
				"uri":      "file:///workspace/logo.png",
				"mimeType": "image/png",
				"blob":     "aW1hZ2U=",
			},
		},
	})
}

func handleHelperToolCall(enc *json.Encoder, req helperRequest) error {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return err
	}
	switch params.Name {
	case "weather.lookup":
		city, _ := params.Arguments["city"].(string)
		return writeHelperResult(enc, req.ID, map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "weather for " + city,
			}},
			"structuredContent": map[string]any{
				"city":          city,
				"temperature_c": 21,
			},
		})
	case "no_schema":
		return writeHelperResult(enc, req.ID, map[string]any{
			"structuredContent": map[string]any{"answer": 42},
		})
	case "image.tool":
		return writeHelperResult(enc, req.ID, map[string]any{
			"content": []map[string]any{{
				"type":     "image",
				"data":     "abc",
				"mimeType": "image/png",
			}},
		})
	case "error.tool":
		return writeHelperResult(enc, req.ID, map[string]any{
			"isError": true,
			"content": []map[string]any{{
				"type": "text",
				"text": "tool failed",
			}},
		})
	case "rpc.fail":
		return enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"error": map[string]any{
				"code":    -32002,
				"message": "call exploded",
			},
		})
	default:
		return fmt.Errorf("unknown helper tool %q", params.Name)
	}
}

func initializeResult(version string) map[string]any {
	return initializeResultWithCapabilities(version, map[string]any{})
}

func initializeResultForScenario(version, scenario string) map[string]any {
	switch scenario {
	case "resources":
		return initializeResultWithCapabilities(version, map[string]any{
			"tools":     map[string]any{},
			"resources": map[string]any{},
		})
	case "resources_only":
		return initializeResultWithCapabilities(version, map[string]any{
			"resources": map[string]any{},
		})
	default:
		return initializeResult(version)
	}
}

func initializeResultWithCapabilities(version string, capabilities map[string]any) map[string]any {
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    capabilities,
		"serverInfo": map[string]string{
			"name":    "fake-mcp",
			"version": "0.1.0",
		},
	}
}

func writeHelperResult(enc *json.Encoder, id json.RawMessage, result any) error {
	return enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})
}

func readInitialized(dec *json.Decoder) error {
	var req helperRequest
	if err := dec.Decode(&req); err != nil {
		return err
	}
	if req.Method != "notifications/initialized" {
		return fmt.Errorf("method = %q, want notifications/initialized", req.Method)
	}
	if len(req.ID) != 0 {
		return fmt.Errorf("initialized notification has id %s", req.ID)
	}
	return nil
}
