// Environment context assembly for coding runs: a small tagged block
// describing where the agent is running (cwd, OS, date, git state) so
// the model does not spend early turns rediscovering it with tools.

package coding

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	toolsgit "github.com/erain/glue/tools/git"
)

// envGitTimeout caps each git invocation used to assemble the block; a
// slow or hung git must never stall the prompt path.
const envGitTimeout = 2 * time.Second

// envHeadSubjectMax bounds the head-commit subject carried in the block.
const envHeadSubjectMax = 72

// EnvContext returns a generator for the tagged environment block that
// accompanies coding prompts (see glue.AgentOptions.EnvContext). The
// generator re-reads the environment on every call so the harness can
// detect changes — branch switch, new commits, date rollover — and
// refresh the block mid-session instead of leaving the model with a
// stale picture.
func EnvContext(workDir string) func() string {
	return func() string { return envContextBlock(workDir) }
}

func envContextBlock(workDir string) string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		abs = workDir
	}
	var b strings.Builder
	b.WriteString("<environment_context>\n")
	fmt.Fprintf(&b, "cwd: %s\n", abs)
	fmt.Fprintf(&b, "os: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "date: %s\n", time.Now().Format("2006-01-02"))
	fmt.Fprintf(&b, "git: %s\n", gitSummary(abs))
	b.WriteString("</environment_context>")
	return b.String()
}

// gitSummary describes the repository in one line: branch, dirty count,
// and head commit. It degrades to "not a repository" outside a work
// tree and swallows individual command failures — assembling the block
// must never fail a prompt.
func gitSummary(dir string) string {
	ctx := context.Background()
	opts := toolsgit.RunOptions{WorkDir: dir, Timeout: envGitTimeout}
	inside, err := toolsgit.RunGit(ctx, opts, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		return "not a repository"
	}

	parts := make([]string, 0, 3)
	if branch, err := toolsgit.RunGit(ctx, opts, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		parts = append(parts, "branch "+strings.TrimSpace(branch))
	}
	if status, err := toolsgit.RunGit(ctx, opts, "status", "--porcelain"); err == nil {
		if dirty := len(splitNonEmptyLines(status)); dirty == 0 {
			parts = append(parts, "clean")
		} else {
			parts = append(parts, fmt.Sprintf("%d dirty file(s)", dirty))
		}
	}
	if head, err := toolsgit.RunGit(ctx, opts, "log", "-1", "--format=%h %s"); err == nil {
		subject := strings.TrimSpace(head)
		if len(subject) > envHeadSubjectMax {
			subject = subject[:envHeadSubjectMax] + "…"
		}
		parts = append(parts, "head "+subject)
	}
	if len(parts) == 0 {
		return "repository state unavailable"
	}
	return strings.Join(parts, " · ")
}

func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
