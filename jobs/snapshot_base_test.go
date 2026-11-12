// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"os/exec"
	"testing"
)

func TestSnapshotPRTargetsDeveloperBranch(t *testing.T) {
	repo, _ := repoWithBareOrigin(t, "")
	if out, err := exec.Command("git", "-C", repo, "push", "-q", "origin", "main:feature/retry").CombinedOutput(); err != nil {
		t.Fatalf("push feature branch: %v %s", err, out)
	}
	for _, tc := range []struct{ name, snapshotOf, baseRef, want string }{
		{"developer branch on origin", "feature/retry@abc", "glue/snapshot-job-1", "feature/retry"},
		{"developer branch not on origin", "local-only@abc", "glue/snapshot-job-1", "trunk"},
		{"plain job", "", "release-1", "release-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := Job{Workspace: repo, Spec: Spec{Repo: RepoSpec{BaseRef: tc.baseRef, SnapshotOf: tc.snapshotOf}}}
			if got := prBase(context.Background(), job, "", "trunk"); got != tc.want {
				t.Fatalf("prBase = %q, want %q", got, tc.want)
			}
		})
	}
}
