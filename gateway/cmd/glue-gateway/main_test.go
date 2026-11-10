// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunStartsAndShutsDownCleanly(t *testing.T) {
	if err := run(t.Context(), nil, &bytes.Buffer{}, nil); err == nil || !strings.Contains(err.Error(), "--config is required") {
		t.Fatalf("expected missing --config error, got %v", err)
	}

	backendTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(backendTS.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.yaml")
	yamlCfg := "listen: 127.0.0.1:0\npublic_url: https://gw.example.com\nstate_dir: " + filepath.Join(dir, "state") + "\nauth:\n  mode: dev-token\n  token: test-gw-token\nbackends:\n  - name: local\n    type: local\n    url: " + backendTS.URL + "\n    token: local-token\ndefault_backend: local\n"
	if err := os.WriteFile(cfgPath, []byte(yamlCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"--config", cfgPath}, &bytes.Buffer{}, nil); err == nil || !strings.Contains(err.Error(), "--insecure-dev") {
		t.Fatalf("expected https:// dev-token refusal without --insecure-dev, got %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() {
		done <- run(ctx, []string{"--config", cfgPath, "--insecure-dev"}, &stderr, func(addr string) { ready <- addr })
	}()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run exited early: %v (stderr=%s)", err, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for gateway listener")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/v1/backends", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-gw-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/backends: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/backends status=%d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for clean shutdown")
	}
}
