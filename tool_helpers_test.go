package glue

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestTextResult(t *testing.T) {
	r := TextResult("hello")
	if r.IsError {
		t.Fatalf("TextResult must not be tagged IsError")
	}
	if len(r.Content) != 1 || r.Content[0].Type != ContentTypeText || r.Content[0].Text != "hello" {
		t.Fatalf("unexpected content: %+v", r.Content)
	}
}

func TestErrorResult(t *testing.T) {
	r := ErrorResult(errors.New("boom"))
	if !r.IsError {
		t.Fatalf("ErrorResult must be tagged IsError")
	}
	if len(r.Content) != 1 || r.Content[0].Text != "boom" {
		t.Fatalf("unexpected content: %+v", r.Content)
	}
}

type echoArgs struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestNewTool_DecodesTypedArgs(t *testing.T) {
	tool := NewTool[echoArgs](ToolSpec{Name: "echo"}, func(_ context.Context, a echoArgs) (ToolResult, error) {
		return TextResult(a.Name + "/" + strconv.Itoa(a.Count)), nil
	})

	res, err := tool.Execute(context.Background(), ToolCall{
		Name:      "echo",
		Arguments: json.RawMessage(`{"name":"glue","count":3}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res)
	}
	if got := res.Content[0].Text; got != "glue/3" {
		t.Fatalf("unexpected text: %q", got)
	}
}

func TestNewTool_EmptyArgsZeroValue(t *testing.T) {
	called := false
	tool := NewTool[echoArgs](ToolSpec{Name: "echo"}, func(_ context.Context, a echoArgs) (ToolResult, error) {
		called = true
		if a.Name != "" || a.Count != 0 {
			t.Fatalf("expected zero value, got %+v", a)
		}
		return TextResult("ok"), nil
	})
	if _, err := tool.Execute(context.Background(), ToolCall{Name: "echo"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !called {
		t.Fatalf("fn not invoked on empty args")
	}
}

func TestNewTool_MalformedArgsReturnsErrorResult(t *testing.T) {
	tool := NewTool[echoArgs](ToolSpec{Name: "echo"}, func(_ context.Context, a echoArgs) (ToolResult, error) {
		t.Fatalf("fn should not be invoked on malformed args")
		return ToolResult{}, nil
	})
	res, err := tool.Execute(context.Background(), ToolCall{
		Name:      "echo",
		Arguments: json.RawMessage(`{not-json`),
	})
	if err != nil {
		t.Fatalf("Execute returned err=%v; expected error ToolResult, not loop crash", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true on malformed args; got %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, `tool "echo"`) {
		t.Fatalf("expected error text to name the tool; got %q", res.Content[0].Text)
	}
}
