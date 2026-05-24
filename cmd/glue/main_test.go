package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
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
