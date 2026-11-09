package jobs

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestJobInspection(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	ctx := context.Background()
	job, err := m.Create(ctx, Spec{Objective: "inspect", Repo: RepoSpec{Path: "unused"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.record(job.ID, "job_started", nil, "", nil, Running); err != nil {
		t.Fatal(err)
	}
	goal := glue.GoalEvent{Type: glue.GoalEventVerdict, Iteration: 2, Checklist: []glue.ChecklistItem{{Title: "A", Done: true}}, Usage: glue.Usage{TotalTokens: 42}}
	if err := m.record(job.ID, "verdict", goal, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.record(job.ID, "permission_request", map[string]any{"permission_id": "p", "request": glue.PermissionRequest{Tool: "write_file"}}, "", nil, WaitingForInput); err != nil {
		t.Fatal(err)
	}
	detail, err := m.Inspect(ctx, job.ID)
	if err != nil || detail.Iteration != 2 || len(detail.Checklist) != 1 || !detail.Checklist[0].Done || detail.Usage.TotalTokens != 42 || detail.PendingPermissionCount != 1 {
		t.Fatalf("detail %+v err %v", detail, err)
	}
	pending, err := m.Permissions(ctx, job.ID, "pending")
	if err != nil || len(pending) != 1 || pending[0].Request.Tool != "write_file" {
		t.Fatalf("permissions %v err %v", pending, err)
	}
	if err := m.record(job.ID, "permission_decision", map[string]any{"permission_id": "p", "decision": glue.PermissionDecision{Allow: true}}, "", nil, Running); err != nil {
		t.Fatal(err)
	}
	detail, err = m.Inspect(ctx, job.ID)
	if err != nil || detail.PendingPermissionCount != 0 {
		t.Fatalf("detail %+v err %v", detail, err)
	}
	allowed, err := m.Permissions(ctx, job.ID, "allowed")
	if err != nil || len(allowed) != 1 || allowed[0].ID != "p" {
		t.Fatalf("permissions %v err %v", allowed, err)
	}
	if err := m.record(job.ID, "permission_request", map[string]any{"permission_id": "orphan", "request": glue.PermissionRequest{Tool: "write_file"}}, "", nil, WaitingForInput); err != nil {
		t.Fatal(err)
	}
	if err := m.record(job.ID, "job_done", map[string]any{"result": glue.GoalResult{Iterations: 3, Usage: glue.Usage{TotalTokens: 99}}}, "", nil, Errored); err != nil {
		t.Fatal(err)
	}
	detail, err = m.Inspect(ctx, job.ID)
	if err != nil || detail.Iteration != 3 || detail.Usage.TotalTokens != 99 || detail.PendingPermissionCount != 0 {
		t.Fatalf("terminal %+v err %v", detail, err)
	}
	expired, err := m.Permissions(ctx, job.ID, "expired")
	if err != nil || len(expired) != 1 {
		t.Fatalf("expired %v err %v", expired, err)
	}
	if _, err := m.Inspect(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := m.Permissions(ctx, job.ID, "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("state: %v", err)
	}
}
func TestJobQueryAndCreatedCancel(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	ctx := context.Background()
	a, err := m.Create(ctx, Spec{Objective: "A", Repo: RepoSpec{Path: "unused"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(ctx, Spec{Objective: "B", Repo: RepoSpec{Path: "unused"}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Query(ctx, Filter{Status: Created, Limit: 1})
	if err != nil || len(first) != 1 || first[0].ID != b.ID {
		t.Fatalf("first %v err %v", first, err)
	}
	page, err := m.Query(ctx, Filter{Before: b.ID, Limit: 1})
	if err != nil || len(page) != 1 || page[0].ID != a.ID {
		t.Fatalf("page %v err %v", page, err)
	}
	if err := m.Cancel(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	canceled, err := m.Query(ctx, Filter{Status: Canceled})
	if err != nil || len(canceled) != 1 || canceled[0].ID != a.ID {
		t.Fatalf("canceled %v err %v", canceled, err)
	}
	events, err := m.Events(ctx, a.ID, 0)
	if err != nil || events[len(events)-1].Type != "job_done" {
		t.Fatalf("events %v err %v", events, err)
	}
	if _, err := m.store.DB.Exec("UPDATE jobs SET created_at='2026-01-01T00:00:00.000000000Z'"); err != nil {
		t.Fatal(err)
	}
	newer, older := a.ID, b.ID
	if newer < older {
		newer, older = older, newer
	}
	tied, err := m.Query(ctx, Filter{Limit: 1})
	if err != nil || len(tied) != 1 || tied[0].ID != newer {
		t.Fatalf("tied %v err %v", tied, err)
	}
	next, err := m.Query(ctx, Filter{Before: newer, Limit: 1})
	if err != nil || len(next) != 1 || next[0].ID != older {
		t.Fatalf("next %v err %v", next, err)
	}
	for _, f := range []Filter{{Limit: -1}, {Limit: 1001}, {Status: "invalid"}, {Before: "missing"}} {
		if _, err := m.Query(ctx, f); !errors.Is(err, ErrInvalid) {
			t.Fatalf("filter %+v: %v", f, err)
		}
	}
}
func TestJobValidationNamesFields(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	for _, field := range []string{"objective", "repo", "budgets.token_budget", "thinking"} {
		s := Spec{Objective: "A", Repo: RepoSpec{Path: "unused"}}
		switch field {
		case "objective":
			s.Objective = ""
		case "repo":
			s.Repo = RepoSpec{}
		case "budgets.token_budget":
			s.Budgets.TokenBudget = -1
		case "thinking":
			s.Thinking = "bad"
		}
		if _, err := m.Create(context.Background(), s); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), field) {
			t.Fatalf("%s: %v", field, err)
		}
	}
}
