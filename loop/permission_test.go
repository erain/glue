package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type recordingPermission struct {
	requests []PermissionRequest
	decision PermissionDecision
	err      error
}

func (p *recordingPermission) Decide(_ context.Context, req PermissionRequest) (PermissionDecision, error) {
	p.requests = append(p.requests, req)
	if p.err != nil {
		return PermissionDecision{}, p.err
	}
	return p.decision, nil
}

type hookFuncs struct {
	pre  func(context.Context, ToolCall) error
	post func(context.Context, ToolCall, *ToolResult) error
}

func (h hookFuncs) PreTool(ctx context.Context, call ToolCall) error {
	if h.pre == nil {
		return nil
	}
	return h.pre(ctx, call)
}

func (h hookFuncs) PostTool(ctx context.Context, call ToolCall, result *ToolResult) error {
	if h.post == nil {
		return nil
	}
	return h.post(ctx, call, result)
}

func runPermissionCase(t *testing.T, req RunRequest) RunResult {
	t.Helper()
	if req.Provider == nil {
		req.Provider = scriptTwoToolCallsThenStop(ToolCall{ID: "c1", Name: "side", Arguments: json.RawMessage(`{"path":"a.txt"}`)})
	}
	res, err := Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	return res
}

func sideTool(executed *bool) Tool {
	return Tool{
		ToolSpec: ToolSpec{
			Name:               "side",
			RequiresPermission: true,
			PermissionAction:   "write_file",
			PermissionTarget: func(call ToolCall) string {
				var args map[string]string
				_ = json.Unmarshal(call.Arguments, &args)
				return args["path"]
			},
		},
		Execute: func(context.Context, ToolCall) (ToolResult, error) {
			if executed != nil {
				*executed = true
			}
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: "ran"}}}, nil
		},
	}
}

func TestPermissionAllowBuildsRequestAndRuns(t *testing.T) {
	t.Parallel()

	var executed bool
	perm := &recordingPermission{decision: PermissionDecision{Allow: true, RememberFor: RememberSession}}
	res := runPermissionCase(t, RunRequest{
		Provider:   scriptTwoToolCallsThenStop(ToolCall{ID: "c1", Name: "side", Arguments: json.RawMessage(`{"path":"a.txt"}`)}),
		Tools:      []Tool{sideTool(&executed)},
		SessionID:  "session-1",
		Permission: perm,
	})

	if !executed {
		t.Fatal("tool did not execute")
	}
	if got := res.NewMessages[1].Content[0].Text; got != "ran" {
		t.Fatalf("tool result = %q, want ran", got)
	}
	if len(perm.requests) != 1 {
		t.Fatalf("permission requests = %d, want 1", len(perm.requests))
	}
	req := perm.requests[0]
	if req.Tool != "side" || req.Action != "write_file" || req.Target != "a.txt" || req.SessionID != "session-1" {
		t.Fatalf("permission request = %+v", req)
	}
	if got := string(req.Args); got != `{"path":"a.txt"}` {
		t.Fatalf("args = %q, want normalized object", got)
	}
}

func TestPermissionNilDeniesSideEffectTool(t *testing.T) {
	t.Parallel()

	var executed bool
	res := runPermissionCase(t, RunRequest{Tools: []Tool{sideTool(&executed)}})

	if executed {
		t.Fatal("tool executed without permission")
	}
	tool := res.NewMessages[1]
	if !tool.IsError {
		t.Fatal("IsError = false, want true")
	}
	if !strings.Contains(tool.Content[0].Text, "no permission handler") {
		t.Fatalf("content = %q, want no permission handler", tool.Content[0].Text)
	}
}

func TestReadOnlyToolBypassesNilPermission(t *testing.T) {
	t.Parallel()

	var executed bool
	readOnly := Tool{
		ToolSpec: ToolSpec{Name: "side"},
		Execute: func(context.Context, ToolCall) (ToolResult, error) {
			executed = true
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: "read"}}}, nil
		},
	}
	res := runPermissionCase(t, RunRequest{Tools: []Tool{readOnly}})

	if !executed {
		t.Fatal("read-only tool did not execute")
	}
	if res.NewMessages[1].IsError {
		t.Fatalf("read-only tool returned error: %q", res.NewMessages[1].Content[0].Text)
	}
}

func TestPermissionDenyReturnsErrorAndRunsPostHook(t *testing.T) {
	t.Parallel()

	var executed bool
	var postCalled bool
	res := runPermissionCase(t, RunRequest{
		Tools:      []Tool{sideTool(&executed)},
		Permission: DenyAll{Reason: "not allowed"},
		Hooks: []Hook{hookFuncs{post: func(_ context.Context, _ ToolCall, result *ToolResult) error {
			postCalled = true
			result.Content[0].Text += " (post)"
			result.Metadata = map[string]any{"post": true}
			return nil
		}}},
	})

	if executed {
		t.Fatal("denied tool executed")
	}
	if !postCalled {
		t.Fatal("post hook did not run")
	}
	tool := res.NewMessages[1]
	if !tool.IsError {
		t.Fatal("IsError = false, want true")
	}
	if got, want := tool.Content[0].Text, "not allowed (post)"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if tool.Metadata["post"] != true {
		t.Fatalf("metadata = %#v, want post marker", tool.Metadata)
	}
}

func TestHooksRunInOrderAroundTool(t *testing.T) {
	t.Parallel()

	var order []string
	h1 := hookFuncs{
		pre: func(context.Context, ToolCall) error {
			order = append(order, "pre1")
			return nil
		},
		post: func(_ context.Context, _ ToolCall, result *ToolResult) error {
			order = append(order, "post1")
			result.Content[0].Text += ":post1"
			return nil
		},
	}
	h2 := hookFuncs{
		pre: func(context.Context, ToolCall) error {
			order = append(order, "pre2")
			return nil
		},
		post: func(_ context.Context, _ ToolCall, result *ToolResult) error {
			order = append(order, "post2")
			result.Content[0].Text += ":post2"
			return nil
		},
	}
	tool := sideTool(nil)
	tool.Execute = func(context.Context, ToolCall) (ToolResult, error) {
		order = append(order, "execute")
		return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: "base"}}}, nil
	}

	res := runPermissionCase(t, RunRequest{
		Tools:      []Tool{tool},
		Permission: AllowAll{},
		Hooks:      []Hook{h1, h2},
	})

	wantOrder := []string{"pre1", "pre2", "execute", "post2", "post1"}
	if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("order = %v, want %v", order, wantOrder)
	}
	if got, want := res.NewMessages[1].Content[0].Text, "base:post2:post1"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestHookSkipTool(t *testing.T) {
	t.Parallel()

	var executed bool
	var permissionCalled bool
	var postCalled bool
	perm := permissionFunc(func(context.Context, PermissionRequest) (PermissionDecision, error) {
		permissionCalled = true
		return PermissionDecision{Allow: true}, nil
	})
	res := runPermissionCase(t, RunRequest{
		Tools:      []Tool{sideTool(&executed)},
		Permission: perm,
		Hooks: []Hook{hookFuncs{
			pre:  func(context.Context, ToolCall) error { return ErrSkipTool },
			post: func(context.Context, ToolCall, *ToolResult) error { postCalled = true; return nil },
		}},
	})

	if executed || permissionCalled || postCalled {
		t.Fatalf("executed=%v permissionCalled=%v postCalled=%v, want all false", executed, permissionCalled, postCalled)
	}
	tool := res.NewMessages[1]
	if !tool.IsError || tool.Content[0].Text != "tool skipped by hook" {
		t.Fatalf("tool result = %+v, want skipped error", tool)
	}
}

func TestHookAbortErrorsRun(t *testing.T) {
	t.Parallel()

	want := errors.New("pre failed")
	_, err := Run(context.Background(), RunRequest{
		Provider: scriptTwoToolCallsThenStop(ToolCall{ID: "c1", Name: "side", Arguments: json.RawMessage(`{}`)}),
		Tools:    []Tool{sideTool(nil)},
		Hooks:    []Hook{hookFuncs{pre: func(context.Context, ToolCall) error { return want }}},
	})
	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
}

func TestPostHookAbortErrorsRun(t *testing.T) {
	t.Parallel()

	want := errors.New("post failed")
	_, err := Run(context.Background(), RunRequest{
		Provider:   scriptTwoToolCallsThenStop(ToolCall{ID: "c1", Name: "side", Arguments: json.RawMessage(`{}`)}),
		Tools:      []Tool{sideTool(nil)},
		Permission: AllowAll{},
		Hooks:      []Hook{hookFuncs{post: func(context.Context, ToolCall, *ToolResult) error { return want }}},
	})
	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
}

func TestToolSpecPermissionMetadataExcludedFromJSON(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(ToolSpec{
		Name:               "side",
		Description:        "desc",
		Parameters:         json.RawMessage(`{"type":"object"}`),
		RequiresPermission: true,
		PermissionAction:   "exec",
		PermissionTarget:   func(ToolCall) string { return "target" },
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, "RequiresPermission") || strings.Contains(s, "PermissionAction") || strings.Contains(s, "exec") {
		t.Fatalf("marshaled ToolSpec leaked permission metadata: %s", s)
	}
}

type permissionFunc func(context.Context, PermissionRequest) (PermissionDecision, error)

func (f permissionFunc) Decide(ctx context.Context, req PermissionRequest) (PermissionDecision, error) {
	return f(ctx, req)
}
