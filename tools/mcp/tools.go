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
var readResourceSchema = json.RawMessage(`{"type":"object","properties":{"uri":{"type":"string","description":"MCP resource URI to read from this server."}},"required":["uri"],"additionalProperties":false}`)

// Options configures an MCP manager. It is intentionally empty for the first
// stdio/tool-mapping slice so future compatibility options have a stable home.
type Options struct{}

// Manager owns initialized MCP clients and exposes their discovered tools as
// ordinary glue tools.
type Manager struct {
	servers []managedServer
	tools   []glue.Tool
}

type managedServer struct {
	name   string
	cfg    ServerConfig
	client *Client
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
		m.servers = append(m.servers, managedServer{name: serverName, cfg: cfg, client: client})

		if shouldListTools(client.InitializeResult()) {
			tools, err := listGlueTools(ctx, client, cfg, serverName, serverPart, seen)
			if err != nil {
				_ = m.Close()
				return nil, fmt.Errorf("mcp: server %q: %w", serverName, err)
			}
			m.tools = append(m.tools, tools...)
		}
		if hasCapability(client.InitializeResult(), "resources") {
			m.tools = append(m.tools, mapResourceReadTool(client, cfg, serverName, serverPart, seen))
		}
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

// Resource describes an MCP resource exposed by one configured server.
type Resource struct {
	Server      string         `json:"server"`
	URI         string         `json:"uri"`
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	MIMEType    string         `json:"mime_type,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Size        *int64         `json:"size,omitempty"`
}

// ResourceRead contains the contents returned by an MCP resources/read call.
type ResourceRead struct {
	Server   string            `json:"server"`
	URI      string            `json:"uri"`
	Contents []ResourceContent `json:"contents"`
}

// ResourceContent is one text or blob content item returned from a resource.
type ResourceContent struct {
	URI      string         `json:"uri"`
	MIMEType string         `json:"mime_type,omitempty"`
	Text     *string        `json:"text,omitempty"`
	Blob     *string        `json:"blob,omitempty"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

// Prompt describes an MCP prompt exposed by one configured server.
type Prompt struct {
	Server      string           `json:"server"`
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument describes one named prompt argument.
type PromptArgument struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptGet contains messages returned by an MCP prompts/get call.
type PromptGet struct {
	Server      string          `json:"server"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// PromptMessage is one role/content pair returned by a prompt.
type PromptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Resources lists resource metadata from servers that advertise MCP resource
// support. It does not read resource contents.
func (m *Manager) Resources(ctx context.Context) ([]Resource, error) {
	if m == nil {
		return nil, nil
	}
	var out []Resource
	for _, server := range m.servers {
		if !hasCapability(server.client.InitializeResult(), "resources") {
			continue
		}
		resources, err := listResources(ctx, server.client, server.cfg, server.name)
		if err != nil {
			return nil, fmt.Errorf("mcp: server %q: %w", server.name, err)
		}
		out = append(out, resources...)
	}
	return out, nil
}

// ReadResource reads a resource URI from one named configured MCP server.
func (m *Manager) ReadResource(ctx context.Context, serverName, uri string) (ResourceRead, error) {
	if m == nil {
		return ResourceRead{}, errors.New("mcp: manager is nil")
	}
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return ResourceRead{}, errors.New("mcp: resource server is required")
	}
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return ResourceRead{}, errors.New("mcp: resource uri is required")
	}
	for _, server := range m.servers {
		if server.name != serverName {
			continue
		}
		if !hasCapability(server.client.InitializeResult(), "resources") {
			return ResourceRead{}, fmt.Errorf("mcp: server %q does not support resources", serverName)
		}
		return readResource(ctx, server.client, server.cfg, serverName, uri)
	}
	return ResourceRead{}, fmt.Errorf("mcp: server %q is not configured", serverName)
}

// Prompts lists prompt metadata from servers that advertise MCP prompt support.
func (m *Manager) Prompts(ctx context.Context) ([]Prompt, error) {
	if m == nil {
		return nil, nil
	}
	var out []Prompt
	for _, server := range m.servers {
		if !hasCapability(server.client.InitializeResult(), "prompts") {
			continue
		}
		prompts, err := listPrompts(ctx, server.client, server.cfg, server.name)
		if err != nil {
			return nil, fmt.Errorf("mcp: server %q: %w", server.name, err)
		}
		out = append(out, prompts...)
	}
	return out, nil
}

// GetPrompt renders a prompt by name from one named configured MCP server.
func (m *Manager) GetPrompt(ctx context.Context, serverName, name string, args map[string]string) (PromptGet, error) {
	if m == nil {
		return PromptGet{}, errors.New("mcp: manager is nil")
	}
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return PromptGet{}, errors.New("mcp: prompt server is required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return PromptGet{}, errors.New("mcp: prompt name is required")
	}
	for _, server := range m.servers {
		if server.name != serverName {
			continue
		}
		if !hasCapability(server.client.InitializeResult(), "prompts") {
			return PromptGet{}, fmt.Errorf("mcp: server %q does not support prompts", serverName)
		}
		return getPrompt(ctx, server.client, server.cfg, serverName, name, args)
	}
	return PromptGet{}, fmt.Errorf("mcp: server %q is not configured", serverName)
}

// Close releases all MCP client transports owned by the manager.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	var errs []error
	for _, server := range m.servers {
		if err := server.client.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type listToolsResult struct {
	Tools []toolDefinition `json:"tools"`
}

type listResourcesResult struct {
	Resources  []resourceDefinition `json:"resources"`
	NextCursor string               `json:"nextCursor,omitempty"`
}

type readResourceResult struct {
	Contents []resourceContentDefinition `json:"contents"`
}

type listPromptsResult struct {
	Prompts    []promptDefinition `json:"prompts"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

type getPromptResult struct {
	Description string                    `json:"description,omitempty"`
	Messages    []promptMessageDefinition `json:"messages"`
}

type toolDefinition struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  map[string]any  `json:"annotations,omitempty"`
}

type resourceDefinition struct {
	URI         string         `json:"uri"`
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	MIMEType    string         `json:"mimeType,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Size        *int64         `json:"size,omitempty"`
}

type resourceContentDefinition struct {
	URI      string         `json:"uri"`
	MIMEType string         `json:"mimeType,omitempty"`
	Text     *string        `json:"text,omitempty"`
	Blob     *string        `json:"blob,omitempty"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

type promptDefinition struct {
	Name        string                     `json:"name"`
	Title       string                     `json:"title,omitempty"`
	Description string                     `json:"description,omitempty"`
	Arguments   []promptArgumentDefinition `json:"arguments,omitempty"`
}

type promptArgumentDefinition struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

type promptMessageDefinition struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
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

type readResourceParams struct {
	URI string `json:"uri"`
}

type getPromptParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
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

func listResources(ctx context.Context, client *Client, cfg ServerConfig, serverName string) ([]Resource, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeoutFor(cfg))
	defer cancel()

	var out []Resource
	var cursor string
	for {
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		var listed listResourcesResult
		if err := client.Request(reqCtx, "resources/list", params, &listed); err != nil {
			return nil, err
		}
		for _, def := range listed.Resources {
			resource, err := mapResource(serverName, def)
			if err != nil {
				return nil, err
			}
			out = append(out, resource)
		}
		if listed.NextCursor == "" {
			return out, nil
		}
		cursor = listed.NextCursor
	}
}

func readResource(ctx context.Context, client *Client, cfg ServerConfig, serverName, uri string) (ResourceRead, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeoutFor(cfg))
	defer cancel()

	var result readResourceResult
	if err := client.Request(reqCtx, "resources/read", readResourceParams{URI: uri}, &result); err != nil {
		return ResourceRead{}, err
	}

	contents := make([]ResourceContent, 0, len(result.Contents))
	for _, def := range result.Contents {
		content, err := mapResourceContent(def)
		if err != nil {
			return ResourceRead{}, err
		}
		contents = append(contents, content)
	}
	return ResourceRead{
		Server:   serverName,
		URI:      uri,
		Contents: contents,
	}, nil
}

func listPrompts(ctx context.Context, client *Client, cfg ServerConfig, serverName string) ([]Prompt, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeoutFor(cfg))
	defer cancel()

	var out []Prompt
	var cursor string
	for {
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		var listed listPromptsResult
		if err := client.Request(reqCtx, "prompts/list", params, &listed); err != nil {
			return nil, err
		}
		for _, def := range listed.Prompts {
			prompt, err := mapPrompt(serverName, def)
			if err != nil {
				return nil, err
			}
			out = append(out, prompt)
		}
		if listed.NextCursor == "" {
			return out, nil
		}
		cursor = listed.NextCursor
	}
}

func getPrompt(ctx context.Context, client *Client, cfg ServerConfig, serverName, name string, args map[string]string) (PromptGet, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeoutFor(cfg))
	defer cancel()

	var result getPromptResult
	if err := client.Request(reqCtx, "prompts/get", getPromptParams{Name: name, Arguments: cloneStringMap(args)}, &result); err != nil {
		return PromptGet{}, err
	}

	messages := make([]PromptMessage, 0, len(result.Messages))
	for _, def := range result.Messages {
		message, err := mapPromptMessage(def)
		if err != nil {
			return PromptGet{}, err
		}
		messages = append(messages, message)
	}
	return PromptGet{
		Server:      serverName,
		Name:        name,
		Description: strings.TrimSpace(result.Description),
		Messages:    messages,
	}, nil
}

func mapResourceContent(def resourceContentDefinition) (ResourceContent, error) {
	uri := strings.TrimSpace(def.URI)
	if uri == "" {
		return ResourceContent{}, errors.New("mcp: resource content uri is required")
	}
	if def.Text == nil && def.Blob == nil {
		return ResourceContent{}, fmt.Errorf("mcp: resource content %q must include text or blob", uri)
	}
	if def.Text != nil && def.Blob != nil {
		return ResourceContent{}, fmt.Errorf("mcp: resource content %q includes both text and blob", uri)
	}
	return ResourceContent{
		URI:      uri,
		MIMEType: strings.TrimSpace(def.MIMEType),
		Text:     cloneString(def.Text),
		Blob:     cloneString(def.Blob),
		Meta:     cloneMap(def.Meta),
	}, nil
}

func mapPrompt(serverName string, def promptDefinition) (Prompt, error) {
	name := strings.TrimSpace(def.Name)
	if name == "" {
		return Prompt{}, errors.New("mcp: prompt name is required")
	}
	args := make([]PromptArgument, 0, len(def.Arguments))
	for _, defArg := range def.Arguments {
		arg, err := mapPromptArgument(name, defArg)
		if err != nil {
			return Prompt{}, err
		}
		args = append(args, arg)
	}
	return Prompt{
		Server:      serverName,
		Name:        name,
		Title:       strings.TrimSpace(def.Title),
		Description: strings.TrimSpace(def.Description),
		Arguments:   args,
	}, nil
}

func mapPromptArgument(promptName string, def promptArgumentDefinition) (PromptArgument, error) {
	name := strings.TrimSpace(def.Name)
	if name == "" {
		return PromptArgument{}, fmt.Errorf("mcp: prompt %q argument name is required", promptName)
	}
	return PromptArgument{
		Name:        name,
		Title:       strings.TrimSpace(def.Title),
		Description: strings.TrimSpace(def.Description),
		Required:    def.Required,
	}, nil
}

func mapPromptMessage(def promptMessageDefinition) (PromptMessage, error) {
	role := strings.TrimSpace(def.Role)
	if role == "" {
		return PromptMessage{}, errors.New("mcp: prompt message role is required")
	}
	content := compactRaw(def.Content)
	if len(content) == 0 {
		return PromptMessage{}, fmt.Errorf("mcp: prompt message %q content is required", role)
	}
	return PromptMessage{
		Role:    role,
		Content: content,
	}, nil
}

func mapResource(serverName string, def resourceDefinition) (Resource, error) {
	uri := strings.TrimSpace(def.URI)
	name := strings.TrimSpace(def.Name)
	if uri == "" {
		return Resource{}, errors.New("mcp: resource uri is required")
	}
	if name == "" {
		return Resource{}, fmt.Errorf("mcp: resource %q name is required", uri)
	}
	return Resource{
		Server:      serverName,
		URI:         uri,
		Name:        name,
		Title:       strings.TrimSpace(def.Title),
		Description: strings.TrimSpace(def.Description),
		MIMEType:    strings.TrimSpace(def.MIMEType),
		Annotations: cloneMap(def.Annotations),
		Size:        cloneInt64(def.Size),
	}, nil
}

func mapResourceReadTool(client *Client, cfg ServerConfig, serverName, serverPart string, seen map[string]struct{}) glue.Tool {
	glueName := reserveGeneratedToolName("mcp_"+serverPart+"_read_resource", seen)
	remote := resourceReadTool{
		client:     client,
		serverName: serverName,
		glueName:   glueName,
		timeout:    timeoutFor(cfg),
	}

	return glue.Tool{
		ToolSpec: glue.ToolSpec{
			Name:               glueName,
			Description:        fmt.Sprintf("Read MCP resource contents from server %s by URI.", serverName),
			Parameters:         append(json.RawMessage(nil), readResourceSchema...),
			RequiresPermission: true,
			PermissionAction:   "mcp_read_resource",
			PermissionTarget: func(call glue.ToolCall) string {
				uri := resourceURIFromArgs(call.Arguments)
				if uri == "" {
					return serverName
				}
				return serverName + ":" + uri
			},
		},
		Execute: remote.execute,
	}
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

type resourceReadTool struct {
	client     *Client
	serverName string
	glueName   string
	timeout    time.Duration
}

func (t resourceReadTool) execute(ctx context.Context, call glue.ToolCall) (glue.ToolResult, error) {
	uri := resourceURIFromArgs(call.Arguments)
	if uri == "" {
		return glue.ErrorResult(errors.New("mcp: resource uri is required")), nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	read, err := readResource(reqCtx, t.client, ServerConfig{Timeout: t.timeout}, t.serverName, uri)
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			return glue.ErrorResult(err), nil
		}
		return glue.ToolResult{}, err
	}
	return t.mapResult(read), nil
}

func (t resourceReadTool) mapResult(read ResourceRead) glue.ToolResult {
	metadata := map[string]any{
		"mcp_server":        t.serverName,
		"mcp_resource_uri":  read.URI,
		"mcp_glue_tool":     t.glueName,
		"mcp_resource_read": resourceContentsMetadata(read.Contents),
	}

	content := make([]glue.ContentPart, 0, len(read.Contents))
	for _, item := range read.Contents {
		switch {
		case item.Text != nil:
			content = append(content, glue.ContentPart{Type: glue.ContentTypeText, Text: *item.Text})
		case item.Blob != nil:
			content = append(content, glue.ContentPart{Type: glue.ContentTypeText, Text: resourceBlobContentLine(item)})
		}
	}
	return glue.ToolResult{
		Content:  content,
		Metadata: metadata,
	}
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

func shouldListTools(init InitializeResult) bool {
	if hasCapability(init, "tools") {
		return true
	}
	return len(init.Capabilities) == 0
}

func hasCapability(init InitializeResult, name string) bool {
	if len(init.Capabilities) == 0 {
		return false
	}
	_, ok := init.Capabilities[name]
	return ok
}

func reserveGeneratedToolName(base string, seen map[string]struct{}) string {
	if _, ok := seen[base]; !ok {
		seen[base] = struct{}{}
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", base, i)
		if _, ok := seen[candidate]; !ok {
			seen[candidate] = struct{}{}
			return candidate
		}
	}
}

func resourceURIFromArgs(raw json.RawMessage) string {
	var args struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.URI)
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

func resourceContentsMetadata(contents []ResourceContent) []map[string]any {
	out := make([]map[string]any, 0, len(contents))
	for _, item := range contents {
		entry := map[string]any{"uri": item.URI}
		if item.MIMEType != "" {
			entry["mime_type"] = item.MIMEType
		}
		if item.Text != nil {
			entry["kind"] = "text"
			entry["bytes"] = len(*item.Text)
		}
		if item.Blob != nil {
			entry["kind"] = "blob"
			entry["base64_bytes"] = len(*item.Blob)
		}
		if len(item.Meta) > 0 {
			entry["_meta"] = cloneMap(item.Meta)
		}
		out = append(out, entry)
	}
	return out
}

func resourceBlobContentLine(item ResourceContent) string {
	entry := map[string]any{
		"uri":  item.URI,
		"blob": "",
	}
	if item.Blob != nil {
		entry["blob"] = *item.Blob
	}
	if item.MIMEType != "" {
		entry["mime_type"] = item.MIMEType
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return ""
	}
	return string(raw)
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

func cloneString(in *string) *string {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneInt64(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
