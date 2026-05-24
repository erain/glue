package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the MCP protocol version this package speaks.
const ProtocolVersion = "2025-11-25"

const defaultTimeout = 30 * time.Second

// ServerConfig configures one MCP server connection.
type ServerConfig struct {
	Name string

	// Stdio transport fields. Command is argv[0], never a shell string.
	Command string
	Args    []string
	Env     []string
	WorkDir string

	// Timeout caps lifecycle and request waits when callers do not supply a
	// tighter context deadline.
	Timeout time.Duration

	// MaxStderrBytes caps retained stderr diagnostics from stdio servers.
	MaxStderrBytes int
}

// Implementation describes an MCP peer.
type Implementation struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// InitializeResult is the portion of MCP initialize results needed by this
// package.
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ServerInfo      Implementation `json:"serverInfo,omitempty"`
	Instructions    string         `json:"instructions,omitempty"`
}

// RPCError is a JSON-RPC error returned by an MCP server.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("mcp: rpc error %d: %s: %s", e.Code, e.Message, strings.TrimSpace(string(e.Data)))
	}
	return fmt.Sprintf("mcp: rpc error %d: %s", e.Code, e.Message)
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type transport interface {
	Encode(any) error
	Decode(*rpcResponse) error
	Close() error
	Stderr() string
}

// Client is one initialized MCP client session.
type Client struct {
	tr transport

	mu     sync.Mutex
	nextID int64

	init InitializeResult
}

// NewStdioClient starts a stdio MCP server and completes lifecycle
// negotiation.
func NewStdioClient(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	tr, err := startStdioTransport(cfg)
	if err != nil {
		return nil, err
	}
	c := &Client{tr: tr}

	initCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if err := c.initialize(initCtx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// InitializeResult returns the negotiated server information.
func (c *Client) InitializeResult() InitializeResult {
	if c == nil {
		return InitializeResult{}
	}
	return c.init
}

// Request sends a JSON-RPC request and decodes the result into result when
// result is non-nil.
func (c *Client) Request(ctx context.Context, method string, params any, result any) error {
	if c == nil || c.tr == nil {
		return errors.New("mcp: nil client")
	}
	method = strings.TrimSpace(method)
	if method == "" {
		return errors.New("mcp: method is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := c.nextID
	if err := c.tr.Encode(rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}); err != nil {
		return err
	}

	type readResult struct {
		resp rpcResponse
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		resp, err := c.readResponse(id)
		ch <- readResult{resp: resp, err: err}
	}()

	select {
	case got := <-ch:
		if got.err != nil {
			return got.err
		}
		if result == nil {
			return nil
		}
		if len(got.resp.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(got.resp.Result, result); err != nil {
			return fmt.Errorf("mcp: decode %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		_ = c.Close()
		return ctx.Err()
	}
}

// Notify sends a JSON-RPC notification.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	if c == nil || c.tr == nil {
		return errors.New("mcp: nil client")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	method = strings.TrimSpace(method)
	if method == "" {
		return errors.New("mcp: method is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tr.Encode(rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
}

// Stderr returns retained stderr diagnostics for stdio transports.
func (c *Client) Stderr() string {
	if c == nil || c.tr == nil {
		return ""
	}
	return c.tr.Stderr()
}

// Close releases the underlying transport.
func (c *Client) Close() error {
	if c == nil || c.tr == nil {
		return nil
	}
	return c.tr.Close()
}

func (c *Client) initialize(ctx context.Context) error {
	var init InitializeResult
	if err := c.Request(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    "glue",
			"version": "0",
		},
	}, &init); err != nil {
		return err
	}
	if init.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("mcp: incompatible protocol version %q, want %q", init.ProtocolVersion, ProtocolVersion)
	}
	if err := c.Notify(ctx, "notifications/initialized", nil); err != nil {
		return err
	}
	c.init = init
	return nil
}

func (c *Client) readResponse(wantID int64) (rpcResponse, error) {
	for {
		var resp rpcResponse
		if err := c.tr.Decode(&resp); err != nil {
			return rpcResponse{}, err
		}
		if len(resp.ID) == 0 {
			continue
		}
		if resp.JSONRPC != "2.0" {
			return rpcResponse{}, fmt.Errorf("mcp: response jsonrpc = %q, want 2.0", resp.JSONRPC)
		}
		if strings.TrimSpace(string(resp.ID)) != fmt.Sprintf("%d", wantID) {
			return rpcResponse{}, fmt.Errorf("mcp: response id %s, want %d", strings.TrimSpace(string(resp.ID)), wantID)
		}
		if resp.Error != nil {
			return rpcResponse{}, resp.Error
		}
		return resp, nil
	}
}
