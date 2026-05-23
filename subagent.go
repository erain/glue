package glue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// SubagentOptions configures [SubagentTool].
type SubagentOptions struct {
	// Name is the model-visible tool name.
	Name string

	// Description is the model-visible tool description.
	Description string

	// Agent is the child agent invoked when the tool runs. Required.
	Agent *Agent

	// SessionID is an optional prefix for generated child session ids.
	// Each tool instance and tool call appends a generated suffix so calls
	// use isolated transcripts. Empty uses "subagent:<Name>".
	SessionID string

	// MaxTurns optionally overrides the child prompt's loop turn budget.
	// Zero uses the child agent/default loop budget.
	MaxTurns int

	// SystemPrompt optionally overrides the child agent system prompt for
	// each delegated prompt.
	SystemPrompt string
}

var subagentInstanceIDs atomic.Uint64

type subagentArgs struct {
	Prompt string `json:"prompt"`
}

// SubagentTool exposes a child Agent as a Tool. Each tool call creates a
// fresh child session, forwards only the explicit prompt argument, and returns
// the child agent's final text as the tool result. Child prompt failures become
// model-visible error results except for context cancellation/deadline errors,
// which are returned as Go errors so the parent loop stops promptly.
func SubagentTool(opts SubagentOptions) (Tool, error) {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return Tool{}, errors.New("glue: subagent name is required")
	}
	description := strings.TrimSpace(opts.Description)
	if description == "" {
		return Tool{}, errors.New("glue: subagent description is required")
	}
	if opts.Agent == nil {
		return Tool{}, errors.New("glue: subagent agent is required")
	}
	if opts.MaxTurns < 0 {
		return Tool{}, errors.New("glue: subagent MaxTurns must be non-negative")
	}

	baseSessionID := strings.TrimSpace(opts.SessionID)
	if baseSessionID == "" {
		baseSessionID = "subagent:" + name
	}
	systemPrompt := opts.SystemPrompt
	maxTurns := opts.MaxTurns
	child := opts.Agent
	instanceID := subagentInstanceIDs.Add(1)
	var calls atomic.Uint64

	return NewTool[subagentArgs](
		ToolSpec{
			Name:        name,
			Description: description,
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "prompt": { "type": "string", "description": "The complete task or question to delegate to this subagent." }
  },
  "required": ["prompt"]
}`),
		},
		func(ctx context.Context, args subagentArgs) (ToolResult, error) {
			if strings.TrimSpace(args.Prompt) == "" {
				return ErrorResult(errors.New("glue: subagent prompt is required")), nil
			}

			sessionID := fmt.Sprintf("%s:%d:%d", baseSessionID, instanceID, calls.Add(1))
			session, err := child.Session(ctx, sessionID)
			if err != nil {
				if contextErr := subagentContextError(ctx, err); contextErr != nil {
					return ToolResult{}, contextErr
				}
				return subagentErrorResult(fmt.Errorf("subagent %q session: %w", name, err), sessionID), nil
			}

			promptOptions := make([]PromptOption, 0, 2)
			if maxTurns > 0 {
				promptOptions = append(promptOptions, WithMaxTurns(maxTurns))
			}
			if systemPrompt != "" {
				promptOptions = append(promptOptions, WithSystemPrompt(systemPrompt))
			}

			result, err := session.Prompt(ctx, args.Prompt, promptOptions...)
			if err != nil {
				if contextErr := subagentContextError(ctx, err); contextErr != nil {
					return ToolResult{}, contextErr
				}
				return subagentErrorResult(fmt.Errorf("subagent %q: %w", name, err), sessionID), nil
			}

			toolResult := TextResult(result.Text)
			toolResult.Metadata = map[string]any{"session_id": sessionID}
			return toolResult, nil
		},
	), nil
}

func subagentErrorResult(err error, sessionID string) ToolResult {
	result := ErrorResult(err)
	if sessionID != "" {
		result.Metadata = map[string]any{"session_id": sessionID}
	}
	return result
}

func subagentContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ctx.Err()
}
