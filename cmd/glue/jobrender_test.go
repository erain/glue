// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/jobs"
)

var updateGolden = flag.Bool("update", false, "rewrite golden transcript files")

type renderFakeAPI struct{ jobAPI }

func (renderFakeAPI) Result(context.Context, string) (jobs.Result, error) {
	return jobs.Result{
		JobID: "job-render0001", Status: jobs.Achieved, Branch: "glue/job-render0001", BaseSHA: "aaa", HeadSHA: "bbb", Iterations: 1,
		PRURL:  "https://github.com/erain/glue-sandbox/pull/9",
		Verify: []jobs.VerifyResult{{Argv: []string{"go", "test", "./..."}, ExitCode: 0, DurationMS: 640}},
	}, nil
}

func renderFixture(t *testing.T, name string, verbose bool) string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "jobevents", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out bytes.Buffer
	r := newJobRenderer(&out, verbose)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev eventlog.Envelope
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		if err := renderJobEvent(context.Background(), renderFakeAPI{}, "job-render0001", ev, r, bufio.NewReader(strings.NewReader("")), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out.String()
}

func checkGolden(t *testing.T, got, file string) {
	t.Helper()
	path := filepath.Join("testdata", "jobevents", file)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./cmd/glue -run TestJobRender -update)", err)
	}
	if got != string(want) {
		t.Fatalf("transcript differs from %s:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

func TestJobRenderGolden(t *testing.T) {
	got := renderFixture(t, "basic", false)
	checkGolden(t, got, "basic.golden")
	for _, line := range strings.Split(got, "\n") {
		if n := utf8.RuneCountInString(line); n > 160 {
			t.Fatalf("line has %d runes (max 160): %q", n, line)
		}
		if strings.Contains(line, "\x1b[") {
			t.Fatalf("control codes in output: %q", line)
		}
	}
	for _, want := range []string{"] planning", "] iteration 1 (4,210 tokens so far)", "] checking", "✓ list_dir .  0.1s", "✗ shell_exec go test ./...  2.4s", "… 10 more lines (--verbose shows all)", "tokens       18,934"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestJobRenderVerbose(t *testing.T) {
	got := renderFixture(t, "basic", true)
	checkGolden(t, got, "basic-verbose.golden")
	if !strings.Contains(got, "shout_test.go:25") || !strings.Contains(got, "stdout: ok") {
		t.Fatalf("--verbose should show all output:\n%s", got)
	}
}

func TestJobRenderSummary(t *testing.T) {
	got := renderFixture(t, "basic", false)
	_, summary, ok := strings.Cut(got, strings.Repeat("─", 40))
	if !ok {
		t.Fatalf("no summary block:\n%s", got)
	}
	for _, want := range []string{"status       achieved", "duration     1m11s", "branch       glue/job-render0001", "pull request https://github.com/erain/glue-sandbox/pull/9", "verify       ✓ go test ./... (exit 0, 0.6s)", "next         glue job result job-render0001"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}
