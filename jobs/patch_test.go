// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
)

// thenBlockProvider plays scripted turns, then blocks until canceled.
type thenBlockProvider struct {
	script  *scriptProvider
	blocked chan struct{}
}

func (p *thenBlockProvider) Stream(ctx context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.script.mu.Lock()
	left := len(p.script.turns)
	p.script.mu.Unlock()
	if left > 0 {
		return p.script.Stream(ctx, req)
	}
	select {
	case <-p.blocked:
	default:
		close(p.blocked)
	}
	ch := make(chan glue.ProviderEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

func TestCanceledJobPushesPartialBranch(t *testing.T) {
	repo, bare := repoWithBareOrigin(t, "")
	p := &thenBlockProvider{blocked: make(chan struct{}), script: &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"edit file"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: []byte(`{"path":"work.txt","content":"half done\n"}`)}}, {Type: glue.ProviderEventDone}},
	}}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "edit file", Repo: RepoSpec{Path: repo, BaseRef: "main"}, Delivery: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("job never reached the blocking turn")
	}
	if err := m.Cancel(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Canceled {
		t.Fatalf("status = %s", got.Status)
	}
	res, err := m.Result(context.Background(), job.ID)
	if err != nil || !res.Partial || res.HeadSHA == res.BaseSHA || res.DeliveryError != "" || res.PRURL != "" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	pushed, err := exec.Command("git", "-C", bare, "rev-parse", "refs/heads/"+res.Branch).CombinedOutput()
	if err != nil || strings.TrimSpace(string(pushed)) != res.HeadSHA {
		t.Fatalf("pushed %q err %v, want %s", pushed, err, res.HeadSHA)
	}
	patch, err := m.Patch(context.Background(), job.ID)
	if err != nil || !strings.Contains(string(patch), "+half done") || !strings.Contains(string(patch), "work.txt") {
		t.Fatalf("patch = %q, %v", patch, err)
	}
}

func TestPatchUnfinishedAndEmpty(t *testing.T) {
	p := &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"look"}]}`),
		turn("nothing to change"),
		turn(`{"done":true,"items":[{"title":"look","done":true,"evidence":"ok"}],"summary":"No changes needed"}`),
	}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "look", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Patch(context.Background(), job.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("patch before finish = %v, want ErrConflict", err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	waitJob(t, m, job.ID)
	if patch, err := m.Patch(context.Background(), job.ID); err != nil || len(patch) != 0 {
		t.Fatalf("patch = %q, %v; want empty", patch, err)
	}
}

func TestPatchEndpointWhileRunning(t *testing.T) {
	p := &thenBlockProvider{blocked: make(chan struct{}), script: &scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"edit"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: []byte(`{"path":"live.txt","content":"live while running\n"}`)}}, {Type: glue.ProviderEventDone}},
	}}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "edit", Repo: RepoSpec{Path: repoFixture(t)}, Policy: "workspace-auto"})
	if err != nil || m.Start(context.Background(), job.ID) != nil {
		t.Fatalf("create/start: %v", err)
	}
	select {
	case <-p.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("job did not reach blocking turn")
	}
	d, err := m.Inspect(context.Background(), job.ID)
	if err != nil || d.Status != Running {
		t.Fatalf("inspect = %+v, %v", d, err)
	}
	if patch, err := m.Patch(context.Background(), job.ID); err != nil || !strings.Contains(string(patch), "+live while running") || !strings.Contains(string(patch), "live.txt") {
		t.Fatalf("running patch = %q, %v", patch, err)
	}
	if cached, err := exec.Command("git", "-C", d.Workspace, "diff", "--cached").CombinedOutput(); err != nil || len(cached) != 0 {
		t.Fatalf("git index mutated: %q, %v", cached, err)
	}
	p.script.mu.Lock()
	p.script.turns = [][]glue.ProviderEvent{{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "s1", Name: "shell_exec", Arguments: []byte(`{"argv":["git","push","origin","main"]}`)}}, {Type: glue.ProviderEventDone}}}
	p.script.mu.Unlock()
	if err := m.Interrupt(context.Background(), job.ID, ""); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if d, _ = m.Inspect(context.Background(), job.ID); d.Status == WaitingForInput {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("status = %s, want waiting_for_input", d.Status)
		}
	}
	if patch, err := m.Patch(context.Background(), job.ID); err != nil || !strings.Contains(string(patch), "+live while running") {
		t.Fatalf("waiting patch = %q, %v", patch, err)
	}
}
