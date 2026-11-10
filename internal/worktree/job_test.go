package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareLocalWorkspace(t *testing.T) {
	repo := scratchRepo(t)
	if _, err := Git(repo, "branch", "-M", "trunk"); err != nil {
		t.Fatal(err)
	}
	base, err := Git(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"update-ref", "refs/remotes/origin/trunk", base}, {"symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk"}, {"checkout", "-qb", "feature"}, {"commit", "--allow-empty", "-qm", "feature"}} {
		if _, err := Git(repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{"", base} {
		t.Run("base="+ref, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "workspace")
			branch := "glue/job-default"
			if ref != "" {
				branch = "glue/job-explicit"
			}
			result, err := Prepare(context.Background(), Options{Path: repo, Dir: dir, Branch: branch, BaseRef: ref})
			if err != nil {
				t.Fatal(err)
			}
			if result.BaseSHA != base || result.Path != dir {
				t.Fatalf("result %+v", result)
			}
			got, err := Git(dir, "branch", "--show-current")
			if err != nil || got != branch {
				t.Fatalf("branch %s err %v", got, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "only-in-job"), []byte("hello"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(repo, "only-in-job")); !os.IsNotExist(err) {
				t.Fatalf("caller changed: %v", err)
			}
		})
	}
	current, err := Git(repo, "branch", "--show-current")
	if err != nil || current != "feature" {
		t.Fatalf("caller branch %s err %v", current, err)
	}
	detached := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", repo}, {"fetch", "-q", "--depth=1", "origin", "trunk"}, {"checkout", "-qf", "FETCH_HEAD"}} {
		if _, err := Git(detached, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{"", "trunk"} {
		res, err := Prepare(context.Background(), Options{Path: detached, Dir: filepath.Join(t.TempDir(), "ax-"+ref), Branch: "glue/ax-" + ref, BaseRef: ref})
		if err != nil || res.BaseSHA != base {
			t.Fatalf("detached ref=%q: res=%+v err=%v", ref, res, err)
		}
	}
}
func TestPrepareURLClone(t *testing.T) {
	repo := scratchRepo(t)
	for _, args := range [][]string{{"branch", "-M", "trunk"}, {"checkout", "-qb", "feature"}, {"commit", "--allow-empty", "-qm", "feature"}, {"checkout", "-q", "trunk"}} {
		if _, err := Git(repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	bare := filepath.Join(t.TempDir(), "remote.git")
	if _, err := Git(repo, "clone", "--bare", repo, bare); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"", "feature"} {
		t.Run("ref="+ref, func(t *testing.T) {
			wantRef := ref
			if wantRef == "" {
				wantRef = "trunk"
			}
			sha, err := Git(repo, "rev-parse", wantRef)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "nested", "clone")
			result, err := Prepare(context.Background(), Options{URL: bare, Dir: dir, Branch: "glue/job-clone", BaseRef: ref})
			if err != nil {
				t.Fatal(err)
			}
			if result.BaseSHA != sha {
				t.Fatalf("result %+v want %s", result, sha)
			}
			branch, err := Git(dir, "branch", "--show-current")
			if err != nil || branch != "glue/job-clone" {
				t.Fatalf("branch %s err %v", branch, err)
			}
		})
	}
}
func TestPrepareInvalidOptions(t *testing.T) {
	repo := scratchRepo(t)
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"missing", Options{}},
		{"both sources", Options{Path: repo, URL: repo, Dir: filepath.Join(t.TempDir(), "job"), Branch: "glue/job"}},
		{"invalid branch", Options{Path: repo, Dir: filepath.Join(t.TempDir(), "job"), Branch: "--bad"}},
		{"missing revision", Options{Path: repo, Dir: filepath.Join(t.TempDir(), "job"), Branch: "glue/job", BaseRef: "missing"}},
		{"existing destination", Options{Path: repo, Dir: t.TempDir(), Branch: "glue/job"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Prepare(context.Background(), tc.opts); err == nil {
				t.Fatalf("accepted %+v", tc.opts)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prepare(ctx, Options{Path: repo, Dir: filepath.Join(t.TempDir(), "job"), Branch: "glue/job"}); err == nil {
		t.Fatal("ignored cancellation")
	}
}
