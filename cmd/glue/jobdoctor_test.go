// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/jobs"
)

type doctorFakeAPI struct{ jobAPI }

func (doctorFakeAPI) Close() error { return nil }
func (doctorFakeAPI) List(context.Context, jobs.Filter) ([]jobs.Job, error) {
	return []jobs.Job{{ID: "job-1"}}, nil
}

func doctorEnv(t *testing.T, push bool) {
	t.Helper()
	clearJobTestEnv(t)
	origAPI, origKube, origGH := newAXJobAPI, resolveKubeTarget, resolveGitHubCLIToken
	t.Cleanup(func() { newAXJobAPI, resolveKubeTarget, resolveGitHubCLIToken = origAPI, origKube, origGH })
	newAXJobAPI = func(context.Context, axbackend.Options) (jobAPI, error) { return doctorFakeAPI{}, nil }
	resolveKubeTarget = func(string, string) (string, string, error) { return "/tmp/kc", "kind-glue-ax", nil }
	resolveGitHubCLIToken = func(context.Context) string { return "" }
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_doctortoken1234567890" || r.URL.Path != "/repos/erain/glue-sandbox" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"permissions": map[string]bool{"push": push}})
	}))
	t.Cleanup(gh.Close)
	t.Setenv("GLUE_GITHUB_API_URL", gh.URL)
	t.Setenv("GITHUB_TOKEN", "ghp_doctortoken1234567890")
	t.Setenv("GLUE_AX_IMAGE", "localhost:5001/glue-task:dev")
}

func runDoctor(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	args := append([]string{"doctor", "--ax", "--repo", "https://github.com/erain/glue-sandbox"}, extra...)
	err := jobCommand(context.Background(), args, strings.NewReader(""), &out, &out, nil)
	return out.String(), err
}

func TestJobDoctor(t *testing.T) {
	doctorEnv(t, true)
	t.Setenv("GEMINI_API_KEY", "AIzaDoctorKey12345678901234")
	out, err := runDoctor(t)
	if err != nil || strings.Contains(out, "FAIL") {
		t.Fatalf("all checks should pass: err=%v\n%s", err, out)
	}
	for _, want := range []string{"ok    target         AX context kind-glue-ax", "ax-server      reachable, 1 job(s)", "task image     localhost:5001/glue-task:dev", "repo           https://github.com/erain/glue-sandbox @ main", "github token   from env GITHUB_TOKEN, can push to erain/glue-sandbox", "provider key   gemini from env GEMINI_API_KEY"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ghp_doctortoken") || strings.Contains(out, "AIzaDoctorKey") {
		t.Fatalf("doctor printed a secret:\n%s", out)
	}
}

func TestJobDoctorFailures(t *testing.T) {
	doctorEnv(t, false)
	t.Setenv("GLUE_AX_IMAGE", "")
	out, err := runDoctor(t, "--json")
	if err == nil || !strings.Contains(err.Error(), "2 check(s) failed") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	var got struct {
		OK     bool          `json:"ok"`
		Checks []doctorCheck `json:"checks"`
	}
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil || got.OK {
		t.Fatalf("json: %v %s", jerr, out)
	}
	failed := map[string]string{}
	for _, c := range got.Checks {
		if !c.OK {
			failed[c.Name] = c.Fix
		}
	}
	if !strings.Contains(failed["provider key"], "GEMINI_API_KEY") || !strings.Contains(failed["github token"], "push access") || failed["task image"] != "" {
		t.Fatalf("failed checks = %v", failed)
	}
}

func TestJobTargetError(t *testing.T) {
	clearJobTestEnv(t)
	origKube := resolveKubeTarget
	t.Cleanup(func() { resolveKubeTarget = origKube })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	meta := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(meta, []byte(`{"version":1,"base_url":"http://`+addr+`","token":"stale","pid":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = jobCommand(context.Background(), []string{"list", "--metadata", meta}, strings.NewReader(""), &out, &out, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot reach the job server http://"+addr+" (from local daemon metadata "+meta+")") || !strings.Contains(err.Error(), "glue job doctor") {
		t.Fatalf("err = %v", err)
	}
}
