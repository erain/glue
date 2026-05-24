package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestManagerExposesMCPTools(t *testing.T) {
	cfg := helperConfig("tools")
	cfg.Name = "fake-server"
	mgr, err := NewManager(context.Background(), []ServerConfig{cfg}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	tool := requireTool(t, mgr.Tools(), "mcp_fake_server_weather_lookup")
	if !tool.RequiresPermission {
		t.Fatal("RequiresPermission = false, want true")
	}
	if tool.PermissionAction != "mcp_call" {
		t.Fatalf("PermissionAction = %q, want mcp_call", tool.PermissionAction)
	}
	if got := tool.PermissionTarget(glue.ToolCall{}); got != "fake-server.weather.lookup" {
		t.Fatalf("PermissionTarget = %q", got)
	}
	if got := string(tool.Parameters); got != `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}` {
		t.Fatalf("parameters = %s", got)
	}
	if !strings.Contains(tool.Description, "fake-server/Weather Lookup") {
		t.Fatalf("description = %q", tool.Description)
	}

	result, err := tool.Execute(context.Background(), glue.ToolCall{
		Name:      tool.Name,
		Arguments: json.RawMessage(`{"city":"Paris"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true")
	}
	if len(result.Content) != 1 || result.Content[0].Text != "weather for Paris" {
		t.Fatalf("content = %#v", result.Content)
	}
	if result.Metadata["mcp_server"] != "fake-server" || result.Metadata["mcp_tool"] != "weather.lookup" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
	structured, ok := result.Metadata["mcp_structured_content"].(map[string]any)
	if !ok || structured["city"] != "Paris" || structured["temperature_c"].(float64) != 21 {
		t.Fatalf("structured metadata = %#v", result.Metadata["mcp_structured_content"])
	}
	if _, ok := result.Metadata["mcp_output_schema"].(map[string]any); !ok {
		t.Fatalf("output schema metadata = %#v", result.Metadata["mcp_output_schema"])
	}
	if _, ok := result.Metadata["mcp_annotations"].(map[string]any); !ok {
		t.Fatalf("annotations metadata = %#v", result.Metadata["mcp_annotations"])
	}
}

func TestManagerUsesDefaultSchemaAndRendersStructuredFallback(t *testing.T) {
	mgr, err := NewManager(context.Background(), []ServerConfig{helperConfig("tools")}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	tool := requireTool(t, mgr.Tools(), "mcp_fake_no_schema")
	if got := string(tool.Parameters); got != string(emptyObjectSchema) {
		t.Fatalf("parameters = %s", got)
	}
	result, err := tool.Execute(context.Background(), glue.ToolCall{Name: tool.Name})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != `{"answer":42}` {
		t.Fatalf("content = %#v", result.Content)
	}
}

func TestManagerMapsNonTextAndToolErrorResults(t *testing.T) {
	mgr, err := NewManager(context.Background(), []ServerConfig{helperConfig("tools")}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	imageTool := requireTool(t, mgr.Tools(), "mcp_fake_image_tool")
	imageResult, err := imageTool.Execute(context.Background(), glue.ToolCall{Name: imageTool.Name})
	if err != nil {
		t.Fatalf("image Execute: %v", err)
	}
	if len(imageResult.Content) != 1 || !strings.Contains(imageResult.Content[0].Text, `"type":"image"`) {
		t.Fatalf("image fallback content = %#v", imageResult.Content)
	}
	nonText, ok := imageResult.Metadata["mcp_non_text_content"].([]any)
	if !ok || len(nonText) != 1 {
		t.Fatalf("non-text metadata = %#v", imageResult.Metadata["mcp_non_text_content"])
	}

	errorTool := requireTool(t, mgr.Tools(), "mcp_fake_error_tool")
	errorResult, err := errorTool.Execute(context.Background(), glue.ToolCall{Name: errorTool.Name})
	if err != nil {
		t.Fatalf("error Execute: %v", err)
	}
	if !errorResult.IsError || len(errorResult.Content) != 1 || errorResult.Content[0].Text != "tool failed" {
		t.Fatalf("error result = %#v", errorResult)
	}
}

func TestManagerMapsRPCErrorToToolResult(t *testing.T) {
	mgr, err := NewManager(context.Background(), []ServerConfig{helperConfig("tools")}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	tool := requireTool(t, mgr.Tools(), "mcp_fake_rpc_fail")
	result, err := tool.Execute(context.Background(), glue.ToolCall{Name: tool.Name})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "call exploded") {
		t.Fatalf("result = %#v", result)
	}
}

func TestManagerRejectsMalformedSchema(t *testing.T) {
	_, err := NewManager(context.Background(), []ServerConfig{helperConfig("bad_schema")}, Options{})
	if err == nil || !strings.Contains(err.Error(), "inputSchema") {
		t.Fatalf("NewManager error = %v, want inputSchema error", err)
	}
}

func TestManagerRejectsToolNameCollision(t *testing.T) {
	_, err := NewManager(context.Background(), []ServerConfig{helperConfig("collision")}, Options{})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("NewManager error = %v, want collision error", err)
	}
}

func TestManagerListsMCPResources(t *testing.T) {
	cfg := helperConfig("resources")
	cfg.Name = "fake-server"
	mgr, err := NewManager(context.Background(), []ServerConfig{cfg}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	resources, err := mgr.Resources(context.Background())
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("resources = %d, want 1: %+v", len(resources), resources)
	}
	resource := resources[0]
	if resource.Server != "fake-server" || resource.URI != "file:///workspace/README.md" || resource.Name != "readme" {
		t.Fatalf("resource identity = %+v", resource)
	}
	if resource.Title != "Project README" || resource.Description != "repository overview" || resource.MIMEType != "text/markdown" {
		t.Fatalf("resource metadata = %+v", resource)
	}
	if resource.Size == nil || *resource.Size != 1234 {
		t.Fatalf("resource size = %+v", resource.Size)
	}
	if resource.Annotations["priority"].(float64) != 0.8 {
		t.Fatalf("annotations = %+v", resource.Annotations)
	}
}

func TestManagerReadsMCPResource(t *testing.T) {
	cfg := helperConfig("resources")
	cfg.Name = "fake-server"
	mgr, err := NewManager(context.Background(), []ServerConfig{cfg}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	read, err := mgr.ReadResource(context.Background(), "fake-server", "file:///workspace/README.md")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if read.Server != "fake-server" || read.URI != "file:///workspace/README.md" {
		t.Fatalf("read identity = %+v", read)
	}
	if len(read.Contents) != 2 {
		t.Fatalf("contents = %d, want 2: %+v", len(read.Contents), read.Contents)
	}
	first := read.Contents[0]
	if first.Text == nil || !strings.Contains(*first.Text, "Hello from MCP resource") || first.MIMEType != "text/markdown" {
		t.Fatalf("text content = %+v", first)
	}
	if first.Meta["etag"] != "abc123" {
		t.Fatalf("text meta = %+v", first.Meta)
	}
	second := read.Contents[1]
	if second.Blob == nil || *second.Blob != "aW1hZ2U=" || second.MIMEType != "image/png" {
		t.Fatalf("blob content = %+v", second)
	}
}

func TestManagerExposesResourceReadTool(t *testing.T) {
	cfg := helperConfig("resources")
	cfg.Name = "fake-server"
	mgr, err := NewManager(context.Background(), []ServerConfig{cfg}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	tool := requireTool(t, mgr.Tools(), "mcp_fake_server_read_resource")
	if !tool.RequiresPermission {
		t.Fatal("RequiresPermission = false, want true")
	}
	if tool.PermissionAction != "mcp_read_resource" {
		t.Fatalf("PermissionAction = %q, want mcp_read_resource", tool.PermissionAction)
	}
	args := json.RawMessage(`{"uri":"file:///workspace/README.md"}`)
	if got := tool.PermissionTarget(glue.ToolCall{Arguments: args}); got != "fake-server:file:///workspace/README.md" {
		t.Fatalf("PermissionTarget = %q", got)
	}

	result, err := tool.Execute(context.Background(), glue.ToolCall{Name: tool.Name, Arguments: args})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true")
	}
	if len(result.Content) != 2 || !strings.Contains(result.Content[0].Text, "Hello from MCP resource") {
		t.Fatalf("content = %#v", result.Content)
	}
	if result.Metadata["mcp_resource_uri"] != "file:///workspace/README.md" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
}

func TestManagerSkipsServersWithoutResources(t *testing.T) {
	mgr, err := NewManager(context.Background(), []ServerConfig{helperConfig("tools")}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	resources, err := mgr.Resources(context.Background())
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(resources) != 0 {
		t.Fatalf("resources = %+v, want none", resources)
	}
}

func TestManagerSupportsResourceOnlyServers(t *testing.T) {
	mgr, err := NewManager(context.Background(), []ServerConfig{helperConfig("resources_only")}, Options{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()
	requireTool(t, mgr.Tools(), "mcp_fake_read_resource")

	resources, err := mgr.Resources(context.Background())
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(resources) != 1 || resources[0].URI != "file:///workspace/README.md" {
		t.Fatalf("resources = %+v", resources)
	}
}

func requireTool(t *testing.T, tools []glue.Tool, name string) glue.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found in %#v", name, tools)
	return glue.Tool{}
}
