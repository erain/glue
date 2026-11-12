// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// snapshotRepoURL never resolves; clones rewrite it to a local bare repo.
const snapshotRepoURL = "https://git.example.invalid/erain/snapshot.git"

// snapshotFixture returns a checkout on my-feature with one unpushed commit,
// a staged edit, an unstaged edit, an untracked file, an ignored file, and
// Glue state, plus the bare origin it pushes to.
func snapshotFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	snapGit(t, root, "init", "-q", "--bare", "-b", "main", bare)
	user := filepath.Join(root, "user")
	snapGit(t, root, "clone", "-q", "--config", "url."+bare+".insteadOf="+snapshotRepoURL, snapshotRepoURL, user)
	snapGit(t, user, "config", "user.email", "t@example.com")
	snapGit(t, user, "config", "user.name", "t")
	write := func(name, content string) {
		t.Helper()
		must(0, os.MkdirAll(filepath.Dir(filepath.Join(user, name)), 0o755))
		must(0, os.WriteFile(filepath.Join(user, name), []byte(content), 0o600))
	}
	write("greet.go", "package sandbox\n")
	write(".gitignore", "*.log\n")
	snapGit(t, user, "add", ".")
	snapGit(t, user, "commit", "-qm", "base")
	snapGit(t, user, "push", "-q", "origin", "main")
	snapGit(t, user, "checkout", "-qb", "my-feature")
	write("feature.go", "package sandbox\n// unpushed\n")
	snapGit(t, user, "add", "feature.go")
	snapGit(t, user, "commit", "-qm", "unpushed work")
	write("greet.go", "package sandbox\n// staged edit\n")
	snapGit(t, user, "add", "greet.go")
	write("greet.go", "package sandbox\n// staged edit\n// unstaged edit\n")
	write("notes/todo.txt", "untracked\n")
	write("debug.log", "ignored\n")
	write(".glue/sessions/s.json", "{}")
	return user, bare
}

func TestSnapshotCommitLeavesIndexAndWorktreeUntouched(t *testing.T) {
	user, bare := snapshotFixture(t)
	ctx := context.Background()
	statusBefore, cachedBefore, branchBefore := snapGit(t, user, "status", "--porcelain"), snapGit(t, user, "diff", "--cached", "--stat"), snapGit(t, user, "branch", "--show-current")
	stashBefore := snapGit(t, user, "stash", "list")
	rs, ref, err := resolveSnapshotRepo(ctx, user, "job-snap00000001", defaultSnapshotMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapGit(t, user, "status", "--porcelain"); got != statusBefore {
		t.Fatalf("status changed:\nbefore %q\nafter  %q", statusBefore, got)
	}
	if snapGit(t, user, "diff", "--cached", "--stat") != cachedBefore || snapGit(t, user, "branch", "--show-current") != branchBefore || snapGit(t, user, "stash", "list") != stashBefore {
		t.Fatal("index, branch, or stash changed")
	}
	if rs.BaseRef != "glue/snapshot-job-snap00000001" || !strings.HasPrefix(rs.SnapshotOf, "my-feature@") || rs.URL != snapshotRepoURL {
		t.Fatalf("repo spec = %+v", rs)
	}
	pushed := snapGit(t, bare, "rev-parse", "refs/heads/"+rs.BaseRef)
	if pushed != ref.sha {
		t.Fatalf("pushed %s, want %s", pushed, ref.sha)
	}
	if got := snapGit(t, bare, "show", ref.sha+":greet.go"); !strings.Contains(got, "unstaged edit") {
		t.Fatalf("snapshot greet.go = %q, want the working-tree version", got)
	}
	if got := snapGit(t, bare, "show", ref.sha+"~1:feature.go"); !strings.Contains(got, "unpushed") {
		t.Fatalf("snapshot parent must be the unpushed HEAD, got %q", got)
	}
	if err := deleteSnapshotBranch(ctx, user, "job-snap00000001"); err != nil {
		t.Fatal(err)
	}
	if out := snapGit(t, bare, "branch", "--list", "glue/*"); out != "" {
		t.Fatalf("snapshot branch left on origin: %q", out)
	}
}

func TestSnapshotIncludesUntrackedRespectsGitignore(t *testing.T) {
	user, bare := snapshotFixture(t)
	_, ref, err := resolveSnapshotRepo(context.Background(), user, "job-snap00000002", defaultSnapshotMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	files := snapGit(t, bare, "ls-tree", "-r", "--name-only", ref.sha)
	for _, want := range []string{"notes/todo.txt", "greet.go", "feature.go", ".gitignore"} {
		if !strings.Contains(files, want) {
			t.Fatalf("snapshot lacks %s:\n%s", want, files)
		}
	}
	for _, unwanted := range []string{"debug.log", ".glue/"} {
		if strings.Contains(files, unwanted) {
			t.Fatalf("snapshot includes %s:\n%s", unwanted, files)
		}
	}
	if ref.files != 2 {
		t.Fatalf("changed files = %d, want 2 (greet.go, notes/todo.txt)", ref.files)
	}
}

func TestSnapshotSizeLimit(t *testing.T) {
	user, bare := snapshotFixture(t)
	_, _, err := resolveSnapshotRepo(context.Background(), user, "job-snap00000003", 10)
	if err == nil || !strings.Contains(err.Error(), "--snapshot-max-size") {
		t.Fatalf("err = %v", err)
	}
	if out := snapGit(t, bare, "branch", "--list", "glue/*"); out != "" {
		t.Fatalf("oversized snapshot was pushed: %q", out)
	}
}

func snapGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
