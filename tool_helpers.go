package glue

import (
	"context"
	"encoding/json"
	"fmt"
)

// TextResult wraps a string in a ToolResult with a single text content part.
// Use it from tool executors when the tool succeeds with textual output.
func TextResult(s string) ToolResult {
	return ToolResult{
		Content: []ContentPart{{Type: ContentTypeText, Text: s}},
	}
}

// ErrorResult wraps an error in a ToolResult tagged with IsError=true so the
// model sees it as a tool failure rather than the loop crashing. The error's
// Error() string becomes the visible text.
func ErrorResult(err error) ToolResult {
	return ToolResult{
		Content: []ContentPart{{Type: ContentTypeText, Text: err.Error()}},
		IsError: true,
	}
}

// NewTool builds a Tool whose executor decodes ToolCall.Arguments into a
// typed Go value before invoking fn. Empty arguments decode as the zero
// value of T. JSON decode failures surface to the model as an error
// ToolResult — matching the existing manual pattern — so a malformed call
// does not crash the loop.
//
// Callers still supply spec.Parameters (a JSON Schema). Schema generation
// from T is intentionally out of scope.
func NewTool[T any](spec ToolSpec, fn func(ctx context.Context, args T) (ToolResult, error)) Tool {
	return Tool{
		ToolSpec: spec,
		Execute: func(ctx context.Context, call ToolCall) (ToolResult, error) {
			var args T
			if len(call.Arguments) > 0 {
				if err := json.Unmarshal(call.Arguments, &args); err != nil {
					return ErrorResult(fmt.Errorf("decode arguments for tool %q: %w", spec.Name, err)), nil
				}
			}
			return fn(ctx, args)
		},
	}
}
