package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/erain/glue"
)

var emptyObjectSchema = json.RawMessage(`{"type":"object","additionalProperties":false}`)

// Options configures an MCP manager. It is intentionally empty for the first
// stdio/tool-mapping slice so future compatibility options have a stable home.
type Options struct{}

// Manager owns initialized MCP clients and exposes their discovered tools as
// ordinary glue tools.
type Manager struct {
	clients []*Client
	tools   []glue.Tool
}

// NewManager initializes each configured MCP server, discovers its tools, and
// maps them to glue.Tool values.
func NewManager(ctx context.Context, configs []ServerConfig, _ Options) (*Manager, error) {
	m := &Manager{}
	seen := map[string]struct{}{}
	for _, cfg := range configs {
		serverName := strings.TrimSpace(cfg.Name)
		if serverName == "" {
			_ = m.Close()
			return nil, errors.New("mcp: server name is required")
		}
		serverPart := sanitizeNamePart(serverName)
		if serverPart == "" {
			_ = m.Close()
			return nil, fmt.Errorf("mcp: server name %q has no valid tool-name characters", cfg.Name)
		}

		client, err := NewClient(ctx, cfg)
		if err != nil {
			_ = m.Close()
			return nil, fmt.Errorf("mcp: server %q: %w", serverName, err)
		}
		m.clients = append(m.clients, client)

		tools, err := listGlueTools(ctx, client, cfg, serverName, serverPart, seen)
		if err != nil {
			_ = m.Close()
			return nil, fmt.Errorf("mcp: server %q: %w", serverName, err)
		}
		m.tools = append(m.tools, tools...)
	}
	return m, nil
}

// Tools returns the discovered glue tools.
func (m *Manager) Tools() []glue.Tool {
	if m == nil {
		return nil
	}
	return append([]glue.Tool(nil), m.tools...)
}

// Close releases all MCP client transports owned by the manager.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	var errs []error
	for _, client := range m.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type listToolsResult struct {
	Tools []toolDefinition `json:"tools"`
}

type toolDefinition struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  map[string]any  `json:"annotations,omitempty"`
}

type callToolResult struct {
	Content           []json.RawMessage `json:"content,omitempty"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func listGlueTools(ctx context.Context, client *Client, cfg ServerConfig, serverName, serverPart string, seen map[string]struct{}) ([]glue.Tool, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeoutFor(cfg))
	defer cancel()

	var listed listToolsResult
	if err := client.Request(reqCtx, "tools/list", nil, &listed); err != nil {
		return nil, err
	}

	out := make([]glue.Tool, 0, len(listed.Tools))
	for _, def := range listed.Tools {
		tool, err := mapTool(client, cfg, serverName, serverPart, def, seen)
		if err != nil {
			return nil, err
		}
		out = append(out, tool)
	}
	return out, nil
}

func mapTool(client *Client, cfg ServerConfig, serverName, serverPart string, def toolDefinition, seen map[string]struct{}) (glue.Tool, error) {
	originalName := strings.TrimSpace(def.Name)
	if originalName == "" {
		return glue.Tool{}, errors.New("mcp: tool name is required")
	}
	toolPart := sanitizeNamePart(originalName)
	if toolPart == "" {
		return glue.Tool{}, fmt.Errorf("mcp: tool name %q has no valid tool-name characters", originalName)
	}
	glueName := "mcp_" + serverPart + "_" + toolPart
	if _, ok := seen[glueName]; ok {
		return glue.Tool{}, fmt.Errorf("mcp: tool name collision for %q", glueName)
	}
	seen[glueName] = struct{}{}

	schema, err := normalizeSchema(def.InputSchema, "inputSchema", originalName)
	if err != nil {
		return glue.Tool{}, err
	}
	if len(def.OutputSchema) > 0 {
		if _, err := normalizeSchema(def.OutputSchema, "outputSchema", originalName); err != nil {
			return glue.Tool{}, err
		}
	}

	remote := remoteTool{
		client:       client,
		serverName:   serverName,
		glueName:     glueName,
		originalName: originalName,
		timeout:      timeoutFor(cfg),
		outputSchema: compactRaw(def.OutputSchema),
		annotations:  cloneMap(def.Annotations),
	}

	return glue.Tool{
		ToolSpec: glue.ToolSpec{
			Name:               glueName,
			Description:        toolDescription(serverName, def),
			Parameters:         schema,
			RequiresPermission: true,
			PermissionAction:   "mcp_call",
			PermissionTarget: func(glue.ToolCall) string {
				return serverName + "." + originalName
			},
		},
		Execute: remote.execute,
	}, nil
}

type remoteTool struct {
	client       *Client
	serverName   string
	glueName     string
	originalName string
	timeout      time.Duration
	outputSchema json.RawMessage
	annotations  map[string]any
}

func (t remoteTool) execute(ctx context.Context, call glue.ToolCall) (glue.ToolResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	args := json.RawMessage(`{}`)
	if len(bytes.TrimSpace(call.Arguments)) > 0 {
		args = call.Arguments
	}

	var result callToolResult
	if err := t.client.Request(reqCtx, "tools/call", callToolParams{
		Name:      t.originalName,
		Arguments: args,
	}, &result); err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			return glue.ErrorResult(err), nil
		}
		return glue.ToolResult{}, err
	}
	return t.mapResult(result), nil
}

func (t remoteTool) mapResult(result callToolResult) glue.ToolResult {
	metadata := map[string]any{
		"mcp_server":    t.serverName,
		"mcp_tool":      t.originalName,
		"mcp_glue_tool": t.glueName,
	}
	if len(t.outputSchema) > 0 {
		metadata["mcp_output_schema"] = decodeRaw(t.outputSchema)
	}
	if len(t.annotations) > 0 {
		metadata["mcp_annotations"] = t.annotations
	}
	if len(result.StructuredContent) > 0 {
		metadata["mcp_structured_content"] = decodeRaw(result.StructuredContent)
	}

	content := make([]glue.ContentPart, 0, len(result.Content))
	var nonText []any
	var nonTextRaw []json.RawMessage
	for _, raw := range result.Content {
		var part struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		}
		if err := json.Unmarshal(raw, &part); err != nil {
			nonText = append(nonText, string(raw))
			nonTextRaw = append(nonTextRaw, raw)
			continue
		}
		if part.Type == "text" {
			content = append(content, glue.ContentPart{Type: glue.ContentTypeText, Text: part.Text})
			continue
		}
		nonText = append(nonText, decodeRaw(raw))
		nonTextRaw = append(nonTextRaw, raw)
	}
	if len(nonText) > 0 {
		metadata["mcp_non_text_content"] = nonText
	}
	if len(content) == 0 {
		if fallback := fallbackContent(result.StructuredContent, nonTextRaw); fallback != "" {
			content = append(content, glue.ContentPart{Type: glue.ContentTypeText, Text: fallback})
		}
	}

	return glue.ToolResult{
		Content:  content,
		IsError:  result.IsError,
		Metadata: metadata,
	}
}

func timeoutFor(cfg ServerConfig) time.Duration {
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return defaultTimeout
}

func sanitizeNamePart(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.TrimSpace(s) {
		r = unicode.ToLower(r)
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func normalizeSchema(raw json.RawMessage, field, tool string) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return append(json.RawMessage(nil), emptyObjectSchema...), nil
	}
	compacted := compactRaw(raw)
	var obj map[string]any
	if err := json.Unmarshal(compacted, &obj); err != nil {
		return nil, fmt.Errorf("mcp: tool %q %s is malformed: %w", tool, field, err)
	}
	if obj == nil {
		return nil, fmt.Errorf("mcp: tool %q %s must be a JSON object", tool, field)
	}
	return compacted, nil
}

func toolDescription(serverName string, def toolDefinition) string {
	title := strings.TrimSpace(def.Title)
	desc := strings.TrimSpace(def.Description)
	switch {
	case title != "" && desc != "":
		return fmt.Sprintf("MCP %s/%s: %s", serverName, title, desc)
	case desc != "":
		return fmt.Sprintf("MCP %s: %s", serverName, desc)
	case title != "":
		return fmt.Sprintf("MCP %s: %s", serverName, title)
	default:
		return fmt.Sprintf("MCP tool %s/%s", serverName, def.Name)
	}
}

func fallbackContent(structured json.RawMessage, nonText []json.RawMessage) string {
	if len(structured) > 0 {
		return string(compactRaw(structured))
	}
	if len(nonText) == 1 {
		return string(compactRaw(nonText[0]))
	}
	if len(nonText) > 1 {
		return compactRawArray(nonText)
	}
	return ""
}

func compactRaw(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return append(json.RawMessage(nil), buf.Bytes()...)
}

func compactRawArray(values []json.RawMessage) string {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(compactRaw(value))
	}
	buf.WriteByte(']')
	return buf.String()
}

func decodeRaw(raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

func cloneMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
