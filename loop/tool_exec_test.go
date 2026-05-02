package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// scriptTwoToolCallsThenStop returns a fake provider whose first turn issues
// two tool calls in the given order and whose second turn returns a final
// "ok" text response.
func scriptTwoToolCallsThenStop(calls ...ToolCall) *fakeProvider {
	first := []ProviderEvent{{Type: ProviderEventStart}}
	for i := range calls {
		c := calls[i]
		first = append(first, ProviderEvent{Type: ProviderEventToolCall, ToolCall: &c})
	}
	first = append(first, ProviderEvent{Type: ProviderEventDone})
	return &fakeProvider{turns: [][]ProviderEvent{
		first,
		{
			{Type: ProviderEventStart},
			{Type: ProviderEventTextDelta, Delta: "ok"},
			{Type: ProviderEventDone},
		},
	}}
}

func runWithTools(t *testing.T, p Provider, tools ...Tool) RunResult {
	t.Helper()
	res, err := Run(context.Background(), RunRequest{Provider: p, Tools: tools})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	return res
}

func TestExecToolValidArgsHappyPath(t *testing.T) {
	t.Parallel()

	provider := scriptTwoToolCallsThenStop(
		ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"v":"hi"}`)},
	)
	echo := Tool{
		ToolSpec: ToolSpec{Name: "echo"},
		Execute: func(_ context.Context, call ToolCall) (ToolResult, error) {
			var args map[string]string
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return ToolResult{}, err
			}
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: args["v"]}}}, nil
		},
	}
	res := runWithTools(t, provider, echo)

	tool := res.NewMessages[1]
	if tool.Role != MessageRoleTool || tool.IsError {
		t.Fatalf("tool message = %+v, want non-error tool message", tool)
	}
	if got := tool.Content[0].Text; got != "hi" {
		t.Fatalf("content = %q, want %q", got, "hi")
	}
}

func TestExecToolInvalidJSONArgs(t *testing.T) {
	t.Parallel()

	provider := scriptTwoToolCallsThenStop(
		ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`not-json`)},
	)
	echo := Tool{
		ToolSpec: ToolSpec{Name: "echo"},
		Execute:  func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil },
	}
	res := runWithTools(t, provider, echo)

	tool := res.NewMessages[1]
	if !tool.IsError {
		t.Fatalf("IsError = false, want true; content=%q", tool.Content[0].Text)
	}
	if !strings.Contains(tool.Content[0].Text, "invalid arguments") {
		t.Fatalf("content = %q, want 'invalid arguments'", tool.Content[0].Text)
	}
}

func TestExecToolNonObjectArgs(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"array":  `[1,2,3]`,
		"null":   `null`,
		"number": `42`,
		"string": `"oops"`,
	}
	for name, args := range cases {
		args := args
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			provider := scriptTwoToolCallsThenStop(
				ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(args)},
			)
			echo := Tool{ToolSpec: ToolSpec{Name: "echo"}, Execute: func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, nil }}
			res := runWithTools(t, provider, echo)
			tool := res.NewMessages[1]
			if !tool.IsError {
				t.Fatalf("IsError = false, want true; content=%q", tool.Content[0].Text)
			}
			if !strings.Contains(tool.Content[0].Text, "invalid arguments for tool \"echo\"") {
				t.Fatalf("content = %q, want phrase 'invalid arguments for tool \"echo\"'", tool.Content[0].Text)
			}
		})
	}
}

func TestExecToolUnknownTool(t *testing.T) {
	t.Parallel()

	provider := scriptTwoToolCallsThenStop(
		ToolCall{ID: "c1", Name: "ghost", Arguments: json.RawMessage(`{}`)},
	)
	res := runWithTools(t, provider /* no tools */)

	tool := res.NewMessages[1]
	if !tool.IsError {
		t.Fatalf("IsError = false, want true")
	}
	if !strings.Contains(tool.Content[0].Text, `unknown tool "ghost"`) {
		t.Fatalf("content = %q, want unknown tool", tool.Content[0].Text)
	}
}

func TestExecToolMissingExecutor(t *testing.T) {
	t.Parallel()

	provider := scriptTwoToolCallsThenStop(
		ToolCall{ID: "c1", Name: "stub", Arguments: json.RawMessage(`{}`)},
	)
	stub := Tool{ToolSpec: ToolSpec{Name: "stub"}, Execute: nil}
	res := runWithTools(t, provider, stub)

	tool := res.NewMessages[1]
	if !tool.IsError {
		t.Fatalf("IsError = false, want true")
	}
	if !strings.Contains(tool.Content[0].Text, "no executor") {
		t.Fatalf("content = %q, want 'no executor'", tool.Content[0].Text)
	}
}

func TestExecToolExecutorError(t *testing.T) {
	t.Parallel()

	provider := scriptTwoToolCallsThenStop(
		ToolCall{ID: "c1", Name: "fail", Arguments: json.RawMessage(`{}`)},
	)
	fail := Tool{
		ToolSpec: ToolSpec{Name: "fail"},
		Execute:  func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{}, errors.New("kaboom") },
	}
	res := runWithTools(t, provider, fail)

	tool := res.NewMessages[1]
	if !tool.IsError {
		t.Fatalf("IsError = false, want true")
	}
	if tool.Content[0].Text != "kaboom" {
		t.Fatalf("content = %q, want 'kaboom'", tool.Content[0].Text)
	}
}

func TestExecToolEmptyArgsDefaultToObject(t *testing.T) {
	t.Parallel()

	var seen json.RawMessage
	provider := scriptTwoToolCallsThenStop(
		ToolCall{ID: "c1", Name: "noop"}, // no arguments at all
	)
	noop := Tool{
		ToolSpec: ToolSpec{Name: "noop"},
		Execute: func(_ context.Context, call ToolCall) (ToolResult, error) {
			seen = call.Arguments
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: "done"}}}, nil
		},
	}
	res := runWithTools(t, provider, noop)

	if string(seen) != "{}" {
		t.Fatalf("executor saw arguments = %q, want \"{}\"", string(seen))
	}
	tool := res.NewMessages[1]
	if tool.IsError {
		t.Fatalf("IsError = true, want false; content=%q", tool.Content[0].Text)
	}
}

func TestExecToolPreservesAssistantSourceOrder(t *testing.T) {
	t.Parallel()

	provider := scriptTwoToolCallsThenStop(
		ToolCall{ID: "c1", Name: "tag", Arguments: json.RawMessage(`{"n":"first"}`)},
		ToolCall{ID: "c2", Name: "tag", Arguments: json.RawMessage(`{"n":"second"}`)},
		ToolCall{ID: "c3", Name: "tag", Arguments: json.RawMessage(`{"n":"third"}`)},
	)
	tag := Tool{
		ToolSpec: ToolSpec{Name: "tag"},
		Execute: func(_ context.Context, call ToolCall) (ToolResult, error) {
			var a map[string]string
			_ = json.Unmarshal(call.Arguments, &a)
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: a["n"]}}}, nil
		},
	}
	res := runWithTools(t, provider, tag)

	// new = [assistant1, tool1, tool2, tool3, assistant2]
	wantRoles := []MessageRole{
		MessageRoleAssistant, MessageRoleTool, MessageRoleTool, MessageRoleTool, MessageRoleAssistant,
	}
	if len(res.NewMessages) != len(wantRoles) {
		t.Fatalf("len(NewMessages) = %d, want %d", len(res.NewMessages), len(wantRoles))
	}
	for i, want := range wantRoles {
		if got := res.NewMessages[i].Role; got != want {
			t.Fatalf("NewMessages[%d].Role = %q, want %q", i, got, want)
		}
	}
	gotIDs := []string{
		res.NewMessages[1].ToolCallID,
		res.NewMessages[2].ToolCallID,
		res.NewMessages[3].ToolCallID,
	}
	wantIDs := []string{"c1", "c2", "c3"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("tool[%d] ToolCallID = %q, want %q (full: %v)", i, gotIDs[i], wantIDs[i], gotIDs)
		}
	}
	gotTexts := []string{
		res.NewMessages[1].Content[0].Text,
		res.NewMessages[2].Content[0].Text,
		res.NewMessages[3].Content[0].Text,
	}
	wantTexts := []string{"first", "second", "third"}
	for i := range wantTexts {
		if gotTexts[i] != wantTexts[i] {
			t.Fatalf("tool[%d] text = %q, want %q (full: %v)", i, gotTexts[i], wantTexts[i], gotTexts)
		}
	}
}

func TestExecToolMixedSuccessAndErrorOrdering(t *testing.T) {
	t.Parallel()

	provider := scriptTwoToolCallsThenStop(
		ToolCall{ID: "c1", Name: "ok", Arguments: json.RawMessage(`{}`)},
		ToolCall{ID: "c2", Name: "ghost", Arguments: json.RawMessage(`{}`)},
		ToolCall{ID: "c3", Name: "ok", Arguments: json.RawMessage(`{}`)},
	)
	ok := Tool{
		ToolSpec: ToolSpec{Name: "ok"},
		Execute: func(context.Context, ToolCall) (ToolResult, error) {
			return ToolResult{Content: []ContentPart{{Type: ContentTypeText, Text: "ok"}}}, nil
		},
	}
	res := runWithTools(t, provider, ok)

	if got := res.NewMessages[1].IsError; got {
		t.Fatalf("[1].IsError = true, want false (c1 ok)")
	}
	if got := res.NewMessages[2].IsError; !got {
		t.Fatalf("[2].IsError = false, want true (c2 ghost)")
	}
	if got := res.NewMessages[3].IsError; got {
		t.Fatalf("[3].IsError = true, want false (c3 ok)")
	}
	wantIDs := []string{"c1", "c2", "c3"}
	for i, want := range wantIDs {
		if got := res.NewMessages[i+1].ToolCallID; got != want {
			t.Fatalf("[%d] ToolCallID = %q, want %q", i+1, got, want)
		}
	}
}
