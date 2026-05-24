package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue"
)

type scriptedProvider struct {
	events []glue.ProviderEvent
}

func (p scriptedProvider) Stream(context.Context, glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	ch := make(chan glue.ProviderEvent, len(p.events))
	for _, event := range p.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

type captureProvider struct {
	text     string
	mu       sync.Mutex
	requests []glue.ProviderRequest
}

func (p *captureProvider) Stream(_ context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	ch := make(chan glue.ProviderEvent, 3)
	ch <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	ch <- glue.ProviderEvent{Type: glue.ProviderEventTextDelta, Delta: p.text}
	ch <- glue.ProviderEvent{Type: glue.ProviderEventDone}
	close(ch)
	return ch, nil
}

type turnProvider struct {
	mu    sync.Mutex
	turns [][]glue.ProviderEvent
}

func (p *turnProvider) Stream(context.Context, glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.turns) == 0 {
		return nil, errors.New("turnProvider: unexpected stream")
	}
	events := p.turns[0]
	p.turns = p.turns[1:]
	ch := make(chan glue.ProviderEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

type blockingProvider struct {
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func (p *blockingProvider) Stream(ctx context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	ch := make(chan glue.ProviderEvent, 1)
	ch <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	p.once.Do(func() { close(p.started) })
	go func() {
		<-ctx.Done()
		close(p.canceled)
		close(ch)
	}()
	return ch, nil
}

type sessionOnlyHost struct{}

func (sessionOnlyHost) Session(context.Context, string, ...glue.SessionOption) (*glue.Session, error) {
	return nil, errors.New("unused")
}

type skillCatalogHost struct {
	skills []SkillCatalogEntry
}

func (skillCatalogHost) Session(context.Context, string, ...glue.SessionOption) (*glue.Session, error) {
	return nil, errors.New("unused")
}

func (h skillCatalogHost) SkillCatalog(context.Context) ([]SkillCatalogEntry, error) {
	return h.skills, nil
}

type mcpCatalogHost struct {
	resources []MCPResourceCatalogEntry
	prompts   []MCPPromptCatalogEntry
}

func (mcpCatalogHost) Session(context.Context, string, ...glue.SessionOption) (*glue.Session, error) {
	return nil, errors.New("unused")
}

func (h mcpCatalogHost) MCPResourceCatalog(context.Context) ([]MCPResourceCatalogEntry, error) {
	return h.resources, nil
}

func (h mcpCatalogHost) MCPPromptCatalog(context.Context) ([]MCPPromptCatalogEntry, error) {
	return h.prompts, nil
}

type mcpActionHost struct{}

func (mcpActionHost) Session(context.Context, string, ...glue.SessionOption) (*glue.Session, error) {
	return nil, errors.New("unused")
}

func (mcpActionHost) MCPReadResource(_ context.Context, req MCPReadResourceRequest) (MCPResourceReadResponse, error) {
	if req.Server != "filesystem" || req.URI != "file:///workspace/README.md" {
		return MCPResourceReadResponse{}, fmt.Errorf("unexpected read request: %+v", req)
	}
	text := "# Project README\n\nHello from daemon."
	return MCPResourceReadResponse{
		Server: req.Server,
		URI:    req.URI,
		Contents: []MCPResourceContent{{
			URI:      req.URI,
			MIMEType: "text/markdown",
			Text:     &text,
			Meta:     map[string]any{"source": "test"},
		}},
	}, nil
}

func (mcpActionHost) MCPRenderPrompt(_ context.Context, req MCPPromptRenderRequest) (MCPPromptRenderResponse, error) {
	if req.Server != "linear" || req.Name != "daily_brief" || req.Arguments["topic"] != "Go" {
		return MCPPromptRenderResponse{}, fmt.Errorf("unexpected prompt request: %+v", req)
	}
	return MCPPromptRenderResponse{
		Server:      req.Server,
		Name:        req.Name,
		Description: "Rendered daily briefing prompt",
		Messages: []MCPPromptMessage{{
			Role:    "user",
			Content: json.RawMessage(`{"type":"text","text":"Brief me on Go."}`),
		}},
	}, nil
}

func TestServerAuthAndHealth(t *testing.T) {
	srv := newTestServer(t, glue.NewAgent(glue.AgentOptions{Provider: scriptedProvider{}}))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	resp = postJSON(t, ts.URL+"/v1/sessions/default/runs", "", `{"text":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	resp = postJSON(t, ts.URL+"/v1/sessions/default/runs", "wrong", `{"text":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestServerToolsCatalog(t *testing.T) {
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: scriptedProvider{},
		Tools: []glue.Tool{{
			ToolSpec: glue.ToolSpec{
				Name:               "demo_tool",
				Description:        "Demo tool",
				Parameters:         json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
				RequiresPermission: true,
				PermissionAction:   "demo_action",
				PermissionTarget: func(call glue.ToolCall) string {
					return "target:" + call.Name
				},
			},
		}},
	})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := getJSON(t, ts.URL+"/v1/tools", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	resp = getJSON(t, ts.URL+"/v1/tools", "token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var catalog toolCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Tools) != 1 {
		t.Fatalf("catalog len = %d, want 1", len(catalog.Tools))
	}
	tool := catalog.Tools[0]
	if tool.Name != "demo_tool" || tool.Description != "Demo tool" || tool.PermissionAction != "demo_action" || tool.PermissionTargetPreview != "target:demo_tool" {
		t.Fatalf("tool = %+v", tool)
	}
	if !tool.RequiresPermission {
		t.Fatal("RequiresPermission = false, want true")
	}
	if !strings.Contains(string(tool.Parameters), `"name"`) {
		t.Fatalf("parameters = %s", string(tool.Parameters))
	}
}

func TestServerToolsCatalogUnsupportedHost(t *testing.T) {
	srv := newTestServer(t, sessionOnlyHost{})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := getJSON(t, ts.URL+"/v1/tools", "token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var catalog toolCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Tools) != 0 {
		t.Fatalf("catalog = %+v, want empty", catalog.Tools)
	}
}

func TestServerSkillsCatalog(t *testing.T) {
	srv := newTestServer(t, skillCatalogHost{skills: []SkillCatalogEntry{{
		Name:        "triage",
		Description: "Triage one issue",
	}}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := getJSON(t, ts.URL+"/v1/skills", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	resp = getJSON(t, ts.URL+"/v1/skills", "token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("skills status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var catalog skillCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || catalog.Skills[0].Name != "triage" || catalog.Skills[0].Description != "Triage one issue" {
		t.Fatalf("catalog = %+v", catalog.Skills)
	}

	var status statusResponse
	resp = getJSON(t, ts.URL+"/v1/status", "token")
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !contains(status.Capabilities, "skills") {
		t.Fatalf("capabilities = %v, missing skills", status.Capabilities)
	}
}

func TestServerSkillsCatalogUnsupportedHost(t *testing.T) {
	srv := newTestServer(t, sessionOnlyHost{})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := getJSON(t, ts.URL+"/v1/skills", "token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("skills status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var catalog skillCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 0 {
		t.Fatalf("catalog = %+v, want empty", catalog.Skills)
	}
}

func TestServerMCPCatalogs(t *testing.T) {
	size := int64(1234)
	srv := newTestServer(t, mcpCatalogHost{
		resources: []MCPResourceCatalogEntry{{
			Server:      "filesystem",
			URI:         "file:///workspace/README.md",
			Name:        "readme",
			Title:       "Project README",
			Description: "repository overview",
			MIMEType:    "text/markdown",
			Annotations: map[string]any{"audience": []any{"assistant"}},
			Size:        &size,
		}},
		prompts: []MCPPromptCatalogEntry{{
			Server:      "linear",
			Name:        "daily_brief",
			Title:       "Daily Brief",
			Description: "Draft a concise daily briefing",
			Arguments: []MCPPromptCatalogArgument{{
				Name:        "topic",
				Description: "Subject to brief",
				Required:    true,
			}},
		}},
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := getJSON(t, ts.URL+"/v1/mcp/resources", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated resources status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	resp = getJSON(t, ts.URL+"/v1/mcp/resources", "token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resources status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var resources mcpResourceCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&resources); err != nil {
		t.Fatal(err)
	}
	if len(resources.Resources) != 1 || resources.Resources[0].URI != "file:///workspace/README.md" || resources.Resources[0].MIMEType != "text/markdown" {
		t.Fatalf("resources = %+v", resources.Resources)
	}

	resp = getJSON(t, ts.URL+"/v1/mcp/prompts", "token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompts status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var prompts mcpPromptCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&prompts); err != nil {
		t.Fatal(err)
	}
	if len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "daily_brief" || len(prompts.Prompts[0].Arguments) != 1 {
		t.Fatalf("prompts = %+v", prompts.Prompts)
	}

	resp = getJSON(t, ts.URL+"/v1/status", "token")
	defer resp.Body.Close()
	var status statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mcp_resources", "mcp_prompts"} {
		if !contains(status.Capabilities, want) {
			t.Fatalf("capabilities = %v, missing %q", status.Capabilities, want)
		}
	}
}

func TestServerMCPCatalogsUnsupportedHost(t *testing.T) {
	srv := newTestServer(t, sessionOnlyHost{})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := getJSON(t, ts.URL+"/v1/mcp/resources", "token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resources status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var resources mcpResourceCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&resources); err != nil {
		t.Fatal(err)
	}
	if len(resources.Resources) != 0 {
		t.Fatalf("resources = %+v, want empty", resources.Resources)
	}

	resp = getJSON(t, ts.URL+"/v1/mcp/prompts", "token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompts status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var prompts mcpPromptCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&prompts); err != nil {
		t.Fatal(err)
	}
	if len(prompts.Prompts) != 0 {
		t.Fatalf("prompts = %+v, want empty", prompts.Prompts)
	}
}

func TestServerMCPReadResourceAndRenderPrompt(t *testing.T) {
	srv := newTestServer(t, mcpActionHost{})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/mcp/resources/read", "", `{"server":"filesystem","uri":"file:///workspace/README.md"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	resp = postJSON(t, ts.URL+"/v1/mcp/resources/read", "token", `{"server":"filesystem","uri":"file:///workspace/README.md"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var read MCPResourceReadResponse
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		t.Fatal(err)
	}
	if read.Server != "filesystem" || read.URI != "file:///workspace/README.md" || len(read.Contents) != 1 || read.Contents[0].Text == nil {
		t.Fatalf("read = %+v", read)
	}

	resp = postJSON(t, ts.URL+"/v1/mcp/prompts/get", "token", `{"server":"linear","name":"daily_brief","arguments":{"topic":"Go"}}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var rendered MCPPromptRenderResponse
	if err := json.NewDecoder(resp.Body).Decode(&rendered); err != nil {
		t.Fatal(err)
	}
	if rendered.Server != "linear" || rendered.Name != "daily_brief" || len(rendered.Messages) != 1 || !strings.Contains(string(rendered.Messages[0].Content), "Brief me on Go.") {
		t.Fatalf("rendered = %+v", rendered)
	}

	resp = getJSON(t, ts.URL+"/v1/status", "token")
	defer resp.Body.Close()
	var status statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mcp_resource_read", "mcp_prompt_get"} {
		if !contains(status.Capabilities, want) {
			t.Fatalf("capabilities = %v, missing %q", status.Capabilities, want)
		}
	}
}

func TestServerMCPActionValidation(t *testing.T) {
	srv := newTestServer(t, mcpActionHost{})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/mcp/resources/read", "token", `{"server":"filesystem"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("read validation status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	resp = postJSON(t, ts.URL+"/v1/mcp/prompts/get", "token", `{"server":"linear"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("prompt validation status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestServerStatus(t *testing.T) {
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: scriptedProvider{},
		Tools: []glue.Tool{{
			ToolSpec: glue.ToolSpec{Name: "demo_tool"},
		}},
	})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := getJSON(t, ts.URL+"/v1/status", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	resp = getJSON(t, ts.URL+"/v1/status", "token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var status statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.OK || status.Version != protocolVersion || status.ActiveRuns != 0 || status.ToolsCount != 1 {
		t.Fatalf("status = %+v", status)
	}
	for _, want := range []string{"runs", "events", "permissions", "tools", "status"} {
		if !contains(status.Capabilities, want) {
			t.Fatalf("capabilities = %v, missing %q", status.Capabilities, want)
		}
	}
}

func TestServerStatusCountsActiveRuns(t *testing.T) {
	provider := &blockingProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	agent := glue.NewAgent(glue.AgentOptions{Provider: provider})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	start := startRun(t, ts.URL, "default")
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for run start")
	}

	resp := getJSON(t, ts.URL+"/v1/status", "token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var status statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.ActiveRuns != 1 {
		t.Fatalf("active_runs = %d, want 1", status.ActiveRuns)
	}

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/v1/runs/"+start.RunID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	cancelResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancelResp.Body.Close()
	select {
	case <-provider.canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancellation")
	}
}

func TestServerStartsRunAndStreamsEvents(t *testing.T) {
	agent := glue.NewAgent(glue.AgentOptions{Provider: scriptedProvider{events: []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: "hello"},
		{Type: glue.ProviderEventDone},
	}}})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/sessions/default/runs", "token", `{"text":"say hi","client_id":"cli:test"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	var start startRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	if start.RunID == "" || start.SessionID != "default" || !strings.HasSuffix(start.EventsURL, "/events") {
		t.Fatalf("start response = %+v", start)
	}

	events := getSSE(t, ts.URL+start.EventsURL, "token")
	types := eventTypes(events)
	for _, want := range []string{"run_start", "loop_start", "turn_start", "message_start", "text_delta", "message_end", "turn_end", "loop_end", "run_done"} {
		if !contains(types, want) {
			t.Fatalf("events = %v, missing %s", types, want)
		}
	}
	if types[0] != "run_start" || types[len(types)-1] != "run_done" {
		t.Fatalf("events = %v, want run_start ... run_done", types)
	}
	for i, event := range events {
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, event.Seq, i+1)
		}
		if event.RunID != start.RunID || event.SessionID != "default" {
			t.Fatalf("event = %+v, want run/session ids", event)
		}
	}
}

func TestServerStartsSkillRunAndStreamsEvents(t *testing.T) {
	workDir := t.TempDir()
	skillDir := filepath.Join(workDir, ".agents", "skills", "triage")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: triage\ndescription: Triage one issue\n---\nInvestigate and summarize."), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &captureProvider{text: "triaged"}
	agent := glue.NewAgent(glue.AgentOptions{Provider: provider, WorkDir: workDir})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/sessions/default/runs", "token", `{"skill":"triage","arguments":{"issue":"GLUE-123"},"client_id":"cli:test"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	var start startRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}

	events := getSSE(t, ts.URL+start.EventsURL, "token")
	types := eventTypes(events)
	if types[0] != "run_start" || types[len(types)-1] != "run_done" || !contains(types, "text_delta") {
		t.Fatalf("events = %v, want streamed skill run", types)
	}
	provider.mu.Lock()
	requests := append([]glue.ProviderRequest(nil), provider.requests...)
	provider.mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(requests))
	}
	prompt := requests[0].Messages[0].Content[0].Text
	for _, want := range []string{"Investigate and summarize.", `"issue": "GLUE-123"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, missing %q", prompt, want)
		}
	}
}

func TestServerStartRunValidation(t *testing.T) {
	agent := glue.NewAgent(glue.AgentOptions{Provider: scriptedProvider{}})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/sessions/default/runs", "token", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty run status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	resp = postJSON(t, ts.URL+"/v1/sessions/default/runs", "token", `{"text":"hi","skill":"triage"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mixed run status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestServerCancelRun(t *testing.T) {
	provider := &blockingProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	agent := glue.NewAgent(glue.AgentOptions{Provider: provider})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/sessions/default/runs", "token", `{"text":"wait"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start status = %d", resp.StatusCode)
	}
	var start startRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/v1/runs/"+start.RunID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	cancelResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want %d", cancelResp.StatusCode, http.StatusAccepted)
	}
	select {
	case <-provider.canceled:
	case <-time.After(time.Second):
		t.Fatal("provider was not canceled")
	}

	events := getSSE(t, ts.URL+start.EventsURL, "token")
	types := eventTypes(events)
	if types[len(types)-1] != "run_error" {
		t.Fatalf("events = %v, want terminal run_error", types)
	}
}

func TestServerPermissionAllowOnce(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{toolCallTurn(), textTurn("done")}},
		Tools:    []glue.Tool{permissionTool(&executed)},
	})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	start := startRun(t, ts.URL, "default")
	events, err := collectSSE(t, ts.URL+start.EventsURL, "token", func(event EventEnvelope) {
		if event.Type == "permission_request" {
			postDecision(t, ts.URL, start.RunID, permissionID(t, event), "token", `{"allow":true}`)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if executed.Load() != 1 {
		t.Fatalf("tool executions = %d, want 1", executed.Load())
	}
	types := eventTypes(events)
	if !contains(types, "permission_request") || types[len(types)-1] != "run_done" {
		t.Fatalf("events = %v, want permission_request and terminal run_done", types)
	}
}

func TestServerPermissionDenyIsModelVisibleToolError(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{toolCallTurn(), textTurn("done")}},
		Tools:    []glue.Tool{permissionTool(&executed)},
	})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	start := startRun(t, ts.URL, "default")
	events, err := collectSSE(t, ts.URL+start.EventsURL, "token", func(event EventEnvelope) {
		if event.Type == "permission_request" {
			postDecision(t, ts.URL, start.RunID, permissionID(t, event), "token", `{"allow":false,"reason":"not now"}`)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if executed.Load() != 0 {
		t.Fatalf("tool executions = %d, want 0", executed.Load())
	}
	if text := toolEndText(t, events); !strings.Contains(text, "not now") {
		t.Fatalf("tool_end text = %q, want denial reason", text)
	}
}

func TestServerPermissionDecisionRejectsInvalidRequests(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{toolCallTurn(), textTurn("done")}},
		Tools:    []glue.Tool{permissionTool(&executed)},
	})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	start := startRun(t, ts.URL, "default")
	events, err := collectSSE(t, ts.URL+start.EventsURL, "token", func(event EventEnvelope) {
		if event.Type != "permission_request" {
			return
		}
		id := permissionID(t, event)
		postDecisionStatus(t, ts.URL, start.RunID, id, "token", "cli:other", `{"allow":true}`, http.StatusForbidden)
		postDecisionStatus(t, ts.URL, start.RunID, id, "token", "", `{"allow":true,"remember_for":"workspace"}`, http.StatusBadRequest)
		postDecisionStatus(t, ts.URL, start.RunID, "perm_missing", "token", "", `{"allow":true}`, http.StatusNotFound)
		postDecisionStatus(t, ts.URL, start.RunID, id, "token", "cli:test", `{"allow":true}`, http.StatusOK)
	})
	if err != nil {
		t.Fatal(err)
	}
	if executed.Load() != 1 {
		t.Fatalf("tool executions = %d, want 1", executed.Load())
	}
	if types := eventTypes(events); types[len(types)-1] != "run_done" {
		t.Fatalf("events = %v, want terminal run_done", types)
	}
}

func TestServerPermissionTimeoutDenies(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{toolCallTurn(), textTurn("done")}},
		Tools:    []glue.Tool{permissionTool(&executed)},
	})
	srv := newTestServerWithTimeout(t, agent, 10*time.Millisecond)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	start := startRun(t, ts.URL, "default")
	events := getSSE(t, ts.URL+start.EventsURL, "token")
	if executed.Load() != 0 {
		t.Fatalf("tool executions = %d, want 0", executed.Load())
	}
	types := eventTypes(events)
	if !contains(types, "permission_request") || types[len(types)-1] != "run_done" {
		t.Fatalf("events = %v, want timed-out permission and run_done", types)
	}
	if text := toolEndText(t, events); !strings.Contains(text, "timed out") {
		t.Fatalf("tool_end text = %q, want timeout reason", text)
	}
	var timedOutPermissionID string
	for _, event := range events {
		if event.Type == "permission_request" {
			timedOutPermissionID = permissionID(t, event)
			break
		}
	}
	if timedOutPermissionID == "" {
		t.Fatal("timed-out run did not emit permission_request")
	}
	postDecisionStatus(t, ts.URL, start.RunID, timedOutPermissionID, "token", "", `{"allow":true}`, http.StatusNotFound)
}

func TestServerPermissionRememberSession(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{
			toolCallTurnValue("first-target"), textTurn("first done"),
			toolCallTurnValue("second-target"), textTurn("second done"),
		}},
		Tools: []glue.Tool{permissionTool(&executed)},
	})
	srv := newTestServerWithTimeout(t, agent, 50*time.Millisecond)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	first := startRun(t, ts.URL, "default")
	firstEvents, err := collectSSE(t, ts.URL+first.EventsURL, "token", func(event EventEnvelope) {
		if event.Type == "permission_request" {
			postDecision(t, ts.URL, first.RunID, permissionID(t, event), "token", `{"allow":true,"remember_for":"session"}`)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(eventTypes(firstEvents), "permission_request") {
		t.Fatal("first run did not ask permission")
	}

	second := startRun(t, ts.URL, "default")
	secondEvents := getSSE(t, ts.URL+second.EventsURL, "token")
	if executed.Load() != 2 {
		t.Fatalf("tool executions = %d, want 2", executed.Load())
	}
	if contains(eventTypes(secondEvents), "permission_request") {
		t.Fatalf("second run events = %v, want cached session allow", eventTypes(secondEvents))
	}
}

func TestServerPermissionRememberTarget(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{
			toolCallTurn(), textTurn("first done"),
			toolCallTurn(), textTurn("second done"),
		}},
		Tools: []glue.Tool{permissionTool(&executed)},
	})
	srv := newTestServerWithTimeout(t, agent, 50*time.Millisecond)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	first := startRun(t, ts.URL, "default")
	firstEvents, err := collectSSE(t, ts.URL+first.EventsURL, "token", func(event EventEnvelope) {
		if event.Type == "permission_request" {
			postDecision(t, ts.URL, first.RunID, permissionID(t, event), "token", `{"allow":true,"remember_for":"session_target"}`)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(eventTypes(firstEvents), "permission_request") {
		t.Fatal("first run did not ask permission")
	}

	second := startRun(t, ts.URL, "default")
	secondEvents := getSSE(t, ts.URL+second.EventsURL, "token")
	if executed.Load() != 2 {
		t.Fatalf("tool executions = %d, want 2", executed.Load())
	}
	if contains(eventTypes(secondEvents), "permission_request") {
		t.Fatalf("second run events = %v, want cached target allow", eventTypes(secondEvents))
	}
}

func TestServerPermissionRememberForeverAcrossSessions(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{
			toolCallTurn(), textTurn("first done"),
			toolCallTurn(), textTurn("second done"),
		}},
		Tools: []glue.Tool{permissionTool(&executed)},
	})
	srv := newTestServerWithTimeout(t, agent, 50*time.Millisecond)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	first := startRun(t, ts.URL, "default")
	firstEvents, err := collectSSE(t, ts.URL+first.EventsURL, "token", func(event EventEnvelope) {
		if event.Type == "permission_request" {
			postDecision(t, ts.URL, first.RunID, permissionID(t, event), "token", `{"allow":true,"remember_for":"forever"}`)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(eventTypes(firstEvents), "permission_request") {
		t.Fatal("first run did not ask permission")
	}

	second := startRun(t, ts.URL, "other-session")
	secondEvents := getSSE(t, ts.URL+second.EventsURL, "token")
	if executed.Load() != 2 {
		t.Fatalf("tool executions = %d, want 2", executed.Load())
	}
	if contains(eventTypes(secondEvents), "permission_request") {
		t.Fatalf("second run events = %v, want cached forever allow", eventTypes(secondEvents))
	}
}

func TestServerPermissionPolicyAllowSkipsPrompt(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{
			toolCallTurn(), textTurn("done"),
		}},
		Tools: []glue.Tool{permissionTool(&executed)},
	})
	var observed PermissionContext
	srv := newTestServerWithPolicy(t, agent, PermissionPolicyFunc(func(_ context.Context, info PermissionContext, req glue.PermissionRequest) (PermissionPolicyDecision, error) {
		observed = info
		if req.Tool != "side_effect" || req.Action != "touch" || req.SessionID != "default" {
			t.Fatalf("permission request = %+v", req)
		}
		return PermissionPolicyDecision{Action: PermissionPolicyAllow}, nil
	}))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	start := startRunWithClient(t, ts.URL, "default", "cli:test")
	events := getSSE(t, ts.URL+start.EventsURL, "token")
	if observed.ClientID != "cli:test" || observed.SessionID != "default" || observed.RunID == "" {
		t.Fatalf("permission context = %+v", observed)
	}
	if executed.Load() != 1 {
		t.Fatalf("tool executions = %d, want 1", executed.Load())
	}
	if contains(eventTypes(events), "permission_request") {
		t.Fatalf("events = %v, want no permission_request", eventTypes(events))
	}
}

func TestServerPermissionPolicyDenySkipsPrompt(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{
			toolCallTurn(), textTurn("done"),
		}},
		Tools: []glue.Tool{permissionTool(&executed)},
	})
	srv := newTestServerWithPolicy(t, agent, PermissionPolicyFunc(func(context.Context, PermissionContext, glue.PermissionRequest) (PermissionPolicyDecision, error) {
		return PermissionPolicyDecision{Action: PermissionPolicyDeny, Reason: "permission denied: read-only channel"}, nil
	}))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	start := startRunWithClient(t, ts.URL, "default", "telegram:123")
	events := getSSE(t, ts.URL+start.EventsURL, "token")
	if executed.Load() != 0 {
		t.Fatalf("tool executions = %d, want 0", executed.Load())
	}
	if contains(eventTypes(events), "permission_request") {
		t.Fatalf("events = %v, want no permission_request", eventTypes(events))
	}
	if got := toolEndText(t, events); !strings.Contains(got, "read-only channel") {
		t.Fatalf("tool error = %q, want policy reason", got)
	}
}

func TestServerPermissionPolicyPromptFallsThrough(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{
			toolCallTurn(), textTurn("done"),
		}},
		Tools: []glue.Tool{permissionTool(&executed)},
	})
	srv := newTestServerWithPolicy(t, agent, PermissionPolicyFunc(func(context.Context, PermissionContext, glue.PermissionRequest) (PermissionPolicyDecision, error) {
		return PermissionPolicyDecision{Action: PermissionPolicyPrompt}, nil
	}))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	start := startRunWithClient(t, ts.URL, "default", "cli:test")
	events, err := collectSSE(t, ts.URL+start.EventsURL, "token", func(event EventEnvelope) {
		if event.Type == "permission_request" {
			postDecision(t, ts.URL, start.RunID, permissionID(t, event), "token", `{"allow":true}`)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if executed.Load() != 1 {
		t.Fatalf("tool executions = %d, want 1", executed.Load())
	}
	if !contains(eventTypes(events), "permission_request") {
		t.Fatalf("events = %v, want permission_request", eventTypes(events))
	}
}

func TestServerPermissionRememberForeverIsClientScoped(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{
		Provider: &turnProvider{turns: [][]glue.ProviderEvent{
			toolCallTurn(), textTurn("first done"),
			toolCallTurn(), textTurn("second done"),
		}},
		Tools: []glue.Tool{permissionTool(&executed)},
	})
	srv := newTestServerWithTimeout(t, agent, 50*time.Millisecond)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	first := startRunWithClient(t, ts.URL, "telegram:123", "telegram:123")
	firstEvents, err := collectSSE(t, ts.URL+first.EventsURL, "token", func(event EventEnvelope) {
		if event.Type == "permission_request" {
			postDecision(t, ts.URL, first.RunID, permissionID(t, event), "token", `{"allow":true,"remember_for":"forever"}`)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(eventTypes(firstEvents), "permission_request") {
		t.Fatal("first run did not ask permission")
	}

	second := startRunWithClient(t, ts.URL, "default", "cli:test")
	secondEvents, err := collectSSE(t, ts.URL+second.EventsURL, "token", func(event EventEnvelope) {
		if event.Type == "permission_request" {
			postDecision(t, ts.URL, second.RunID, permissionID(t, event), "token", `{"allow":true}`)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if executed.Load() != 2 {
		t.Fatalf("tool executions = %d, want 2", executed.Load())
	}
	if !contains(eventTypes(secondEvents), "permission_request") {
		t.Fatalf("second run events = %v, want client-scoped cache miss", eventTypes(secondEvents))
	}
}

func newTestServer(t *testing.T, host Host) *Server {
	return newTestServerWithTimeout(t, host, time.Second)
}

func newTestServerWithPolicy(t *testing.T, host Host, policy PermissionPolicy) *Server {
	t.Helper()
	now := time.Date(2026, 5, 23, 20, 46, 0, 0, time.UTC)
	srv, err := New(Options{
		Host:              host,
		Token:             "token",
		PermissionPolicy:  policy,
		Now:               func() time.Time { return now },
		NewID:             sequenceIDs(),
		PermissionTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func newTestServerWithTimeout(t *testing.T, host Host, timeout time.Duration) *Server {
	t.Helper()
	now := time.Date(2026, 5, 23, 20, 46, 0, 0, time.UTC)
	srv, err := New(Options{
		Host:              host,
		Token:             "token",
		Now:               func() time.Time { return now },
		NewID:             sequenceIDs(),
		PermissionTimeout: timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func sequenceIDs() func(prefix string) string {
	var mu sync.Mutex
	var n int
	return func(prefix string) string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("%s_%02d", prefix, n)
	}
}

func postJSON(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getJSON(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func startRun(t *testing.T, baseURL, sessionID string) startRunResponse {
	t.Helper()
	return startRunWithClient(t, baseURL, sessionID, "cli:test")
}

func startRunWithClient(t *testing.T, baseURL, sessionID, clientID string) startRunResponse {
	t.Helper()
	body := fmt.Sprintf(`{"text":"go","client_id":%s}`, strconv.Quote(clientID))
	resp := postJSON(t, baseURL+"/v1/sessions/"+sessionID+"/runs", "token", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	var start startRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	return start
}

func getSSE(t *testing.T, url, token string) []EventEnvelope {
	t.Helper()
	events, err := collectSSE(t, url, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func collectSSE(t *testing.T, url, token string, onEvent func(EventEnvelope)) ([]EventEnvelope, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SSE status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var events []EventEnvelope
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event EventEnvelope
		dec := json.NewDecoder(bytes.NewBufferString(strings.TrimPrefix(line, "data: ")))
		dec.UseNumber()
		if err := dec.Decode(&event); err != nil {
			return nil, err
		}
		events = append(events, event)
		if onEvent != nil {
			onEvent(event)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, errors.New("no SSE events")
	}
	return events, nil
}

func eventTypes(events []EventEnvelope) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Type)
	}
	return out
}

func contains(in []string, want string) bool {
	for _, got := range in {
		if got == want {
			return true
		}
	}
	return false
}

func toolCallTurn() []glue.ProviderEvent {
	return toolCallTurnValue("x")
}

func toolCallTurnValue(value string) []glue.ProviderEvent {
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "c1", Name: "side_effect", Arguments: []byte(`{"value":` + strconv.Quote(value) + `}`)}},
		{Type: glue.ProviderEventDone},
	}
}

func textTurn(text string) []glue.ProviderEvent {
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: text},
		{Type: glue.ProviderEventDone},
	}
}

func permissionTool(counter *atomic.Int32) glue.Tool {
	return glue.NewTool[struct {
		Value string `json:"value"`
	}](glue.ToolSpec{
		Name:               "side_effect",
		RequiresPermission: true,
		PermissionAction:   "touch",
		PermissionTarget: func(call glue.ToolCall) string {
			var args struct {
				Value string `json:"value"`
			}
			_ = json.Unmarshal(call.Arguments, &args)
			return args.Value
		},
	}, func(context.Context, struct {
		Value string `json:"value"`
	}) (glue.ToolResult, error) {
		counter.Add(1)
		return glue.TextResult("touched"), nil
	})
}

func permissionID(t *testing.T, event EventEnvelope) string {
	t.Helper()
	payload, ok := event.Payload.(map[string]any)
	if !ok {
		t.Fatalf("permission payload = %#v", event.Payload)
	}
	id, ok := payload["permission_id"].(string)
	if !ok || id == "" {
		t.Fatalf("permission payload missing id: %#v", payload)
	}
	return id
}

func postDecision(t *testing.T, baseURL, runID, permissionID, token, body string) {
	t.Helper()
	postDecisionStatus(t, baseURL, runID, permissionID, token, "", body, http.StatusOK)
}

func postDecisionStatus(t *testing.T, baseURL, runID, permissionID, token, clientID, body string, want int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/runs/"+runID+"/permissions/"+permissionID+"/decision", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if clientID != "" {
		req.Header.Set("X-Glue-Client-ID", clientID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("decision status = %d, want %d", resp.StatusCode, want)
	}
}

func toolEndText(t *testing.T, events []EventEnvelope) string {
	t.Helper()
	for _, event := range events {
		if event.Type != "tool_end" {
			continue
		}
		payload, ok := event.Payload.(map[string]any)
		if !ok {
			t.Fatalf("tool_end payload = %#v", event.Payload)
		}
		result, ok := payload["tool_result"].(map[string]any)
		if !ok {
			t.Fatalf("tool_end missing result: %#v", payload)
		}
		if isError, _ := result["is_error"].(bool); !isError {
			t.Fatalf("tool_end result = %#v, want is_error", result)
		}
		content, ok := result["content"].([]any)
		if !ok || len(content) == 0 {
			t.Fatalf("tool_end result missing content: %#v", result)
		}
		part, ok := content[0].(map[string]any)
		if !ok {
			t.Fatalf("tool_end content = %#v", content[0])
		}
		text, _ := part["text"].(string)
		return text
	}
	t.Fatal("missing tool_end event")
	return ""
}
