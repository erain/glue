package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	return func() (glue.Provider, error) { return provider, nil }
}

func textTurn(text string) []glue.ProviderEvent {
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: text},
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

func TestRunCLIStreamsOutputAndLoadsEnv(t *testing.T) {
	// Not parallel: mutates process env.
	t.Setenv("GLUE_TEST_ENV", "")
	os.Unsetenv("GLUE_TEST_ENV")

	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("GLUE_TEST_ENV=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: "hello"},
		{Type: glue.ProviderEventTextDelta, Delta: " cli"},
		{Type: glue.ProviderEventDone},
	}}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "say hi",
		"--store", t.TempDir(),
		"--env", envPath,
	}, &stdout, &stderr, fakeFactory(provider))
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "hello cli\n" {
		t.Fatalf("stdout = %q, want streamed text", stdout.String())
	}
	if got := os.Getenv("GLUE_TEST_ENV"); got != "from-file" {
		t.Fatalf("env GLUE_TEST_ENV = %q, want from-file", got)
	}
	t.Cleanup(func() { os.Unsetenv("GLUE_TEST_ENV") })
}

func TestRunCLIUsageReportsTokens(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: "hello"},
		{Type: glue.ProviderEventDone, Message: &glue.Message{
			Role: glue.MessageRoleAssistant,
			Usage: &glue.Usage{
				InputTokens:     3,
				OutputTokens:    2,
				CacheReadTokens: 1,
				TotalTokens:     5,
			},
		}},
	}}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "say hi",
		"--store", t.TempDir(),
		"--usage",
	}, &stdout, &stderr, fakeFactory(provider))
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "hello\n" {
		t.Fatalf("stdout = %q, want streamed text", stdout.String())
	}
	if got, want := stderr.String(), "usage: input=3 output=2 cache_read=1 total=5\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRunCLIUsageReportsEstimatedCost(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: "hello"},
		{Type: glue.ProviderEventDone, Message: &glue.Message{
			Role: glue.MessageRoleAssistant,
			Usage: &glue.Usage{
				InputTokens:      1_000_000,
				OutputTokens:     500_000,
				CacheReadTokens:  250_000,
				CacheWriteTokens: 100_000,
				TotalTokens:      1_850_000,
			},
		}},
	}}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "say hi",
		"--store", t.TempDir(),
		"--usage",
		"--usage-input-price", "1",
		"--usage-output-price", "2",
		"--usage-cache-read-price", "0.25",
		"--usage-cache-write-price", "3",
	}, &stdout, &stderr, fakeFactory(provider))
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if got, want := stderr.String(), "usage: input=1000000 output=500000 cache_read=250000 cache_write=100000 total=1850000 cost_usd=2.362500\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRunCLIUsageSilentWhenMissing(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("ok")}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "go",
		"--store", t.TempDir(),
		"--usage",
		"--usage-input-price", "1",
	}, &stdout, &stderr, fakeFactory(provider))
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "ok\n" {
		t.Fatalf("stdout = %q, want ok", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want no usage output", stderr.String())
	}
}

func TestRunCLIUsagePriceRejectsNegative(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "go",
		"--usage",
		"--usage-input-price", "-1",
	}, &stdout, &stderr, fakeFactory(&scriptedProvider{}))
	if code == 0 {
		t.Fatal("code = 0, want failure")
	}
	if !strings.Contains(stderr.String(), "--usage-input-price must be non-negative") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunCLIMultipleEnvFilesShellEnvWins(t *testing.T) {
	t.Setenv("GLUE_TEST_FROM_SHELL", "shell-value")
	os.Unsetenv("GLUE_TEST_FROM_FILE_A")
	os.Unsetenv("GLUE_TEST_FROM_FILE_B")
	t.Cleanup(func() {
		os.Unsetenv("GLUE_TEST_FROM_FILE_A")
		os.Unsetenv("GLUE_TEST_FROM_FILE_B")
	})

	dir := t.TempDir()
	a := filepath.Join(dir, "a.env")
	b := filepath.Join(dir, "b.env")
	if err := os.WriteFile(a, []byte("GLUE_TEST_FROM_FILE_A=A\nGLUE_TEST_FROM_SHELL=ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("GLUE_TEST_FROM_FILE_B=B\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("ok")}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run",
		"--prompt", "go",
		"--store", t.TempDir(),
		"--env", a, "--env", b,
	}, &stdout, &stderr, fakeFactory(provider))
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if got := os.Getenv("GLUE_TEST_FROM_FILE_A"); got != "A" {
		t.Fatalf("FILE_A = %q, want A", got)
	}
	if got := os.Getenv("GLUE_TEST_FROM_FILE_B"); got != "B" {
		t.Fatalf("FILE_B = %q, want B", got)
	}
	if got := os.Getenv("GLUE_TEST_FROM_SHELL"); got != "shell-value" {
		t.Fatalf("FROM_SHELL = %q, want shell-value (env file should not override)", got)
	}
}

func TestRunCLIResumesSession(t *testing.T) {
	t.Parallel()

	storeDir := t.TempDir()

	first := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("first")}}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run", "--id", "same", "--prompt", "first", "--store", storeDir,
	}, &stdout, &stderr, fakeFactory(first))
	if code != 0 {
		t.Fatalf("first code = %d stderr=%q", code, stderr.String())
	}

	second := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("second")}}
	stdout.Reset()
	stderr.Reset()
	code = runCLI(context.Background(), []string{
		"run", "--id", "same", "--prompt", "second", "--store", storeDir,
	}, &stdout, &stderr, fakeFactory(second))
	if code != 0 {
		t.Fatalf("second code = %d stderr=%q", code, stderr.String())
	}
	if len(second.requests) != 1 {
		t.Fatalf("second provider calls = %d, want 1", len(second.requests))
	}
	if got := len(second.requests[0].Messages); got != 3 {
		t.Fatalf("second request msg count = %d, want 3 (resumed user/assistant + new user)", got)
	}
}

func TestRunCLIProviderErrorExit(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{err: errors.New("provider failed")}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run", "--prompt", "fail", "--store", t.TempDir(),
	}, &stdout, &stderr, fakeFactory(provider))
	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "provider failed") {
		t.Fatalf("stderr = %q, want provider failed", stderr.String())
	}
}

func TestRunCLIMissingPrompt(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{"run"}, &stdout, &stderr, fakeFactory(&scriptedProvider{}))
	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "missing required --prompt") {
		t.Fatalf("stderr = %q, want missing prompt", stderr.String())
	}
}

func TestRunCLIServeBuildsDaemon(t *testing.T) {
	provider := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("served")}}
	storeDir := t.TempDir()
	workDir := t.TempDir()
	metadataPath := filepath.Join(t.TempDir(), "daemon.json")
	var captured serveConfig
	serve := func(ctx context.Context, cfg serveConfig, handler http.Handler, _ io.Writer) error {
		captured = cfg

		health := httptest.NewRecorder()
		handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
		if health.Code != http.StatusOK {
			t.Fatalf("health status = %d, want %d", health.Code, http.StatusOK)
		}

		ts := httptest.NewServer(handler)
		defer ts.Close()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/sessions/default/runs", strings.NewReader(`{"text":"go"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("start status = %d, want %d", resp.StatusCode, http.StatusCreated)
		}
		var start struct {
			EventsURL string `json:"events_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
			t.Fatal(err)
		}

		eventsReq, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+start.EventsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		eventsReq.Header.Set("Authorization", "Bearer test-token")
		eventsResp, err := http.DefaultClient.Do(eventsReq)
		if err != nil {
			t.Fatal(err)
		}
		defer eventsResp.Body.Close()
		if eventsResp.StatusCode != http.StatusOK {
			t.Fatalf("events status = %d, want %d", eventsResp.StatusCode, http.StatusOK)
		}
		if _, err := io.Copy(io.Discard, eventsResp.Body); err != nil {
			t.Fatal(err)
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := runCLIWithServe(context.Background(), []string{
		"serve",
		"--listen", "127.0.0.1:0",
		"--token", "test-token",
		"--metadata", metadataPath,
		"--model", "gemini/custom",
		"--store", storeDir,
		"--work", workDir,
	}, &stdout, &stderr, fakeFactory(provider), serve)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if captured.ListenAddr != "127.0.0.1:0" || captured.Token != "test-token" || captured.TokenSource != "flag" {
		t.Fatalf("serve config auth/listen = %+v", captured)
	}
	if captured.Model != "custom" || captured.StoreDir != storeDir || captured.WorkDir != workDir || captured.MetadataPath != metadataPath {
		t.Fatalf("serve config paths/model = %+v", captured)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.requests))
	}
	if got := provider.requests[0].Model; got != "custom" {
		t.Fatalf("provider model = %q, want custom", got)
	}
}

func TestRunCLIServeGeneratesTokenWhenMetadataEnabled(t *testing.T) {
	t.Setenv("GLUE_DAEMON_TOKEN", "")
	var captured serveConfig
	serve := func(_ context.Context, cfg serveConfig, _ http.Handler, _ io.Writer) error {
		captured = cfg
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := runCLIWithServe(context.Background(), []string{
		"serve",
		"--metadata", filepath.Join(t.TempDir(), "daemon.json"),
	}, &stdout, &stderr, fakeFactory(&scriptedProvider{}), serve)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if captured.TokenSource != "generated" || len(captured.Token) != 64 {
		t.Fatalf("generated token = source %q len %d", captured.TokenSource, len(captured.Token))
	}
}

func TestRunCLIServeMetadataDisabledRequiresKnownToken(t *testing.T) {
	t.Setenv("GLUE_DAEMON_TOKEN", "")
	var stdout, stderr bytes.Buffer
	code := runCLIWithServe(context.Background(), []string{
		"serve",
		"--metadata", "",
	}, &stdout, &stderr, fakeFactory(&scriptedProvider{}), func(context.Context, serveConfig, http.Handler, io.Writer) error {
		t.Fatal("serve should not be called")
		return nil
	})
	if code == 0 {
		t.Fatal("code = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "metadata disabled requires") {
		t.Fatalf("stderr = %q, want metadata error", stderr.String())
	}
}

func TestRunCLIServeMetadataDisabledAllowsEnvToken(t *testing.T) {
	t.Setenv("GLUE_DAEMON_TOKEN", "env-token")
	var captured serveConfig
	var stdout, stderr bytes.Buffer
	code := runCLIWithServe(context.Background(), []string{
		"serve",
		"--metadata", "",
	}, &stdout, &stderr, fakeFactory(&scriptedProvider{}), func(_ context.Context, cfg serveConfig, _ http.Handler, _ io.Writer) error {
		captured = cfg
		return nil
	})
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if captured.Token != "env-token" || captured.TokenSource != "GLUE_DAEMON_TOKEN" {
		t.Fatalf("token config = %+v", captured)
	}
}

func TestRunCLIServeLoadsEnvBeforeTokenResolution(t *testing.T) {
	os.Unsetenv("GLUE_DAEMON_TOKEN")
	t.Cleanup(func() { os.Unsetenv("GLUE_DAEMON_TOKEN") })
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("GLUE_DAEMON_TOKEN=file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var captured serveConfig
	var stdout, stderr bytes.Buffer
	code := runCLIWithServe(context.Background(), []string{
		"serve",
		"--metadata", "",
		"--env", envPath,
	}, &stdout, &stderr, fakeFactory(&scriptedProvider{}), func(_ context.Context, cfg serveConfig, _ http.Handler, _ io.Writer) error {
		captured = cfg
		return nil
	})
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if captured.Token != "file-token" || captured.TokenSource != "GLUE_DAEMON_TOKEN" {
		t.Fatalf("token config = %+v", captured)
	}
}

func TestServeDaemonWritesMetadataAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	metadataPath := filepath.Join(t.TempDir(), "glue", "daemon.json")
	var stdout bytes.Buffer

	err := serveDaemon(ctx, serveConfig{
		ListenAddr:      "127.0.0.1:0",
		Token:           "secret-token",
		TokenSource:     "test",
		MetadataPath:    metadataPath,
		ShutdownTimeout: time.Second,
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), &stdout)
	if err != nil {
		t.Fatalf("serveDaemon: %v", err)
	}

	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta daemonMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Version != 1 || meta.Token != "secret-token" || !strings.HasPrefix(meta.BaseURL, "http://127.0.0.1:") || meta.PID == 0 {
		t.Fatalf("metadata = %+v", meta)
	}
	info, err := os.Stat(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("metadata mode = %v, want 0600", info.Mode().Perm())
	}
	if strings.Contains(stdout.String(), "secret-token") {
		t.Fatalf("stdout leaked token: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "metadata: "+metadataPath) {
		t.Fatalf("stdout = %q, want metadata path", stdout.String())
	}
}

func TestResolveConnectConfigUsesMetadataAndOverrides(t *testing.T) {
	metadataPath := filepath.Join(t.TempDir(), "daemon.json")
	if err := writeDaemonMetadata(metadataPath, daemonMetadata{
		Version: 1,
		BaseURL: "http://metadata",
		Token:   "meta-token",
		PID:     123,
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := resolveConnectConfig(connectConfig{MetadataPath: metadataPath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://metadata" || cfg.Token != "meta-token" || cfg.SessionID != "default" || !strings.HasPrefix(cfg.ClientID, "cli:") {
		t.Fatalf("metadata config = %+v", cfg)
	}

	cfg, err = resolveConnectConfig(connectConfig{
		MetadataPath: metadataPath,
		BaseURL:      "http://override/",
		Token:        "override-token",
		SessionID:    "work",
		ClientID:     "cli:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://override" || cfg.Token != "override-token" || cfg.SessionID != "work" || cfg.ClientID != "cli:test" {
		t.Fatalf("override config = %+v", cfg)
	}

	t.Setenv("GLUE_DAEMON_TOKEN", "env-token")
	cfg, err = resolveConnectConfig(connectConfig{
		MetadataPath: filepath.Join(t.TempDir(), "missing.json"),
		BaseURL:      "http://explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://explicit" || cfg.Token != "env-token" {
		t.Fatalf("env fallback config = %+v", cfg)
	}
}

func TestRunCLIConnectStartsRunAndStreamsText(t *testing.T) {
	var got startRunPayload
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/dev/runs":
			if r.Method != http.MethodPost {
				t.Fatalf("start method = %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("auth = %q", auth)
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			writeJSONResponse(t, w, http.StatusCreated, startRunResult{RunID: "run_1", SessionID: "dev", EventsURL: "/v1/runs/run_1/events"})
		case "/v1/runs/run_1/events":
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("events auth = %q", auth)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSETest(t, w, daemon.EventEnvelope{Type: "text_delta", Payload: map[string]any{"delta": "hi"}})
			writeSSETest(t, w, daemon.EventEnvelope{Type: "run_done"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--prompt", "hello",
		"--id", "dev",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
		"--model", "gemini/custom",
		"--role", "reviewer",
		"--max-turns", "3",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "hi\n" {
		t.Fatalf("stdout = %q, want streamed text", stdout.String())
	}
	if got.Text != "hello" || got.Model != "gemini/custom" || got.Role != "reviewer" || got.MaxTurns != 3 || got.ClientID == "" {
		t.Fatalf("start payload = %+v", got)
	}
}

func TestRunCLIConnectUsageReportsTokens(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/default/runs":
			writeJSONResponse(t, w, http.StatusCreated, startRunResult{RunID: "run_1", SessionID: "default", EventsURL: "/v1/runs/run_1/events"})
		case "/v1/runs/run_1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSETest(t, w, daemon.EventEnvelope{Type: "text_delta", Payload: map[string]any{"delta": "done"}})
			writeSSETest(t, w, daemon.EventEnvelope{Type: "run_done", Payload: connectRunDonePayload{
				NewMessages: []glue.Message{
					{
						Role: glue.MessageRoleAssistant,
						Usage: &glue.Usage{
							InputTokens:     3,
							OutputTokens:    2,
							CacheReadTokens: 1,
							TotalTokens:     5,
						},
					},
					{
						Role: glue.MessageRoleAssistant,
						Usage: &glue.Usage{
							InputTokens:  4,
							OutputTokens: 1,
							TotalTokens:  5,
						},
					},
				},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--prompt", "hello",
		"--usage",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "done\n" {
		t.Fatalf("stdout = %q, want streamed text", stdout.String())
	}
	if got, want := stderr.String(), "usage: input=7 output=3 cache_read=1 total=10\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRunCLIConnectUsageReportsEstimatedCost(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/default/runs":
			writeJSONResponse(t, w, http.StatusCreated, startRunResult{RunID: "run_1", SessionID: "default", EventsURL: "/v1/runs/run_1/events"})
		case "/v1/runs/run_1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSETest(t, w, daemon.EventEnvelope{Type: "text_delta", Payload: map[string]any{"delta": "done"}})
			writeSSETest(t, w, daemon.EventEnvelope{Type: "run_done", Payload: connectRunDonePayload{
				NewMessages: []glue.Message{
					{
						Role: glue.MessageRoleAssistant,
						Usage: &glue.Usage{
							InputTokens:  1_000_000,
							OutputTokens: 500_000,
							TotalTokens:  1_500_000,
						},
					},
					{
						Role: glue.MessageRoleAssistant,
						Usage: &glue.Usage{
							CacheReadTokens:  250_000,
							CacheWriteTokens: 100_000,
							TotalTokens:      350_000,
						},
					},
				},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--prompt", "hello",
		"--usage",
		"--usage-input-price", "1",
		"--usage-output-price", "2",
		"--usage-cache-read-price", "0.25",
		"--usage-cache-write-price", "3",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if got, want := stderr.String(), "usage: input=1000000 output=500000 cache_read=250000 cache_write=100000 total=1850000 cost_usd=2.362500\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRunCLIConnectUsageSilentWhenMissing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/default/runs":
			writeJSONResponse(t, w, http.StatusCreated, startRunResult{RunID: "run_1", SessionID: "default", EventsURL: "/v1/runs/run_1/events"})
		case "/v1/runs/run_1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSETest(t, w, daemon.EventEnvelope{Type: "run_done", Payload: connectRunDonePayload{Text: "fallback"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--prompt", "hello",
		"--usage",
		"--usage-input-price", "1",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "fallback\n" {
		t.Fatalf("stdout = %q, want fallback text", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want no usage output", stderr.String())
	}
}

func TestRunCLIConnectListsTools(t *testing.T) {
	var sawTools bool
	var sawRun bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tools":
			sawTools = true
			if r.Method != http.MethodGet {
				t.Fatalf("tools method = %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("tools auth = %q", auth)
			}
			writeJSONResponse(t, w, http.StatusOK, daemonToolCatalog{Tools: []daemonToolCatalogEntry{{
				Name:                    "mcp_fake_echo",
				Description:             "MCP fake: echoes text",
				Parameters:              json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
				RequiresPermission:      true,
				PermissionAction:        "mcp_call",
				PermissionTargetPreview: "fake.echo",
			}}})
		case "/v1/sessions/default/runs":
			sawRun = true
			http.Error(w, "unexpected run", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--tools",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if !sawTools || sawRun {
		t.Fatalf("sawTools=%v sawRun=%v", sawTools, sawRun)
	}
	for _, want := range []string{
		"mcp_fake_echo",
		"description: MCP fake: echoes text",
		"permission: mcp_call fake.echo",
		`parameters: {"type":"object","properties":{"text":{"type":"string"}}}`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunCLIConnectListsToolsJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools" {
			http.NotFound(w, r)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, daemonToolCatalog{Tools: []daemonToolCatalogEntry{{
			Name:               "shell_exec",
			RequiresPermission: true,
			PermissionAction:   "exec",
		}}})
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--tools-json",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	var catalog daemonToolCatalog
	if err := json.Unmarshal(stdout.Bytes(), &catalog); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if len(catalog.Tools) != 1 || catalog.Tools[0].Name != "shell_exec" || catalog.Tools[0].PermissionAction != "exec" {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestRunCLIConnectListsSkills(t *testing.T) {
	var sawSkills bool
	var sawRun bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/skills":
			sawSkills = true
			if r.Method != http.MethodGet {
				t.Fatalf("skills method = %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("skills auth = %q", auth)
			}
			writeJSONResponse(t, w, http.StatusOK, daemonSkillCatalog{Skills: []daemon.SkillCatalogEntry{{
				Name:        "triage",
				Description: "Triage one issue",
			}}})
		case "/v1/sessions/default/runs":
			sawRun = true
			http.Error(w, "unexpected run", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--skills",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if !sawSkills || sawRun {
		t.Fatalf("sawSkills=%v sawRun=%v", sawSkills, sawRun)
	}
	for _, want := range []string{"triage", "description: Triage one issue"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunCLIConnectListsSkillsJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/skills" {
			http.NotFound(w, r)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, daemonSkillCatalog{Skills: []daemon.SkillCatalogEntry{{
			Name:        "daily",
			Description: "Daily plan",
		}}})
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--skills-json",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	var catalog daemonSkillCatalog
	if err := json.Unmarshal(stdout.Bytes(), &catalog); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if len(catalog.Skills) != 1 || catalog.Skills[0].Name != "daily" || catalog.Skills[0].Description != "Daily plan" {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestRunCLIConnectListsRoles(t *testing.T) {
	var sawRoles bool
	var sawRun bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/roles":
			sawRoles = true
			writeJSONResponse(t, w, http.StatusOK, daemonRoleCatalog{Roles: []daemon.RoleCatalogEntry{{
				Name:        "reviewer",
				Description: "Reviews diffs",
				Model:       "fast-model",
			}}})
		case "/v1/sessions/default/runs":
			sawRun = true
			http.Error(w, "unexpected run", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--roles",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if !sawRoles || sawRun {
		t.Fatalf("sawRoles=%v sawRun=%v", sawRoles, sawRun)
	}
	for _, want := range []string{"reviewer", "description: Reviews diffs", "model: fast-model"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunCLIConnectListsRolesJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/roles" {
			http.NotFound(w, r)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, daemonRoleCatalog{Roles: []daemon.RoleCatalogEntry{{
			Name:  "reviewer",
			Model: "fast-model",
		}}})
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--roles-json",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	var catalog daemonRoleCatalog
	if err := json.Unmarshal(stdout.Bytes(), &catalog); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if len(catalog.Roles) != 1 || catalog.Roles[0].Name != "reviewer" || catalog.Roles[0].Model != "fast-model" {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestRunCLIConnectRunsSkill(t *testing.T) {
	var payload startRunPayload
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/work/runs":
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			writeJSONResponse(t, w, http.StatusCreated, startRunResult{RunID: "run_1", SessionID: "work", EventsURL: "/v1/runs/run_1/events"})
		case "/v1/runs/run_1/events":
			writeSSETest(t, w, daemon.EventEnvelope{Type: "run_done", Payload: connectRunDonePayload{Text: "triaged"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--skill", "triage",
		"--arg", "issue=GLUE-123",
		"--id", "work",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "triaged\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if payload.Text != "" || payload.Skill != "triage" || payload.Arguments["issue"] != "GLUE-123" || payload.ClientID == "" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestRunCLIConnectListsMCPResources(t *testing.T) {
	size := int64(1234)
	var sawResources bool
	var sawRun bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/mcp/resources":
			sawResources = true
			if r.Method != http.MethodGet {
				t.Fatalf("resources method = %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("resources auth = %q", auth)
			}
			writeJSONResponse(t, w, http.StatusOK, daemonMCPResourceCatalog{Resources: []daemon.MCPResourceCatalogEntry{{
				Server:      "filesystem",
				URI:         "file:///workspace/README.md",
				Name:        "readme",
				Title:       "Project README",
				Description: "repository overview",
				MIMEType:    "text/markdown",
				Annotations: map[string]any{"audience": []string{"assistant"}},
				Size:        &size,
			}}})
		case "/v1/sessions/default/runs":
			sawRun = true
			http.Error(w, "unexpected run", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--mcp-resources",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if !sawResources || sawRun {
		t.Fatalf("sawResources=%v sawRun=%v", sawResources, sawRun)
	}
	for _, want := range []string{
		"file:///workspace/README.md",
		"server: filesystem",
		"name: readme",
		"title: Project README",
		"description: repository overview",
		"mime_type: text/markdown",
		"size: 1234",
		`annotations: {"audience":["assistant"]}`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunCLIConnectListsMCPResourcesJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mcp/resources" {
			http.NotFound(w, r)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, daemonMCPResourceCatalog{Resources: []daemon.MCPResourceCatalogEntry{{
			Server: "filesystem",
			URI:    "file:///workspace/README.md",
			Name:   "readme",
		}}})
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--mcp-resources-json",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	var catalog daemonMCPResourceCatalog
	if err := json.Unmarshal(stdout.Bytes(), &catalog); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if len(catalog.Resources) != 1 || catalog.Resources[0].URI != "file:///workspace/README.md" {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestRunCLIConnectListsMCPPrompts(t *testing.T) {
	var sawPrompts bool
	var sawRun bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/mcp/prompts":
			sawPrompts = true
			if r.Method != http.MethodGet {
				t.Fatalf("prompts method = %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("prompts auth = %q", auth)
			}
			writeJSONResponse(t, w, http.StatusOK, daemonMCPPromptCatalog{Prompts: []daemon.MCPPromptCatalogEntry{{
				Server:      "linear",
				Name:        "daily_brief",
				Title:       "Daily Brief",
				Description: "Draft a concise daily briefing",
				Arguments: []daemon.MCPPromptCatalogArgument{{
					Name:        "topic",
					Description: "Subject to brief",
					Required:    true,
				}},
			}}})
		case "/v1/sessions/default/runs":
			sawRun = true
			http.Error(w, "unexpected run", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--mcp-prompts",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if !sawPrompts || sawRun {
		t.Fatalf("sawPrompts=%v sawRun=%v", sawPrompts, sawRun)
	}
	for _, want := range []string{
		"daily_brief",
		"server: linear",
		"title: Daily Brief",
		"description: Draft a concise daily briefing",
		"arguments:",
		"topic required - Subject to brief",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunCLIConnectListsMCPPromptsJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mcp/prompts" {
			http.NotFound(w, r)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, daemonMCPPromptCatalog{Prompts: []daemon.MCPPromptCatalogEntry{{
			Server: "linear",
			Name:   "daily_brief",
		}}})
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--mcp-prompts-json",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	var catalog daemonMCPPromptCatalog
	if err := json.Unmarshal(stdout.Bytes(), &catalog); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if len(catalog.Prompts) != 1 || catalog.Prompts[0].Name != "daily_brief" {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestRunCLIConnectReadsMCPResource(t *testing.T) {
	var sawRead bool
	var sawRun bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/mcp/resources/read":
			sawRead = true
			if r.Method != http.MethodPost {
				t.Fatalf("read method = %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("read auth = %q", auth)
			}
			var req daemon.MCPReadResourceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.Server != "filesystem" || req.URI != "file:///workspace/README.md" {
				t.Fatalf("read request = %+v", req)
			}
			text := "# Project README\n\nHello from daemon."
			writeJSONResponse(t, w, http.StatusOK, daemon.MCPResourceReadResponse{
				Server: req.Server,
				URI:    req.URI,
				Contents: []daemon.MCPResourceContent{{
					URI:      req.URI,
					MIMEType: "text/markdown",
					Text:     &text,
					Meta:     map[string]any{"source": "test"},
				}},
			})
		case "/v1/sessions/default/runs":
			sawRun = true
			http.Error(w, "unexpected run", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--mcp-read",
		"--server", "filesystem",
		"--uri", "file:///workspace/README.md",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if !sawRead || sawRun {
		t.Fatalf("sawRead=%v sawRun=%v", sawRead, sawRun)
	}
	for _, want := range []string{
		"file:///workspace/README.md",
		"server: filesystem",
		"requested_uri: file:///workspace/README.md",
		"mime_type: text/markdown",
		`meta: {"source":"test"}`,
		"Hello from daemon.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunCLIConnectReadsMCPResourceJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mcp/resources/read" {
			http.NotFound(w, r)
			return
		}
		text := "hello"
		writeJSONResponse(t, w, http.StatusOK, daemon.MCPResourceReadResponse{
			Server: "filesystem",
			URI:    "file:///workspace/README.md",
			Contents: []daemon.MCPResourceContent{{
				URI:  "file:///workspace/README.md",
				Text: &text,
			}},
		})
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--mcp-read-json",
		"--server", "filesystem",
		"--uri", "file:///workspace/README.md",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	var read daemon.MCPResourceReadResponse
	if err := json.Unmarshal(stdout.Bytes(), &read); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if read.Server != "filesystem" || len(read.Contents) != 1 || read.Contents[0].Text == nil {
		t.Fatalf("read = %+v", read)
	}
}

func TestRunCLIConnectRendersMCPPrompt(t *testing.T) {
	var sawPrompt bool
	var sawRun bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/mcp/prompts/get":
			sawPrompt = true
			if r.Method != http.MethodPost {
				t.Fatalf("prompt method = %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("prompt auth = %q", auth)
			}
			var req daemon.MCPPromptRenderRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.Server != "linear" || req.Name != "daily_brief" || req.Arguments["topic"] != "Go" {
				t.Fatalf("prompt request = %+v", req)
			}
			writeJSONResponse(t, w, http.StatusOK, daemon.MCPPromptRenderResponse{
				Server:      req.Server,
				Name:        req.Name,
				Description: "Rendered daily briefing prompt",
				Messages: []daemon.MCPPromptMessage{{
					Role:    "user",
					Content: json.RawMessage(`{"type":"text","text":"Brief me on Go."}`),
				}},
			})
		case "/v1/sessions/default/runs":
			sawRun = true
			http.Error(w, "unexpected run", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--mcp-prompt",
		"--server", "linear",
		"--name", "daily_brief",
		"--arg", "topic=Go",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if !sawPrompt || sawRun {
		t.Fatalf("sawPrompt=%v sawRun=%v", sawPrompt, sawRun)
	}
	for _, want := range []string{
		"daily_brief",
		"server: linear",
		"description: Rendered daily briefing prompt",
		"messages:",
		"Brief me on Go.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunCLIConnectRendersMCPPromptJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mcp/prompts/get" {
			http.NotFound(w, r)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, daemon.MCPPromptRenderResponse{
			Server: "linear",
			Name:   "daily_brief",
			Messages: []daemon.MCPPromptMessage{{
				Role:    "user",
				Content: json.RawMessage(`{"type":"text","text":"Brief me."}`),
			}},
		})
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--mcp-prompt-json",
		"--server", "linear",
		"--name", "daily_brief",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	var rendered daemon.MCPPromptRenderResponse
	if err := json.Unmarshal(stdout.Bytes(), &rendered); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if rendered.Server != "linear" || rendered.Name != "daily_brief" || len(rendered.Messages) != 1 {
		t.Fatalf("rendered = %+v", rendered)
	}
}

func TestRunCLIConnectShowsStatus(t *testing.T) {
	var sawStatus bool
	var sawRun bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			sawStatus = true
			if r.Method != http.MethodGet {
				t.Fatalf("status method = %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("status auth = %q", auth)
			}
			writeJSONResponse(t, w, http.StatusOK, daemonStatus{
				OK:           true,
				Version:      1,
				ActiveRuns:   2,
				ToolsCount:   5,
				Capabilities: []string{"runs", "tools", "status"},
			})
		case "/v1/sessions/default/runs":
			sawRun = true
			http.Error(w, "unexpected run", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--status",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if !sawStatus || sawRun {
		t.Fatalf("sawStatus=%v sawRun=%v", sawStatus, sawRun)
	}
	for _, want := range []string{
		"status: ok",
		"version: 1",
		"active_runs: 2",
		"tools_count: 5",
		"capabilities: runs, tools, status",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunCLIConnectShowsStatusJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, daemonStatus{
			OK:           true,
			Version:      1,
			Capabilities: []string{"status"},
		})
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--status-json",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	var status daemonStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if !status.OK || status.Version != 1 || len(status.Capabilities) != 1 || status.Capabilities[0] != "status" {
		t.Fatalf("status = %+v", status)
	}
}

func TestRunCLIConnectInspectsDaemon(t *testing.T) {
	var sawStatus bool
	var sawTools bool
	var sawRun bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			sawStatus = true
			if r.Method != http.MethodGet {
				t.Fatalf("status method = %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("status auth = %q", auth)
			}
			writeJSONResponse(t, w, http.StatusOK, daemonStatus{
				OK:           true,
				Version:      1,
				ActiveRuns:   2,
				ToolsCount:   1,
				Capabilities: []string{"runs", "tools", "status"},
			})
		case "/v1/tools":
			sawTools = true
			if r.Method != http.MethodGet {
				t.Fatalf("tools method = %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
				t.Fatalf("tools auth = %q", auth)
			}
			writeJSONResponse(t, w, http.StatusOK, daemonToolCatalog{Tools: []daemonToolCatalogEntry{{
				Name:                    "mcp_fake_echo",
				Description:             "MCP fake: echoes text",
				RequiresPermission:      true,
				PermissionAction:        "mcp_call",
				PermissionTargetPreview: "fake.echo",
			}}})
		case "/v1/sessions/default/runs":
			sawRun = true
			http.Error(w, "unexpected run", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--inspect",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if !sawStatus || !sawTools || sawRun {
		t.Fatalf("sawStatus=%v sawTools=%v sawRun=%v", sawStatus, sawTools, sawRun)
	}
	for _, want := range []string{
		"status: ok",
		"active_runs: 2",
		"tools_count: 1",
		"tools:",
		"mcp_fake_echo",
		"description: MCP fake: echoes text",
		"permission: mcp_call fake.echo",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunCLIConnectInspectsDaemonJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			writeJSONResponse(t, w, http.StatusOK, daemonStatus{
				OK:           true,
				Version:      1,
				Capabilities: []string{"status", "tools"},
			})
		case "/v1/tools":
			writeJSONResponse(t, w, http.StatusOK, daemonToolCatalog{Tools: []daemonToolCatalogEntry{{
				Name:               "shell_exec",
				RequiresPermission: true,
				PermissionAction:   "exec",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--inspect-json",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	var inspect daemonInspect
	if err := json.Unmarshal(stdout.Bytes(), &inspect); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if !inspect.Status.OK || inspect.Status.Version != 1 {
		t.Fatalf("status = %+v", inspect.Status)
	}
	if len(inspect.Tools) != 1 || inspect.Tools[0].Name != "shell_exec" || inspect.Tools[0].PermissionAction != "exec" {
		t.Fatalf("tools = %+v", inspect.Tools)
	}
}

func TestRunCLIConnectInspectIncludesMCPCatalogs(t *testing.T) {
	var sawSkills bool
	var sawRoles bool
	var sawResources bool
	var sawPrompts bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			writeJSONResponse(t, w, http.StatusOK, daemonStatus{
				OK:           true,
				Version:      1,
				Capabilities: []string{"status", "tools", "skills", "roles", "mcp_resources", "mcp_prompts"},
			})
		case "/v1/tools":
			writeJSONResponse(t, w, http.StatusOK, daemonToolCatalog{})
		case "/v1/skills":
			sawSkills = true
			writeJSONResponse(t, w, http.StatusOK, daemonSkillCatalog{Skills: []daemon.SkillCatalogEntry{{
				Name:        "triage",
				Description: "Triage one issue",
			}}})
		case "/v1/roles":
			sawRoles = true
			writeJSONResponse(t, w, http.StatusOK, daemonRoleCatalog{Roles: []daemon.RoleCatalogEntry{{
				Name:  "reviewer",
				Model: "fast-model",
			}}})
		case "/v1/mcp/resources":
			sawResources = true
			writeJSONResponse(t, w, http.StatusOK, daemonMCPResourceCatalog{Resources: []daemon.MCPResourceCatalogEntry{{
				Server: "filesystem",
				URI:    "file:///workspace/README.md",
				Name:   "readme",
			}}})
		case "/v1/mcp/prompts":
			sawPrompts = true
			writeJSONResponse(t, w, http.StatusOK, daemonMCPPromptCatalog{Prompts: []daemon.MCPPromptCatalogEntry{{
				Server: "linear",
				Name:   "daily_brief",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--inspect",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if !sawSkills || !sawRoles || !sawResources || !sawPrompts {
		t.Fatalf("sawSkills=%v sawRoles=%v sawResources=%v sawPrompts=%v", sawSkills, sawRoles, sawResources, sawPrompts)
	}
	for _, want := range []string{
		"skills:",
		"triage",
		"roles:",
		"reviewer",
		"model: fast-model",
		"mcp_resources:",
		"file:///workspace/README.md",
		"mcp_prompts:",
		"daily_brief",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunCLIConnectRequiresPromptUnlessInspectMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--base-url", "http://daemon",
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code == 0 {
		t.Fatal("code = 0, want prompt validation failure")
	}
	if !strings.Contains(stderr.String(), "missing required --prompt or --skill") {
		t.Fatalf("stderr = %q, want missing prompt or skill error", stderr.String())
	}
}

func TestRunCLIConnectRejectsPromptAndSkillTogether(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--prompt", "hello",
		"--skill", "triage",
		"--base-url", "http://daemon",
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code == 0 {
		t.Fatal("code = 0, want prompt/skill validation failure")
	}
	if !strings.Contains(stderr.String(), "choose only one of --prompt or --skill") {
		t.Fatalf("stderr = %q, want prompt/skill conflict error", stderr.String())
	}
}

func TestRunCLIConnectRejectsMultipleInspectModes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--status",
		"--inspect",
		"--base-url", "http://daemon",
		"--token", "tok",
		"--metadata", "",
	}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code == 0 {
		t.Fatal("code = 0, want inspect mode validation failure")
	}
	if !strings.Contains(stderr.String(), "choose only one") {
		t.Fatalf("stderr = %q, want mode conflict error", stderr.String())
	}
}

func TestRunCLIConnectPostsPermissionDecision(t *testing.T) {
	decisionCh := make(chan connectPermissionDecision, 1)
	observedDecisionCh := make(chan connectPermissionDecision, 1)
	clientIDCh := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/default/runs":
			writeJSONResponse(t, w, http.StatusCreated, startRunResult{RunID: "run_1", SessionID: "default", EventsURL: "/v1/runs/run_1/events"})
		case "/v1/runs/run_1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSETest(t, w, daemon.EventEnvelope{Type: "permission_request", Payload: connectPermissionPayload{
				PermissionID: "perm_1",
				Request: glue.PermissionRequest{
					Tool:   "shell_exec",
					Action: "exec",
					Target: "go test ./...",
				},
			}})
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case decision := <-decisionCh:
				observedDecisionCh <- decision
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for permission decision")
			}
			writeSSETest(t, w, daemon.EventEnvelope{Type: "text_delta", Payload: map[string]any{"delta": "done"}})
			writeSSETest(t, w, daemon.EventEnvelope{Type: "run_done"})
		case "/v1/runs/run_1/permissions/perm_1/decision":
			clientIDCh <- r.Header.Get("X-Glue-Client-ID")
			var decision connectPermissionDecision
			if err := json.NewDecoder(r.Body).Decode(&decision); err != nil {
				t.Fatal(err)
			}
			decisionCh <- decision
			writeJSONResponse(t, w, http.StatusOK, map[string]any{"accepted": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{
		"connect",
		"--prompt", "run tests",
		"--base-url", ts.URL,
		"--token", "tok",
		"--metadata", "",
		"--client-id", "cli:test",
	}, strings.NewReader("t\n"), &stdout, &stderr, fakeFactory(nil), nil, http.DefaultClient)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	decision := <-observedDecisionCh
	if !decision.Allow || decision.RememberFor != "session_target" {
		t.Fatalf("decision = %+v, want session_target allow", decision)
	}
	if got := <-clientIDCh; got != "cli:test" {
		t.Fatalf("client id header = %q", got)
	}
	if stdout.String() != "done\n" {
		t.Fatalf("stdout = %q, want done", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Permission requested: shell_exec exec") {
		t.Fatalf("stderr = %q, want permission prompt", stderr.String())
	}
}

func TestCancelDaemonRunDeletesRun(t *testing.T) {
	deleted := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
			t.Fatalf("auth = %q", auth)
		}
		deleted <- r.URL.Path
		writeJSONResponse(t, w, http.StatusAccepted, map[string]any{"canceled": true})
	}))
	defer ts.Close()

	err := cancelDaemonRun(context.Background(), connectConfig{BaseURL: ts.URL, Token: "tok"}, "run_1", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-deleted; got != "/v1/runs/run_1" {
		t.Fatalf("delete path = %q", got)
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
	}, &stdout, &stderr, defaultGeminiFactory)
	if code == 0 {
		t.Fatal("code = 0, want nonzero for missing GEMINI_API_KEY")
	}
	if !strings.Contains(stderr.String(), "GEMINI_API_KEY") {
		t.Fatalf("stderr = %q, want GEMINI_API_KEY hint", stderr.String())
	}
}
