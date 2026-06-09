package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
)

type scriptedProvider struct {
	turns    [][]glue.ProviderEvent
	requests []glue.ProviderRequest
	err      error
}

func (p *scriptedProvider) Stream(_ context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	if p.err != nil {
		return nil, p.err
	}
	if len(p.requests) >= len(p.turns) {
		return nil, errors.New("scriptedProvider: unexpected call")
	}
	p.requests = append(p.requests, req)
	events := p.turns[len(p.requests)-1]
	ch := make(chan glue.ProviderEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func fakeFactory(provider glue.Provider) providerFactory {
	return func(string) (glue.Provider, error) { return provider, nil }
}

func textTurn(text string) []glue.ProviderEvent {
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: text},
		{Type: glue.ProviderEventDone},
	}
}

func toolCallTurn(id, name, args string) []glue.ProviderEvent {
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}},
		{Type: glue.ProviderEventDone},
	}
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func writeSSETest(t *testing.T, w io.Writer, event daemon.EventEnvelope) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		t.Fatal(err)
	}
}

func TestRunCLIHelp(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	code := runCLI(context.Background(), []string{"--help"}, &stdout, io.Discard, fakeFactory(nil))
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "glue run") {
		t.Fatalf("help missing 'glue run': %q", stdout.String())
	}
}

func TestRunCLIVersion(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{"version", "--version", "-v"} {
		arg := arg
		t.Run(arg, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			code := runCLI(context.Background(), []string{arg}, &stdout, io.Discard, fakeFactory(nil))
			if code != 0 {
				t.Fatalf("code = %d, want 0", code)
			}
			out := stdout.String()
			if !strings.HasPrefix(out, "glue ") {
				t.Fatalf("version output should start with 'glue ': %q", out)
			}
			// The Go toolchain banner always lands; everything else (module
			// version, vcs revision, build time) is conditional on how the
			// binary was produced.
			if !strings.Contains(out, "go") {
				t.Fatalf("version output missing toolchain: %q", out)
			}
		})
	}
}

func TestRunCLINoArgsPrintsHelp(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	code := runCLI(context.Background(), nil, &stdout, io.Discard, fakeFactory(nil))
	if code != 0 || !strings.Contains(stdout.String(), "glue run") {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
}

func TestRunCLIUnknownCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{"nope"}, &stdout, &stderr, fakeFactory(nil))
	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q, want 'unknown command'", stderr.String())
	}
}

func TestRunCLIUnknownAgent(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{"run", "weird", "--prompt", "x"}, &stdout, &stderr, fakeFactory(&scriptedProvider{}))
	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "unknown agent") {
		t.Fatalf("stderr = %q, want 'unknown agent'", stderr.String())
	}
}

func TestRunCLIMissingGeminiAPIKey(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run", "--prompt", "go", "--store", t.TempDir(),
	}, &stdout, &stderr, defaultProviderFactory)
	if code == 0 {
		t.Fatal("code = 0, want nonzero for missing GEMINI_API_KEY")
	}
	if !strings.Contains(stderr.String(), "GEMINI_API_KEY") {
		t.Fatalf("stderr = %q, want GEMINI_API_KEY hint", stderr.String())
	}
}

func TestResolveProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		provider  string
		model     string
		wantName  string
		wantModel string
		wantErr   string
	}{
		{name: "default empty selects gemini", provider: "", model: "", wantName: "gemini", wantModel: "gemini-3.1-pro-preview"},
		{name: "codex default model", provider: "codex", model: "", wantName: "codex", wantModel: "gpt-5-codex"},
		{name: "explicit model overrides default", provider: "gemini", model: "gemini-2.0-pro", wantName: "gemini", wantModel: "gemini-2.0-pro"},
		{name: "case insensitive name", provider: "CodeX", model: "", wantName: "codex", wantModel: "gpt-5-codex"},
		{name: "unknown provider errors", provider: "bogus", wantErr: "unknown provider"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name, model, err := resolveProvider(tc.provider, tc.model)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), "known:") {
					t.Fatalf("err = %v, want a known-providers hint", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if name != tc.wantName {
				t.Fatalf("name = %q, want %q", name, tc.wantName)
			}
			if model != tc.wantModel {
				t.Fatalf("model = %q, want %q", model, tc.wantModel)
			}
		})
	}
}

func TestDefaultProviderFactory(t *testing.T) {
	t.Run("missing gemini key errors", func(t *testing.T) {
		t.Setenv("GEMINI_API_KEY", "")
		_, err := defaultProviderFactory("gemini")
		if err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
			t.Fatalf("err = %v, want GEMINI_API_KEY hint", err)
		}
	})
	t.Run("codex needs no env key", func(t *testing.T) {
		provider, err := defaultProviderFactory("codex")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if provider == nil {
			t.Fatal("provider = nil, want a codex provider")
		}
	})
	t.Run("unknown provider errors", func(t *testing.T) {
		if _, err := defaultProviderFactory("bogus"); err == nil || !strings.Contains(err.Error(), "unknown provider") {
			t.Fatalf("err = %v, want unknown provider", err)
		}
	})
}

func TestRunCLIUnknownProvider(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run", "--provider", "bogus", "--prompt", "x", "--store", t.TempDir(),
	}, &stdout, &stderr, fakeFactory(&scriptedProvider{}))
	if code == 0 {
		t.Fatal("code = 0, want nonzero for unknown provider")
	}
	if !strings.Contains(stderr.String(), "unknown provider") {
		t.Fatalf("stderr = %q, want 'unknown provider'", stderr.String())
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func lastToolText(t *testing.T, req glue.ProviderRequest) string {
	t.Helper()
	if len(req.Messages) == 0 {
		t.Fatal("provider request has no messages")
	}
	msg := req.Messages[len(req.Messages)-1]
	if msg.Role != glue.MessageRoleTool || len(msg.Content) == 0 {
		t.Fatalf("last message = %#v, want tool result", msg)
	}
	return msg.Content[0].Text
}
