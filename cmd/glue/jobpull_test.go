// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/jobs"
	filestore "github.com/erain/glue/stores/file"
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

func setupTeleportClones(t *testing.T, p glue.Provider) (string, string, *jobs.Manager) {
	t.Helper()
	clearJobTestEnv(t)
	bare, src, dst := filepath.Join(t.TempDir(), "origin.git"), serveJobRepo(t), filepath.Join(t.TempDir(), "user")
	runGit(t, t.TempDir(), "init", "--bare", "-qb", "main", bare)
	runGit(t, src, "remote", "add", "origin", bare)
	runGit(t, src, "push", "-q", "origin", "main:main")
	gitIn(t, t.TempDir(), "clone", "-q", bare, dst)
	ts, mgr := newTestJobDaemon(t, p)
	t.Setenv("GLUE_SERVER", ts.URL)
	t.Setenv("GLUE_TOKEN", "job-token")
	return src, dst, mgr
}

func TestJobTeleportCleanCheckoutAndSessionBriefing(t *testing.T) {
	src, dst, mgr := setupTeleportClones(t, &scriptedProvider{turns: [][]glue.ProviderEvent{
		textTurn(`{"items":[{"title":"add shout.txt"}]}`), toolCallTurn("q1", "ask_user", `{"question":"Use uppercase?","options":["yes","no"]}`),
		toolCallTurn("w1", "write_file", `{"path":"shout.txt","content":"SHOUT\n"}`), textTurn("wrote shout.txt"),
		textTurn(`{"done":true,"items":[{"title":"add shout.txt","done":true,"evidence":"shout.txt verified"}],"summary":"Shipped shout.txt"}`),
	}})
	t.Setenv("GLUE_PUBLIC_URL", "https://glue.example.com")
	stdout, stderr, code := runJobCLI(t, "submit", "--repo", src, "--deliver", "branch", "--verify", "sh -c 'echo VERIFY_PASS_MARKER'", "Add shout.txt greeting")
	id := strings.TrimSpace(stdout)
	if code != 0 || stdout != id+"\n" || !strings.Contains(stderr, "browser: https://glue.example.com/#/jobs/"+id) {
		t.Fatalf("submit code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
		if d, err := mgr.Inspect(context.Background(), id); err == nil && d.Question != nil {
			break
		}
	}
	mustJobCLI(t, "answer", id, "yes, use SHOUT")
	waitJob(t, mgr, id, jobs.Achieved)
	out, sid := mustJobCLI(t, "teleport", "--no-tui", "--repo", dst), teleportSessionID(id)
	if cur := gitIn(t, dst, "branch", "--show-current"); cur != "glue/"+id || string(must(os.ReadFile(filepath.Join(dst, "shout.txt")))) != "SHOUT\n" || !strings.Contains(out, "session: "+sid) || !strings.Contains(out, "next: glue code --id "+sid) {
		t.Fatalf("cur=%q out=%q", cur, out)
	}
	localProv := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("ready to refine")}}
	if code := runCLIWithDeps(context.Background(), []string{"code", "--work", dst, "--store", filepath.Join(dst, ".glue", "sessions"), "--id", sid, "--prompt", "Refine edge case"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, fakeFactory(localProv), serveDaemon, nil); code != 0 {
		t.Fatalf("glue code exit=%d", code)
	}
	msgs := localProv.requests[0].Messages
	wants := []string{"Add shout.txt greeting", "[x] add shout.txt — shout.txt verified", "Use uppercase?", "yes, use SHOUT", "shout.txt", "VERIFY_PASS_MARKER", "Shipped shout.txt"}
	if len(msgs) < 3 || msgs[0].Role != glue.MessageRoleUser || msgs[1].Role != glue.MessageRoleAssistant || slices.ContainsFunc(wants, func(w string) bool { return !strings.Contains(fmt.Sprint(msgs), w) }) {
		t.Fatalf("unexpected provider messages:\n%+v", msgs)
	}
}

func TestJobTeleportSnapshotAutoAppliesToDirtyTree(t *testing.T) {
	user, api := pullFixture(t, true)
	api.detail.Spec.Objective, api.detail.Spec.Repo.SnapshotOf, api.result.Diffstat = "Extend notes.txt from snapshot", "main@"+api.result.BaseSHA, " notes.txt | 1 +"
	api.patch = []byte("diff --git a/notes.txt b/notes.txt\n--- a/notes.txt\n+++ b/notes.txt\n@@ -1,2 +1,3 @@\n one\n two\n+three\n")
	must(0, os.WriteFile(filepath.Join(user, "notes.txt"), []byte("one\ntwo\n"), 0o600))
	must(0, os.WriteFile(filepath.Join(user, "unrelated.txt"), []byte("keep my local wip\n"), 0o600))
	var out bytes.Buffer
	sid, sdir, err := teleportJob(context.Background(), api, api.result.JobID, user, "", pullOptions{}, &out)
	if err != nil || gitIn(t, user, "branch", "--show-current") != "main" || string(must(os.ReadFile(filepath.Join(user, "notes.txt")))) != "one\ntwo\nthree\n" || string(must(os.ReadFile(filepath.Join(user, "unrelated.txt")))) != "keep my local wip\n" {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
	if st, ok, err := filestore.New(sdir).Load(context.Background(), sid); err != nil || !ok || !strings.Contains(fmt.Sprint(st.Messages), "snapshot_of: main@") {
		t.Fatalf("stored session ok=%v err=%v", ok, err)
	}
	api.detail.Spec.Repo.SnapshotOf = ""
	if _, _, err := teleportJob(context.Background(), api, api.result.JobID, user, "", pullOptions{}, &out); err == nil || !strings.Contains(err.Error(), "--apply") {
		t.Fatalf("expected dirty-tree refusal, got %v", err)
	}
}

func TestJobTeleportBlockedNoChangesStillHydratesContext(t *testing.T) {
	stuck := textTurn(`{"items":[{"title":"wire v2 endpoint","done":false}],"summary":"Blocked on missing /v2/schema"}`)
	src, dst, mgr := setupTeleportClones(t, &scriptedProvider{turns: [][]glue.ProviderEvent{
		textTurn(`{"items":[{"title":"wire v2 endpoint"}]}`), textTurn("missing"), stuck, textTurn("missing"), stuck, textTurn("missing"), stuck,
	}})
	id := strings.TrimSpace(mustJobCLI(t, "submit", "--repo", src, "--start=false", "Wire /v2/schema client"))
	mustJobCLI(t, "say", id, "check internal docs first")
	mustJobCLI(t, "start", id)
	waitJob(t, mgr, id, jobs.Blocked)
	if out := mustJobCLI(t, "teleport", id, "--no-tui", "--repo", dst); !strings.Contains(out, "made no file changes; hydrating session context only") || gitIn(t, dst, "branch", "--show-current") != "main" {
		t.Fatalf("out=%q", out)
	}
	st, ok, err := filestore.New(filepath.Join(dst, ".glue", "sessions")).Load(context.Background(), teleportSessionID(id))
	wants := []string{"status: blocked", "Wire /v2/schema client", "Blocked on missing /v2/schema", "say: check internal docs first"}
	if err != nil || !ok || len(st.Messages) != 2 || slices.ContainsFunc(wants, func(w string) bool { return !strings.Contains(fmt.Sprint(st.Messages), w) }) {
		t.Fatalf("ok=%v err=%v msgs=%+v", ok, err, st.Messages)
	}
}
