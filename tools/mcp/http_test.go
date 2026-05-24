package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

func TestHTTPManagerDiscoversAndCallsTools(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var missingProtocol []string
	var authFailures []string
	var initializeProtocol string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var req httpTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		methods = append(methods, req.Method)
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			authFailures = append(authFailures, req.Method+":"+got)
		}
		if req.Method == "initialize" {
			initializeProtocol = r.Header.Get("MCP-Protocol-Version")
		} else if got := r.Header.Get("MCP-Protocol-Version"); got != ProtocolVersion {
			missingProtocol = append(missingProtocol, req.Method+":"+got)
		}
		mu.Unlock()

		switch req.Method {
		case "initialize":
			writeHTTPJSON(t, w, req.ID, initializeResult(ProtocolVersion))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeHTTPJSON(t, w, req.ID, map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "echoes text",
					"inputSchema": json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
				}},
			})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments,omitempty"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Errorf("decode params: %v", err)
			}
			writeHTTPJSON(t, w, req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": params.Arguments["text"]}},
			})
		default:
			t.Errorf("unexpected method %q", req.Method)
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	mgr, err := NewManager(context.Background(), []ServerConfig{{
		Name:      "remote",
		Transport: TransportHTTP,
		URL:       srv.URL,
		Headers:   map[string]string{"Authorization": "Bearer test-token"},
		Timeout:   2 * time.Second,
	}}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	tool := requireHTTPTool(t, mgr.Tools(), "mcp_remote_echo")
	result, err := tool.Execute(context.Background(), glue.ToolCall{
		Name:      tool.Name,
		Arguments: json.RawMessage(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hello" {
		t.Fatalf("result = %#v", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if initializeProtocol != "" {
		t.Fatalf("initialize protocol header = %q, want empty", initializeProtocol)
	}
	if len(missingProtocol) != 0 {
		t.Fatalf("missing protocol headers: %v", missingProtocol)
	}
	if len(authFailures) != 0 {
		t.Fatalf("auth header failures: %v", authFailures)
	}
	for _, want := range []string{"initialize", "notifications/initialized", "tools/list", "tools/call"} {
		if !containsMethod(methods, want) {
			t.Fatalf("methods = %v, missing %s", methods, want)
		}
	}
}

func TestHTTPTransportParsesSSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req httpTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "initialize":
			writeHTTPJSON(t, w, req.ID, initializeResult(ProtocolVersion))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			payload := jsonRPCResponse(req.ID, map[string]any{
				"tools": []map[string]any{{"name": "sse_tool"}},
			})
			raw, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("event: message\n"))
			_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
		default:
			t.Errorf("unexpected method %q", req.Method)
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	mgr, err := NewManager(context.Background(), []ServerConfig{{
		Name:      "sse",
		Transport: TransportHTTP,
		URL:       srv.URL,
		Timeout:   2 * time.Second,
	}}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()
	requireHTTPTool(t, mgr.Tools(), "mcp_sse_sse_tool")
}

func TestHTTPClientRejectsInvalidContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	_, err := NewHTTPClient(context.Background(), ServerConfig{
		Name:    "bad",
		URL:     srv.URL,
		Timeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported http content type") {
		t.Fatalf("NewHTTPClient error = %v, want content-type error", err)
	}
}

type httpTestRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func writeHTTPJSON(t *testing.T, w http.ResponseWriter, id json.RawMessage, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(jsonRPCResponse(id, result)); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func jsonRPCResponse(id json.RawMessage, result any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	}
}

func requireHTTPTool(t *testing.T, tools []glue.Tool, name string) glue.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found in %#v", name, tools)
	return glue.Tool{}
}

func containsMethod(methods []string, want string) bool {
	for _, method := range methods {
		if method == want {
			return true
		}
	}
	return false
}
