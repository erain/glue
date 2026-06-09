// Tests for the "glue serve" subcommand and daemon metadata.

package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestRunCLIServeCodingAdvertisesTools(t *testing.T) {
	workDir := t.TempDir()
	var captured serveConfig
	serve := func(_ context.Context, cfg serveConfig, handler http.Handler, _ io.Writer) error {
		captured = cfg
		req := httptest.NewRequest(http.MethodGet, "/v1/tools", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("tools status = %d body=%s", rec.Code, rec.Body.String())
		}
		var catalog daemonToolCatalog
		if err := json.NewDecoder(rec.Body).Decode(&catalog); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range catalog.Tools {
			names = append(names, tool.Name)
			if tool.Name == "shell_exec" && (!tool.RequiresPermission || tool.PermissionAction != "exec") {
				t.Fatalf("shell_exec permission metadata = %+v", tool)
			}
		}
		for _, want := range []string{"read_file", "write_file", "shell_exec", "git_diff_branch", "git_log_branch"} {
			if !containsString(names, want) {
				t.Fatalf("tools = %v, missing %s", names, want)
			}
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := runCLIWithServe(context.Background(), []string{
		"serve",
		"--coding",
		"--work", workDir,
		"--allow-binary", "go",
		"--token", "test-token",
		"--metadata", filepath.Join(t.TempDir(), "daemon.json"),
	}, &stdout, &stderr, fakeFactory(&scriptedProvider{}), serve)
	if code != 0 {
		t.Fatalf("code = %d stderr=%q", code, stderr.String())
	}
	if captured.WorkDir != workDir {
		t.Fatalf("workdir = %q, want %q", captured.WorkDir, workDir)
	}
	if !strings.Contains(stderr.String(), "glue serve: coding tools enabled") {
		t.Fatalf("stderr = %q, want coding notice", stderr.String())
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
