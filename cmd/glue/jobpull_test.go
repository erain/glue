// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue/jobs"
)

// pullRepoURL never resolves; every clone rewrites it to a local bare repo.
const pullRepoURL = "https://git.example.invalid/erain/glue-sandbox.git"

type pullFakeAPI struct {
	jobAPI
	detail jobs.Detail
	result jobs.Result
	patch  []byte
}

func (f *pullFakeAPI) Show(context.Context, string) (jobs.Detail, error)   { return f.detail, nil }
func (f *pullFakeAPI) Result(context.Context, string) (jobs.Result, error) { return f.result, nil }
func (f *pullFakeAPI) Patch(context.Context, string) ([]byte, error)       { return f.patch, nil }

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// pullFixture returns a user checkout of pullRepoURL (served from a local
// bare repo) and a fake job whose sandbox changed greet.txt on glue/<id>.
func pullFixture(t *testing.T, pushBranch bool) (string, *pullFakeAPI) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", bare)
	rewrite := []string{"clone", "-q", "--config", "url." + bare + ".insteadOf=" + pullRepoURL}
	sandbox := filepath.Join(root, "sandbox")
	gitIn(t, root, append(rewrite, pullRepoURL, sandbox)...)
	gitIn(t, sandbox, "config", "user.email", "t@example.com")
	gitIn(t, sandbox, "config", "user.name", "t")
	must(0, os.WriteFile(filepath.Join(sandbox, "greet.txt"), []byte("hello\n"), 0o600))
	gitIn(t, sandbox, "add", ".")
	gitIn(t, sandbox, "commit", "-qm", "base")
	gitIn(t, sandbox, "push", "-q", "origin", "main")
	base := gitIn(t, sandbox, "rev-parse", "HEAD")
	user := filepath.Join(root, "user")
	gitIn(t, root, append(rewrite, pullRepoURL, user)...)

	id := "job-pull00000001"
	gitIn(t, sandbox, "checkout", "-qb", "glue/"+id)
	must(0, os.WriteFile(filepath.Join(sandbox, "greet.txt"), []byte("HELLO\n"), 0o600))
	gitIn(t, sandbox, "commit", "-qam", "shout")
	head := gitIn(t, sandbox, "rev-parse", "HEAD")
	if pushBranch {
		gitIn(t, sandbox, "push", "-q", "origin", "glue/"+id)
	}
	patch, err := exec.Command("git", "-C", sandbox, "diff", "--binary", base+".."+head).Output()
	if err != nil {
		t.Fatal(err)
	}
	return user, &pullFakeAPI{
		detail: jobs.Detail{Job: jobs.Job{ID: id, Spec: jobs.Spec{Repo: jobs.RepoSpec{URL: pullRepoURL}}}},
		result: jobs.Result{JobID: id, Status: jobs.Achieved, Branch: "glue/" + id, BaseSHA: base, HeadSHA: head, Diffstat: " greet.txt | 2 +-"},
		patch:  patch,
	}
}

func TestJobPullCreatesBranch(t *testing.T) {
	user, api := pullFixture(t, true)
	var out bytes.Buffer
	mustOK := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("pull: %v\n%s", err, out.String())
		}
	}
	mustOK(pullJob(context.Background(), api, api.result.JobID, user, pullOptions{}, &out))
	if got := gitIn(t, user, "rev-parse", api.result.Branch); got != api.result.HeadSHA {
		t.Fatalf("branch at %s, want %s", got, api.result.HeadSHA)
	}
	if cur := gitIn(t, user, "branch", "--show-current"); cur != "main" || !strings.Contains(out.String(), "next: git switch "+api.result.Branch) {
		t.Fatalf("current %q out %q", cur, out.String())
	}
	if err := pullJob(context.Background(), api, api.result.JobID, user, pullOptions{}, &out); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second pull = %v", err)
	}
	mustOK(pullJob(context.Background(), api, api.result.JobID, user, pullOptions{force: true}, &out))
}

func TestJobPullCheckout(t *testing.T) {
	user, api := pullFixture(t, true)
	var out bytes.Buffer
	if err := pullJob(context.Background(), api, api.result.JobID, user, pullOptions{checkout: true}, &out); err != nil {
		t.Fatal(err)
	}
	if cur := gitIn(t, user, "branch", "--show-current"); cur != api.result.Branch || gitIn(t, user, "rev-parse", "HEAD") != api.result.HeadSHA {
		t.Fatalf("on %q", cur)
	}
}

func TestJobPullApply3Way(t *testing.T) {
	user, api := pullFixture(t, true)
	var out bytes.Buffer
	if err := pullJob(context.Background(), api, api.result.JobID, user, pullOptions{apply: true}, &out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(user, "greet.txt"))
	if string(data) != "HELLO\n" || gitIn(t, user, "rev-parse", "HEAD") != api.result.BaseSHA || gitIn(t, user, "branch", "--show-current") != "main" {
		t.Fatalf("apply left greet.txt=%q", data)
	}
}

func TestJobPullApplyRefusesLocalChanges(t *testing.T) {
	user, api := pullFixture(t, true)
	must(0, os.WriteFile(filepath.Join(user, "greet.txt"), []byte("mine\n"), 0o600))
	var out bytes.Buffer
	err := pullJob(context.Background(), api, api.result.JobID, user, pullOptions{apply: true}, &out)
	data, _ := os.ReadFile(filepath.Join(user, "greet.txt"))
	if err == nil || !strings.Contains(err.Error(), "greet.txt") || string(data) != "mine\n" {
		t.Fatalf("err=%v greet.txt=%q", err, data)
	}
}

func TestJobPullRepoMismatch(t *testing.T) {
	user, api := pullFixture(t, true)
	api.detail.Spec.Repo.URL = "https://git.example.invalid/someone/else.git"
	var out bytes.Buffer
	err := pullJob(context.Background(), api, api.result.JobID, user, pullOptions{}, &out)
	if err == nil || !strings.Contains(err.Error(), "someone/else") {
		t.Fatalf("err = %v", err)
	}
	if _, gerr := exec.Command("git", "-C", user, "rev-parse", "--verify", "--quiet", api.result.Branch).Output(); gerr == nil {
		t.Fatal("mismatched pull created a branch")
	}
}

func TestJobPullFallsBackToPatch(t *testing.T) {
	user, api := pullFixture(t, false)
	var out bytes.Buffer
	if err := pullJob(context.Background(), api, api.result.JobID, user, pullOptions{checkout: true}, &out); err != nil {
		t.Fatalf("pull: %v\n%s", err, out.String())
	}
	data, _ := os.ReadFile(filepath.Join(user, "greet.txt"))
	if !strings.Contains(out.String(), "from the job patch") || string(data) != "HELLO\n" || gitIn(t, user, "branch", "--show-current") != api.result.Branch {
		t.Fatalf("out %q greet.txt %q", out.String(), data)
	}
	if wt := gitIn(t, user, "worktree", "list"); strings.Count(wt, "\n") != 0 {
		t.Fatalf("temporary worktree left behind:\n%s", wt)
	}
}

func TestJobPullRefusesCheckedOutBranch(t *testing.T) {
	user, api := pullFixture(t, true)
	var out bytes.Buffer
	if err := pullJob(context.Background(), api, api.result.JobID, user, pullOptions{checkout: true}, &out); err != nil {
		t.Fatal(err)
	}
	must(0, os.WriteFile(filepath.Join(user, "greet.txt"), []byte("my edit\n"), 0o600))
	err := pullJob(context.Background(), api, api.result.JobID, user, pullOptions{force: true}, &out)
	data, _ := os.ReadFile(filepath.Join(user, "greet.txt"))
	if err == nil || !strings.Contains(err.Error(), "checked-out branch") || string(data) != "my edit\n" {
		t.Fatalf("err=%v greet.txt=%q", err, data)
	}
}

func TestJobPullApplyOnSnapshotWork(t *testing.T) {
	user, api := pullFixture(t, true)
	// Uncommitted work that a --snapshot job started from; the job extended it.
	must(0, os.WriteFile(filepath.Join(user, "notes.txt"), []byte("one\ntwo\n"), 0o600))
	api.patch = []byte("diff --git a/notes.txt b/notes.txt\n--- a/notes.txt\n+++ b/notes.txt\n@@ -1,2 +1,3 @@\n one\n two\n+three\n")
	var out bytes.Buffer
	if err := pullJob(context.Background(), api, api.result.JobID, user, pullOptions{apply: true}, &out); err != nil {
		t.Fatalf("apply onto the snapshot's own work: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(user, "notes.txt")); string(data) != "one\ntwo\nthree\n" {
		t.Fatalf("notes.txt = %q", data)
	}
}
