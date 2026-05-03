// Command glue is the local CLI runner for Glue agents.
//
// Run a single prompt:
//
//	glue run --prompt "Say hi" --id local-dev --store .glue/sessions
//
// The default subcommand uses Glue's Gemini-backed agent. Streaming text
// is written to stdout; provider, store, or flag errors exit non-zero.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/erain/glue"
	"github.com/erain/glue/providers/gemini"
	filestore "github.com/erain/glue/stores/file"
)

const defaultModel = "gemini-2.5-flash"

// providerFactory returns a [glue.Provider] or an error. The error is the
// hook the default factory uses to surface "GEMINI_API_KEY missing"
// before any API call is attempted.
type providerFactory func() (glue.Provider, error)

type envFiles []string

func (e *envFiles) String() string { return strings.Join(*e, ",") }

func (e *envFiles) Set(value string) error {
	*e = append(*e, value)
	return nil
}

func main() {
	os.Exit(runCLI(context.Background(), os.Args[1:], os.Stdout, os.Stderr, defaultGeminiFactory))
}

func defaultGeminiFactory() (glue.Provider, error) {
	if strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) == "" {
		return nil, errors.New("GEMINI_API_KEY is required (set in shell or pass --env <file>)")
	}
	return gemini.New(gemini.Options{}), nil
}

func runCLI(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printUsage(stdout)
		return 0
	}

	switch args[0] {
	case "run":
		if err := runCommand(ctx, args[1:], stdout, stderr, newProvider); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 1
	}
}

func runCommand(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newProvider providerFactory) error {
	flags := flag.NewFlagSet("glue run", flag.ContinueOnError)
	flags.SetOutput(stderr)

	id := flags.String("id", "default", "session id")
	prompt := flags.String("prompt", "", "prompt text")
	model := flags.String("model", defaultModel, "model id or gemini/<model>")
	storeDir := flags.String("store", ".glue/sessions", "session store directory")
	var envs envFiles
	flags.Var(&envs, "env", "env file path; repeatable")

	if err := flags.Parse(args); err != nil {
		return err
	}

	agentName := "default"
	if flags.NArg() > 0 {
		agentName = flags.Arg(0)
	}
	if agentName != "default" && agentName != "gemini" {
		return fmt.Errorf("unknown agent %q; only 'default' is available in this runner", agentName)
	}
	if strings.TrimSpace(*prompt) == "" {
		return errors.New("missing required --prompt")
	}
	if err := loadEnvFiles(envs); err != nil {
		return err
	}

	provider, err := newProvider()
	if err != nil {
		return err
	}

	agent := glue.NewAgent(glue.AgentOptions{
		Provider: provider,
		Model:    normalizeModel(*model),
		Store:    filestore.New(*storeDir),
		WorkDir:  ".",
	})
	session, err := agent.Session(ctx, *id)
	if err != nil {
		return err
	}

	wroteDelta := false
	response, err := session.Prompt(ctx, *prompt, glue.WithEvents(func(event glue.Event) {
		if event.Type == glue.EventTextDelta && event.Delta != "" {
			fmt.Fprint(stdout, event.Delta)
			wroteDelta = true
		}
	}))
	if err != nil {
		if wroteDelta {
			fmt.Fprintln(stdout)
		}
		return err
	}
	if wroteDelta {
		fmt.Fprintln(stdout)
		return nil
	}
	if response.Text != "" {
		fmt.Fprintln(stdout, response.Text)
	}
	return nil
}

func normalizeModel(model string) string {
	return strings.TrimPrefix(model, "gemini/")
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  glue run [default|gemini] --prompt <text> [--id <id>] [--model <model>] [--store <dir>] [--env <path>]

Commands:
  run    Run the local Gemini-backed agent.

Flags:
  --id       Session id. Defaults to "default".
  --prompt   Prompt text. Required.
  --model    Gemini model id or gemini/<model>. Defaults to gemini-2.5-flash.
  --store    File session store directory. Defaults to .glue/sessions.
  --env      Load env vars from a .env file. Repeatable; shell env wins.
`)
}

func loadEnvFiles(files []string) error {
	shellEnv := map[string]struct{}{}
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			shellEnv[key] = struct{}{}
		}
	}

	for _, path := range files {
		if err := loadEnvFile(path, shellEnv); err != nil {
			return err
		}
	}
	return nil
}

func loadEnvFile(path string, shellEnv map[string]struct{}) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s:%d: empty key", path, lineNumber)
		}
		if _, exists := shellEnv[key]; exists {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}
