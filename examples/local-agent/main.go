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

	"github.com/erain/glue"
	"github.com/erain/glue/providers/gemini"
	filestore "github.com/erain/glue/stores/file"
)

// timezoneArgs is the decoded argument shape for the local_time tool.
type timezoneArgs struct {
	Timezone string `json:"timezone"`
}

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

	// WithStreamWriter mirrors assistant text deltas straight to stdout.
	if _, err := session.Prompt(ctx, *prompt, glue.WithStreamWriter(stdout)); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout)
	return 0
}

// localTimeTool returns the current wall-clock time for a requested
// timezone label. The label is forwarded back in the response so the
// model can verify the tool understood the input.
//
// It uses glue.NewTool[T], which decodes ToolCall.Arguments into the
// typed timezoneArgs before the executor runs and turns malformed
// arguments into an error ToolResult the model can recover from. Pair it
// with glue.TextResult / glue.ErrorResult for the result side.
func localTimeTool() glue.Tool {
	return glue.NewTool[timezoneArgs](
		glue.ToolSpec{
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
		func(_ context.Context, args timezoneArgs) (glue.ToolResult, error) {
			if args.Timezone == "" {
				return glue.ErrorResult(errors.New("timezone is required")), nil
			}
			data, err := json.Marshal(map[string]string{
				"timezone": args.Timezone,
				"time":     time.Now().Format(time.RFC3339),
			})
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return glue.TextResult(string(data)), nil
		},
	)
}
