// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"testing"

	"github.com/erain/glue"
)

func TestYoloDoesNotOverrideHostHandler(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir(), Permission: glue.DenyAll{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job, err := m.Create(ctx, Spec{Objective: "deny", Repo: RepoSpec{Path: "unused"}, Policy: "yolo"})
	if err != nil {
		t.Fatal(err)
	}
	run := &execution{ctx: ctx, cancel: cancel}
	m.active[job.ID] = run
	decision, err := (jobPermission{manager: m, id: job.ID, run: run}).Decide(ctx, glue.PermissionRequest{Tool: "write_file"})
	if err != nil || decision.Allow {
		t.Fatalf("decision %+v err %v", decision, err)
	}
	history, err := m.Permissions(ctx, job.ID, "denied")
	if err != nil || len(history) != 1 {
		t.Fatalf("history %v err %v", history, err)
	}
}
