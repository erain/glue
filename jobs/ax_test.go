// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJobSpecFromAXTaskYAML(t *testing.T) {
	t.Run("valid and unknown fields ignored", func(t *testing.T) {
		raw := []byte("apiVersion: ax.io/v1alpha1\nkind: Task\nfutureTop: true\nmetadata:\n  name: t1\n  futureMeta: 1\nspec:\n  futureSpec: 2\n  workspace:\n    name: ws\n    path: /workspace/repo\n  config:\n    objective: \"fix the bug\"\n    policy: workspace-auto\n    delivery: draft_pr\n    verify:\n      - [\"go\", \"test\", \"./...\"]\n      - \"go vet ./...\"\n    futureConfigField: \"ignored\"\n")
		spec, err := JobSpecFromAXTaskYAML(raw)
		if err != nil {
			t.Fatalf("JobSpecFromAXTaskYAML: %v", err)
		}
		if spec.Objective != "fix the bug" || spec.Repo.Path != "/workspace/repo" || spec.Policy != "workspace-auto" || spec.Delivery != "draft_pr" || len(spec.Verify) != 2 || strings.Join(spec.Verify[1], " ") != "go vet ./..." {
			t.Fatalf("unexpected spec: %+v", spec)
		}
	})
	t.Run("missing config", func(t *testing.T) {
		for _, yml := range []string{
			"apiVersion: ax.io/v1alpha1\nkind: Task\nspec:\n  debug: true\n",
			"apiVersion: ax.io/v1alpha1\nkind: Task\nspec:\n  config: {}\n",
			"apiVersion: ax.io/v1alpha1\nkind: Task\nspec:\n  config:\n    policy: yolo\n",
		} {
			if _, err := JobSpecFromAXTaskYAML([]byte(yml)); err == nil {
				t.Fatalf("expected error for %q", yml)
			}
		}
	})
	t.Run("oversize config", func(t *testing.T) {
		yml := "apiVersion: ax.io/v1alpha1\nkind: Task\nspec:\n  config:\n    objective: " + strings.Repeat("a", MaxAXTaskConfigBytes+1) + "\n"
		if _, err := JobSpecFromAXTaskYAML([]byte(yml)); err == nil || !strings.Contains(err.Error(), "16384") {
			t.Fatalf("expected 16 KiB limit error, got: %v", err)
		}
	})
	t.Run("oversize task yaml", func(t *testing.T) {
		yml := "apiVersion: ax.io/v1alpha1\nkind: Task\nextra: " + strings.Repeat("b", MaxAXTaskYAMLBytes+1) + "\nspec:\n  config:\n    objective: hi\n"
		if _, err := JobSpecFromAXTaskYAML([]byte(yml)); err == nil || !strings.Contains(err.Error(), "31744") {
			t.Fatalf("expected 31 KiB limit error, got: %v", err)
		}
	})
}

func TestJobFromAXIsIdempotent(t *testing.T) {
	state := t.TempDir()
	taskYAML := "apiVersion: ax.io/v1alpha1\nkind: Task\nspec:\n  config:\n    objective: \"idempotent boot\"\n    repo:\n      path: /workspace\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metadata/v1alpha1/ax/task" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(taskYAML))
	}))
	defer srv.Close()
	t.Setenv("AX_METADATA_URL", srv.URL)
	t.Setenv("AX_TASK_YAML", "")
	var firstID string
	for boot := 1; boot <= 2; boot++ {
		m, err := New(Options{StateDir: state})
		if err != nil {
			t.Fatalf("boot %d New: %v", boot, err)
		}
		job, err := m.EnsureJobFromAX(context.Background(), srv.Client())
		list, listErr := m.List(context.Background())
		_ = m.Close()
		if err != nil || listErr != nil || len(list) != 1 || list[0].Status != Created {
			t.Fatalf("boot %d: err=%v listErr=%v jobs=%+v", boot, err, listErr, list)
		}
		if boot == 1 {
			firstID = job.ID
		} else if job.ID != firstID {
			t.Fatalf("boot 2 returned %q, want %q", job.ID, firstID)
		}
	}
}
