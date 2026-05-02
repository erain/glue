// Command local-agent is a small Gemini-backed CLI built directly on the
// glue library. It registers a local_time tool so the model can call into
// the Go process to get the current wall-clock time for a requested
// timezone label, streams the assistant text to stdout, and persists
// sessions through stores/file. Use it as a smoke test and as a tutorial
// for new applications.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"glue"
	"glue/providers/gemini"
	filestore "glue/stores/file"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("local-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)

	prompt := flags.String("prompt", "", "prompt text")
	id := flags.String("id", "example", "session id")
	model := flags.String("model", "gemini-2.5-flash", "Gemini model id")
	storeDir := flags.String("store", ".glue/example-sessions", "session store directory")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if *prompt == "" {
		fmt.Fprintln(stderr, "missing required --prompt")
		return 1
	}

	agent := glue.NewAgent(glue.AgentOptions{
		Provider: gemini.New(gemini.Options{}),
		Model:    *model,
		Store:    filestore.New(*storeDir),
		WorkDir:  ".",
		Tools:    []glue.Tool{localTimeTool()},
	})
	session, err := agent.Session(ctx, *id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	wrote := false
	_, err = session.Prompt(ctx, *prompt, glue.WithEvents(func(event glue.Event) {
		if event.Type == glue.EventTextDelta && event.Delta != "" {
			fmt.Fprint(stdout, event.Delta)
			wrote = true
		}
	}))
	if wrote {
		fmt.Fprintln(stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// localTimeTool returns the current wall-clock time for a requested
// timezone label. The label is forwarded back in the response so the
// model can verify the tool understood the input.
func localTimeTool() glue.Tool {
	return glue.Tool{
		ToolSpec: glue.ToolSpec{
			Name:        "local_time",
			Description: "Return the current local time for a requested timezone label.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "timezone": {
      "type": "string",
      "description": "Human-readable timezone label, for example America/Toronto"
    }
  },
  "required": ["timezone"]
}`),
		},
		Execute: func(_ context.Context, call glue.ToolCall) (glue.ToolResult, error) {
			var args struct {
				Timezone string `json:"timezone"`
			}
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return glue.ToolResult{}, err
			}
			if args.Timezone == "" {
				return glue.ToolResult{}, errors.New("timezone is required")
			}
			payload := map[string]string{
				"timezone": args.Timezone,
				"time":     time.Now().Format(time.RFC3339),
			}
			data, err := json.Marshal(payload)
			if err != nil {
				return glue.ToolResult{}, err
			}
			return glue.ToolResult{
				Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: string(data)}},
			}, nil
		},
	}
}
