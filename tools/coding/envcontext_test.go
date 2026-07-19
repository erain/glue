package coding

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnvContextBlock_NonRepo(t *testing.T) {
	dir := t.TempDir()
	block := EnvContext(dir)()
	if !strings.HasPrefix(block, "<environment_context>\n") || !strings.HasSuffix(block, "</environment_context>") {
		t.Fatalf("block not tagged: %q", block)
	}
	abs, _ := filepath.Abs(dir)
	for _, want := range []string{"cwd: " + abs, "os: ", "date: " + time.Now().Format("2006-01-02"), "git: not a repository"} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
}

func TestEnvContextBlock_GitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	mustRun(t, dir, "git", "init", "-q", "-b", "main")
	mustRun(t, dir, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "initial commit")

	gen := EnvContext(dir)
	block := gen()
	for _, want := range []string{"branch main", "clean", "head "} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}

	// Dirty the tree; the generator must observe the change.
	mustWrite(t, filepath.Join(dir, "new.txt"), "x")
	block = gen()
	if !strings.Contains(block, "1 dirty file(s)") {
		t.Errorf("block missing dirty count:\n%s", block)
	}
}

func mustWrite(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v (%s)", name, args, err, out)
	}
}
