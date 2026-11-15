package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
	"github.com/erain/glue/cmd/glue/tui"
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
	code := runCLI(context.Background(), []string{"nope"}, &stdout, &stderr, func(string) (glue.Provider, error) {
		t.Fatal("unknown command must not construct provider")
		return nil, nil
	})
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
		"run", "--provider", "gemini", "--prompt", "go", "--store", t.TempDir(),
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
		{name: "default empty selects openrouter", provider: "", model: "", wantName: "openrouter", wantModel: "openrouter/free"},
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

func TestRunCLIGitAskpass(t *testing.T) {
	t.Setenv("GLUE_GITHUB_TOKEN", "ghp_cli_askpass_secret")
	var stdout, stderr bytes.Buffer
	if code := runCLI(context.Background(), []string{"git-askpass", "Username for 'https://github.com': "}, &stdout, &stderr, fakeFactory(nil)); code != 0 || stdout.String() != "x-access-token\n" {
		t.Fatalf("username askpass code=%d out=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := runCLI(context.Background(), []string{"git-askpass", "Password for 'https://x-access-token@github.com': "}, &stdout, &stderr, fakeFactory(nil)); code != 0 || stdout.String() != "ghp_cli_askpass_secret\n" {
		t.Fatalf("password askpass code=%d out=%q", code, stdout.String())
	}
	t.Setenv("GLUE_GITHUB_TOKEN", "")
	stdout.Reset()
	if code := runCLI(context.Background(), []string{"git-askpass"}, &stdout, &stderr, fakeFactory(nil)); code == 0 || stdout.Len() != 0 {
		t.Fatalf("empty token askpass code=%d out=%q", code, stdout.String())
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

func clearCodingConfigEnv(t *testing.T, xdg string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	for _, k := range []string{"GLUE_CONFIG_DIR", "GLUE_PROVIDER", "GLUE_MODEL", "GLUE_THINKING"} {
		t.Setenv(k, "")
	}
}

func TestBareGlueTTYLaunchesCodingTUI(t *testing.T) {
	t.Chdir(t.TempDir())
	xdg := t.TempDir()
	clearCodingConfigEnv(t, xdg)
	origTTY, origTUI := isTTY, runTUI
	t.Cleanup(func() { isTTY, runTUI = origTTY, origTUI })

	checkLaunch := func(t *testing.T, wantProv, wantModel, wantThinking string) {
		t.Helper()
		p := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("ready")}}
		var gotProv string
		var cfg tui.Config
		isTTY = func(any) bool { return true }
		runTUI = func(_ context.Context, c tui.Config) error { cfg = c; return nil }
		var out, errs bytes.Buffer
		code := runCLIWithDeps(context.Background(), nil, strings.NewReader(""), &out, &errs, func(n string) (glue.Provider, error) {
			gotProv = n
			return p, nil
		}, serveDaemon, http.DefaultClient)
		if code != 0 || gotProv != wantProv || cfg.Provider != wantProv || cfg.Model != wantModel || cfg.WorkDir != "." || cfg.Agent == nil || cfg.BuildTools == nil || cfg.SubmitRemote == nil || cfg.Jobs == nil {
			t.Fatalf("code=%d prov=%q cfg=%+v stderr=%q", code, gotProv, cfg, errs.String())
		}
		var names []string
		for _, tool := range cfg.Tools {
			names = append(names, tool.Name)
		}
		for _, want := range []string{"read_file", "write_file", "edit_file", "shell_exec", "git_diff_branch", "git_log_branch"} {
			if !containsString(names, want) {
				t.Fatalf("tools=%v missing %s", names, want)
			}
		}
		sess := must(cfg.Agent.Session(context.Background(), "bare-tui"))
		_ = must(sess.Prompt(context.Background(), "hi"))
		if len(p.requests) != 1 || p.requests[0].Model != wantModel || p.requests[0].Options["thinking_level"] != wantThinking || p.requests[0].SystemPrompt == "" {
			t.Fatalf("agent request=%+v", p.requests)
		}
	}

	t.Run("defaults", func(t *testing.T) { checkLaunch(t, "gemini", "gemini-3-flash-preview", "low") })
	t.Run("user_config", func(t *testing.T) {
		cfgFile := filepath.Join(xdg, "glue", "config.yaml")
		must(0, os.MkdirAll(filepath.Dir(cfgFile), 0o700))
		must(0, os.WriteFile(cfgFile, []byte("provider: gemini\nmodel: gemini-3.1-pro-preview\nthinking: high\n"), 0o600))
		checkLaunch(t, "gemini", "gemini-3.1-pro-preview", "high")
	})
	t.Run("partial_tty_prints_help", func(t *testing.T) {
		in, out := strings.NewReader(""), &bytes.Buffer{}
		for _, tty := range []func(any) bool{func(v any) bool { return v == in }, func(v any) bool { return v == out }} {
			isTTY, runTUI = tty, func(context.Context, tui.Config) error { t.Fatal("must not launch TUI"); return nil }
			out.Reset()
			code := runCLIWithDeps(context.Background(), nil, in, out, io.Discard, func(string) (glue.Provider, error) { t.Fatal("must not construct provider"); return nil, nil }, serveDaemon, http.DefaultClient)
			if code != 0 || !strings.Contains(out.String(), "glue code") {
				t.Fatalf("code=%d out=%q", code, out.String())
			}
		}
	})
}

func TestCodeSubcommandEnablesCodingTools(t *testing.T) {
	clearCodingConfigEnv(t, t.TempDir())
	workDir, storeDir := t.TempDir(), t.TempDir()
	p1 := &scriptedProvider{turns: [][]glue.ProviderEvent{toolCallTurn("c1", "write_file", `{"path":"note.txt","content":"from glue code"}`), textTurn("wrote note")}}
	var gotProv string
	var out, errs bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{"code", "--work", workDir, "--store", storeDir, "--prompt", "write a note"}, strings.NewReader("a\n"), &out, &errs, func(n string) (glue.Provider, error) { gotProv = n; return p1, nil }, serveDaemon, http.DefaultClient)
	if code != 0 || gotProv != "gemini" || p1.requests[0].Model != "gemini-3-flash-preview" || p1.requests[0].Options["thinking_level"] != "low" || string(must(os.ReadFile(filepath.Join(workDir, "note.txt")))) != "from glue code" {
		t.Fatalf("code=%d prov=%q req=%+v stderr=%q", code, gotProv, p1.requests, errs.String())
	}
	p2 := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("resumed")}}
	errs.Reset()
	code = runCLIWithDeps(context.Background(), []string{"code", "--continue", "--model", "gemini-3.1-pro-preview", "--work", workDir, "--store", storeDir, "--prompt", "next"}, strings.NewReader(""), io.Discard, &errs, fakeFactory(p2), serveDaemon, http.DefaultClient)
	if code != 0 || len(p2.requests) != 1 || p2.requests[0].Model != "gemini-3.1-pro-preview" || len(p2.requests[0].Messages) != 6 || len(p2.requests[0].Tools) == 0 || !strings.Contains(errs.String(), "glue code: continuing session") {
		t.Fatalf("continue code=%d req=%+v stderr=%q", code, p2.requests, errs.String())
	}
	errs.Reset()
	if code = runCLIWithDeps(context.Background(), []string{"code", "--select", "--prompt", "hi"}, strings.NewReader(""), io.Discard, &errs, fakeFactory(nil), serveDaemon, http.DefaultClient); code == 0 || !strings.Contains(errs.String(), "requires terminal") {
		t.Fatalf("select code=%d stderr=%q", code, errs.String())
	}
	chat := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("hi")}}
	var chatProv string
	if code = runCLIWithDeps(context.Background(), []string{"run", "--store", t.TempDir(), "--prompt", "hi"}, strings.NewReader(""), io.Discard, io.Discard, func(n string) (glue.Provider, error) { chatProv = n; return chat, nil }, serveDaemon, http.DefaultClient); code != 0 || chatProv != "openrouter" || chat.requests[0].Model != "openrouter/free" || len(chat.requests[0].Tools) != 0 {
		t.Fatalf("run without --coding changed: code=%d prov=%q req=%+v", code, chatProv, chat.requests)
	}
}

func TestConciseTopLevelHelp(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		var out, errs bytes.Buffer
		code := runCLI(context.Background(), args, &out, &errs, fakeFactory(nil))
		text := out.String()
		if lines := strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1; code != 0 || errs.Len() != 0 || lines >= 40 || strings.Contains(text, "--mcp-resources-json") || strings.Contains(text, "--forget-permission-json") {
			t.Fatalf("args=%v code=%d lines=%d stderr=%q\n%s", args, code, lines, errs.String(), text)
		}
		for _, want := range []string{"Code locally:", "Run remotely on AX:", "Manage remote jobs:", "Models & daemon:", "  glue ", "glue code", "glue --remote", "glue job"} {
			if !strings.Contains(text, want) {
				t.Fatalf("args=%v missing %q:\n%s", args, want, text)
			}
		}
	}
}

func TestSubcommandHelpFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sub   string
		wants []string
	}{
		{"code", []string{"glue code", "--continue", "--select", "--model", "--prompt"}},
		{"run", []string{"glue run", "--coding", "--continue", "--mode"}},
		{"connect", []string{"glue connect", "--mcp-resources-json", "--forget-permission-json", "--inspect"}},
		{"serve", []string{"glue serve", "--coding", "--jobs", "--job-from-ax", "--token-file"}},
		{"job", []string{"glue job", "--snapshot", "--verify"}},
		{"goal", []string{"glue goal", "--worktree", "--resume"}},
		{"models", []string{"glue models", "--provider", "--json"}},
	} {
		t.Run(tc.sub, func(t *testing.T) {
			t.Parallel()
			var flagOut, helpOut, errs bytes.Buffer
			c1 := runCLI(context.Background(), []string{tc.sub, "--help"}, &flagOut, &errs, fakeFactory(nil))
			c2 := runCLI(context.Background(), []string{"help", tc.sub}, &helpOut, &errs, fakeFactory(nil))
			if c1 != 0 || c2 != 0 || errs.Len() != 0 || helpOut.String() != flagOut.String() {
				t.Fatalf("%s help: c1=%d c2=%d stderr=%q", tc.sub, c1, c2, errs.String())
			}
			for _, want := range tc.wants {
				if !strings.Contains(flagOut.String(), want) {
					t.Fatalf("%s --help missing %q:\n%s", tc.sub, want, flagOut.String())
				}
			}
		})
	}
}
