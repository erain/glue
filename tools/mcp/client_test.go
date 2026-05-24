package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	if err := writeHelperResult(enc, initReq.ID, initializeResult(ProtocolVersion)); err != nil {
		return err
	}
	if err := readInitialized(dec); err != nil {
		return err
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

func initializeResult(version string) map[string]any {
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{},
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
