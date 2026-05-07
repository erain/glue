// Package git provides git tool factories and shell-out helpers that
// agents can register without re-implementing PATH lookup, timeout
// management, or pathspec construction.
//
// The package is intentionally outside the core glue package so the
// harness stays free of POSIX coupling per ADR 0003. RunGit shells out
// to the system `git` binary; importing this package does not pull in
// a Git library.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultRunTimeout caps a single git invocation. Override per-call via
// RunOptions.Timeout when a long-running operation is expected.
const DefaultRunTimeout = 15 * time.Second

// RunOptions configures RunGit.
type RunOptions struct {
	// WorkDir is the cwd for the git invocation. Required.
	WorkDir string

	// Timeout caps a single invocation. Zero falls back to
	// DefaultRunTimeout.
	Timeout time.Duration
}

// RunGit invokes the system `git` binary in opts.WorkDir with the given
// arguments. Stdout is returned on success; on failure the returned
// error includes the command line and trimmed stderr so callers can
// surface a useful message to the model.
func RunGit(ctx context.Context, opts RunOptions, args ...string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git binary not found in PATH")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(dctx, "git", args...)
	cmd.Dir = opts.WorkDir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}

// BuildPathspec converts include / exclude glob lists into Git pathspec
// arguments. The Git CLI accepts:
//
//   - bare patterns as include filters (e.g. `*.go`, `cmd/...`)
//   - `:(exclude)pattern` as exclude filters
//
// Excludes only take effect after at least one include pattern is
// present, so when callers pass only excludes we add `*` as an explicit
// catch-all include — matching the intuitive "review everything except X"
// semantics.
//
// Returns nil when both lists are empty so callers can branch on
// len(out) > 0 to decide whether to emit a `--` separator.
func BuildPathspec(includes, excludes []string) []string {
	if len(includes) == 0 && len(excludes) == 0 {
		return nil
	}
	out := make([]string, 0, len(includes)+len(excludes))
	for _, p := range includes {
		out = append(out, p)
	}
	if len(out) == 0 && len(excludes) > 0 {
		out = append(out, "*")
	}
	for _, p := range excludes {
		out = append(out, ":(exclude)"+p)
	}
	return out
}
