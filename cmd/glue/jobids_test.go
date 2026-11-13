// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue/internal/axbackend"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/jobs"
)

type listFakeAPI struct {
	jobAPI
	list []jobs.Job
	err  error
	n    int
}

func (f *listFakeAPI) List(context.Context, jobs.Filter) ([]jobs.Job, error) {
	f.n++
	return f.list, f.err
}

func TestResolveJobIDPrefix(t *testing.T) {
	api := &listFakeAPI{list: []jobs.Job{{ID: "job-55f83167728b"}, {ID: "job-55aa00000000"}, {ID: "job-0123456789ab"}}}
	ctx := context.Background()
	for in, want := range map[string]string{"latest": "job-55f83167728b", "55f8": "job-55f83167728b", "job-0123": "job-0123456789ab", "nomatch": "nomatch"} {
		if got, err := resolveJobID(ctx, api, in); err != nil || got != want {
			t.Fatalf("resolveJobID(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := resolveJobID(ctx, api, "55"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	api.n = 0
	if got, _ := resolveJobID(ctx, api, "job-55f83167728b"); got != "job-55f83167728b" || api.n != 0 {
		t.Fatalf("a full ID must not list jobs (lists: %d)", api.n)
	}
	failing := &listFakeAPI{err: errors.New("list not allowed")}
	if got, err := resolveJobID(ctx, failing, "abc"); err != nil || got != "abc" {
		t.Fatalf("list failure should fall back to the argument: %q %v", got, err)
	}
	if _, err := resolveJobID(ctx, &listFakeAPI{}, "latest"); err == nil {
		t.Fatal("latest with no jobs should fail")
	}
}

func TestJobListTable(t *testing.T) {
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	printJobTable(&out, []jobs.Job{
		{ID: "job-55f83167728b", Status: jobs.Running, CreatedAt: now.Add(-12 * time.Minute), Spec: jobs.Spec{Objective: "Add Shout\nwith a test", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox.git"}}},
		{ID: "job-0123456789ab", Status: jobs.Achieved, CreatedAt: now.Add(-50 * time.Hour), Spec: jobs.Spec{Objective: strings.Repeat("x", 200), Repo: jobs.RepoSpec{Path: "/home/me/src/app"}}},
	}, now, 120)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "OBJECTIVE") {
		t.Fatalf("table:\n%s", out.String())
	}
	if !strings.Contains(lines[1], "running") || !strings.Contains(lines[1], "12m") || !strings.Contains(lines[1], "erain/glue-sandbox") || !strings.Contains(lines[1], "Add Shout with a test") {
		t.Fatalf("row 1: %q", lines[1])
	}
	if !strings.Contains(lines[2], "2d") || !strings.Contains(lines[2], "app") || len([]rune(lines[2])) > 130 {
		t.Fatalf("row 2 (%d runes): %q", len([]rune(lines[2])), lines[2])
	}
	if strings.Index(lines[1], "running") != strings.Index(lines[2], "achieved") {
		t.Fatalf("status column not aligned:\n%s", out.String())
	}
}

type logsFakeAPI struct {
	jobAPI
	saved bool
}

func (f *logsFakeAPI) Show(context.Context, string) (jobs.Detail, error) {
	return jobs.Detail{Job: jobs.Job{ID: "job-0123456789ab", Status: jobs.Running, LastSeq: 3}}, nil
}
func (f *logsFakeAPI) SaveSeq(string, int64) error { f.saved = true; return nil }
func (f *logsFakeAPI) StreamEvents(ctx context.Context, _ string, after int64, h func(eventlog.Envelope) error) error {
	for seq := after + 1; seq <= 3; seq++ {
		if err := h(eventlog.Envelope{Seq: seq, Type: "operator_message", Payload: map[string]any{"text": fmt.Sprintf("m%d", seq)}}); err != nil {
			return err
		}
	}
	<-ctx.Done() // a live job's stream stays open
	return ctx.Err()
}

func TestJobLogsDoesNotFollow(t *testing.T) {
	api := &logsFakeAPI{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := jobLogs(ctx, api, "job-0123456789ab", 0, false, false, &out); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("logs followed the live stream instead of stopping at the current end")
	}
	if !strings.Contains(out.String(), "operator: m1") || !strings.Contains(out.String(), "operator: m3") || api.saved {
		t.Fatalf("out=%q saved=%v", out.String(), api.saved)
	}
	out.Reset()
	if err := jobLogs(ctx, api, "job-0123456789ab", 1, false, false, &out); err != nil || strings.Contains(out.String(), "m2") || !strings.Contains(out.String(), "m3") {
		t.Fatalf("--tail 1: %v %q", err, out.String())
	}
}

type answerFakeAPI struct {
	jobAPI
	id, qid, text string
}

func (f *answerFakeAPI) Close() error { return nil }
func (f *answerFakeAPI) Answer(_ context.Context, id, qid, text string) error {
	f.id, f.qid, f.text = id, qid, text
	return nil
}

func TestJobAnswer(t *testing.T) {
	clearJobTestEnv(t)
	orig := newAXJobAPI
	t.Cleanup(func() { newAXJobAPI = orig })
	api := &answerFakeAPI{}
	newAXJobAPI = func(context.Context, axbackend.Options) (jobAPI, error) { return api, nil }
	var out bytes.Buffer
	if err := jobCommand(context.Background(), []string{"answer", "--ax", "job-0123456789ab", "Yes,", "create", "Greet"}, strings.NewReader(""), &out, &out, nil); err != nil {
		t.Fatal(err)
	}
	if api.id != "job-0123456789ab" || api.text != "Yes, create Greet" || api.qid != "" {
		t.Fatalf("answer = %+v", api)
	}
	if err := jobCommand(context.Background(), []string{"answer", "--ax", "job-0123456789ab", "--question", "job-0123456789ab:question:7", "stop"}, strings.NewReader(""), &out, &out, nil); err != nil || api.qid != "job-0123456789ab:question:7" {
		t.Fatalf("answer --question: %v %+v", err, api)
	}
}

type queuedFakeAPI struct{ jobAPI }

func (queuedFakeAPI) Close() error { return nil }
func (queuedFakeAPI) Submit(_ context.Context, spec jobs.Spec, _ bool) (jobs.Job, error) {
	return jobs.Job{ID: "job-0123456789ab", Spec: spec, Status: axbackend.Queued}, axbackend.ErrNoCapacity
}

func TestSubmitQueuedExitCode(t *testing.T) {
	clearJobTestEnv(t)
	orig := newAXJobAPI
	t.Cleanup(func() { newAXJobAPI = orig })
	newAXJobAPI = func(context.Context, axbackend.Options) (jobAPI, error) { return queuedFakeAPI{}, nil }
	var stdout, stderr bytes.Buffer
	code := runCLIWithDeps(context.Background(), []string{"--remote", "x", "--ax", "--repo", "https://github.com/erain/glue-sandbox"}, strings.NewReader(""), &stdout, &stderr, fakeFactory(nil), serveDaemon, nil)
	if code != exitQueued || stdout.String() != "job-0123456789ab\n" || !strings.Contains(stderr.String(), "queued: no AX worker has room") || !strings.Contains(stderr.String(), "glue job start job-0123456789ab") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

type interruptFakeAPI struct {
	jobAPI
	said, interrupted string
}

func (f *interruptFakeAPI) Close() error                                { return nil }
func (f *interruptFakeAPI) Say(_ context.Context, _, text string) error { f.said = text; return nil }
func (f *interruptFakeAPI) Interrupt(_ context.Context, _, text string) error {
	f.interrupted = text
	return nil
}

func TestJobSayInterrupt(t *testing.T) {
	clearJobTestEnv(t)
	orig := newAXJobAPI
	t.Cleanup(func() { newAXJobAPI = orig })
	api := &interruptFakeAPI{}
	newAXJobAPI = func(context.Context, axbackend.Options) (jobAPI, error) { return api, nil }
	var out bytes.Buffer
	if err := jobCommand(context.Background(), []string{"say", "--ax", "--interrupt", "job-0123456789ab", "stop", "now"}, strings.NewReader(""), &out, &out, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobCommand(context.Background(), []string{"say", "--ax", "job-0123456789ab", "later"}, strings.NewReader(""), &out, &out, nil); err != nil {
		t.Fatal(err)
	}
	if api.interrupted != "stop now" || api.said != "later" {
		t.Fatalf("interrupt=%q say=%q", api.interrupted, api.said)
	}
}
