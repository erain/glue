package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fixture defines one structural-replay scenario: a tiny synthetic Git
// repo seeded with a baseline commit and one or more change commits,
// plus assertions that read the agent's review output and verify
// invariants the prompt is supposed to enforce. We assert structure
// rather than exact wording — LLM output is non-deterministic.
type fixture struct {
	name string
	// seed populates the repo on the `feature` branch. The scratch
	// repo already has an empty `main` baseline commit; seed runs
	// after `git checkout -b feature`.
	seed func(t *testing.T, repo string)
	// expect runs the structural assertions over the agent's stdout
	// (the full review markdown). The fixture passes the test only if
	// every assertion holds.
	expect func(t *testing.T, review string)
}

// fixtureReplayAttempts allows the non-deterministic OpenRouter free
// router two retries when an upstream model returns a provider-quality
// failure or stalls instead of a review. Each attempt has runAgentInRepo's
// 180-second timeout, so the worst case stays bounded for CI.
const fixtureReplayAttempts = 3

// fixtures is the set of scenarios we replay against a real free model
// to catch prompt regressions. Add a new entry when a new prompt
// behavior needs to be locked in.
var fixtures = []fixture{
	{
		name: "panic-stub",
		seed: func(t *testing.T, repo string) {
			// Classic regression-bait: a stub that panics on startup.
			// Prompt should produce at least one [major] or [critical]
			// issue tied to main.go.
			writeFile(t, repo, "main.go", "package main\n\nfunc main() { panic(\"todo\") }\n")
			gitCommit(t, repo, "scaffold", "main.go")
		},
		expect: func(t *testing.T, review string) {
			// The fixture is a smoke for output-shape regressions, not a
			// precision test for the model's judgement. The model may
			// legitimately read `func main() { panic("todo") }` as an
			// intentional scaffold placeholder and emit Variant B (LGTM),
			// or call it `medium` instead of `critical` ("intentional
			// stub" vs "ships broken"). Both are defensible from a senior
			// reviewer. What we want to catch is: silence, fabricated
			// paths, or output that isn't in the canonical shape.
			assertGlueReviewHeader(t, review)
			isLGTM := reviewLooksLGTM(review)
			mentionsMain := strings.Contains(review, "main.go")
			if !isLGTM && !mentionsMain {
				t.Errorf("review neither said LGTM nor mentioned main.go; review=%q", review)
			}
			if !isLGTM {
				assertHasFixBlock(t, review)
			}
		},
	},
	{
		name: "subtle-bug",
		seed: func(t *testing.T, repo string) {
			// Off-by-one: the loop runs n+1 times instead of n. A senior
			// reviewer catches this; we assert the section names line
			// up but don't demand the model spotted the specific bug
			// (model quality varies).
			body := "package main\n\n" +
				"// SumFirstN returns the sum of integers 1..n inclusive.\n" +
				"func SumFirstN(n int) int {\n" +
				"\ttotal := 0\n" +
				"\tfor i := 0; i <= n; i++ { // bug: should be i < n+1 or i := 1; i <= n\n" +
				"\t\ttotal += i\n" +
				"\t}\n" +
				"\treturn total\n" +
				"}\n"
			writeFile(t, repo, "math.go", body)
			gitCommit(t, repo, "add SumFirstN", "math.go")
		},
		expect: func(t *testing.T, review string) {
			// Don't gate on "the model spotted this specific bug" —
			// model quality varies. Do gate on output structure being
			// well-formed for a non-trivial diff.
			assertGlueReviewHeader(t, review)
			if !strings.Contains(review, "math.go") {
				t.Errorf("expected review to mention math.go (the only changed file); review=%q", review)
			}
		},
	},
	{
		name: "cosmetic-only",
		seed: func(t *testing.T, repo string) {
			// Pure formatting / comment tweak. Prompt should not pad
			// Issues with phantom problems on a no-op-equivalent diff.
			writeFile(t, repo, "doc.go", "// Package x prints stuff.\npackage x\n")
			gitCommit(t, repo, "polish: fix package comment trailing period", "doc.go")
		},
		expect: func(t *testing.T, review string) {
			assertGlueReviewHeader(t, review)
			// Permissive: the model may legitimately have nothing
			// substantive to say. We only fail if it fabricated a
			// reference to a file outside the diff.
			if strings.Contains(review, ".bash_history") || strings.Contains(review, "/etc/passwd") {
				t.Errorf("review references suspicious file outside diff; review=%q", review)
			}
		},
	},
}

// TestFixtureReplay drives every fixture through the agent against a
// real free model and asserts each scenario's structural invariants.
// Skipped quietly when no provider key is in env (matches the existing
// TestLiveReviewSmoke gating convention). The OpenRouter free router is
// intentionally non-deterministic, so known upstream-quality faults
// (empty/preamble-only responses, malformed tool-call JSON, or stalled
// upstream responses) get two retries before the last attempt is judged
// normally.
func TestFixtureReplay(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	provider, err := pickLiveProvider(t)
	if err != nil {
		t.Skipf("no provider key in env: %v", err)
	}

	for _, f := range fixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			repo := t.TempDir()
			gitInit(t, repo)
			f.seed(t, repo)
			review, err := runFixtureReplay(t, repo, provider, f.name)
			if err != nil {
				// Free upstreams (e.g. inclusionai/ling-2.6-1t:free)
				// share a 20 req/min quota and 429 frequently. Treat
				// upstream rate limits as a skip so transient quota
				// trips don't fail CI; other failures still fail loudly.
				if isUpstreamRateLimit(err) {
					t.Skipf("upstream rate-limited (free tier): %v", err)
				}
				t.Fatalf("run failed: %v", err)
			}
			t.Logf("%s review (%d bytes):\n%s", f.name, len(review), review)
			f.expect(t, review)
		})
	}
}

func TestFixtureReplayRetryClassifiers(t *testing.T) {
	tests := []struct {
		name   string
		review string
		want   bool
	}{
		{name: "empty", review: "", want: true},
		{name: "short noise", review: "ok", want: true},
		{name: "preamble only", review: "I'll review the current Git branch for issues and report findings.", want: true},
		{name: "canonical review", review: "## glue-review\n\nNo concerns — LGTM", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableFixtureReplayReview(tt.review); got != tt.want {
				t.Fatalf("isRetryableFixtureReplayReview(%q)=%v, want %v", tt.review, got, tt.want)
			}
		})
	}

	if !isRetryableFixtureReplayError(fmt.Errorf("rc=1 stderr=openrouter: tool call %q invalid JSON arguments", "git_diff_branch")) {
		t.Fatal("invalid JSON tool-call error should be retryable")
	}
	if !isRetryableFixtureReplayError(fmt.Errorf("rc=1 stderr=[failover] openrouter failed: context deadline exceeded")) {
		t.Fatal("context deadline from live upstream should be retryable")
	}
	if !isUpstreamRateLimit(fmt.Errorf("openrouter http 429: Rate limit exceeded")) {
		t.Fatal("429 rate limit should be classified as upstream rate limit")
	}
}

func TestFixtureReplayReviewMatchers(t *testing.T) {
	for _, review := range []string{
		"## glue-review\n\nNo concerns — LGTM",
		"##glue-review\n\nNo concerns - LGTM.",
		"## glue-reviewlow\n\nNoconcerns — LGTM.",
	} {
		if !glueReviewHeaderPattern.MatchString(review) {
			t.Fatalf("header pattern did not match %q", review)
		}
		if !reviewLooksLGTM(review) {
			t.Fatalf("LGTM pattern did not match %q", review)
		}
	}
}

func TestFixtureReplayFixBlockMatcher(t *testing.T) {
	for _, review := range []string{
		"```markdown\nFix the following in this PR before merging.\n```",
		"```\nmarkdown\nFix the following in this PR before merging.\n```",
	} {
		if !fixBlockPattern.MatchString(review) {
			t.Fatalf("fix block pattern did not match %q", review)
		}
	}
	if fixBlockPattern.MatchString("```go\nfmt.Println(\"not a fix block\")\n```") {
		t.Fatal("fix block pattern matched unrelated code fence")
	}
}

func runFixtureReplay(t *testing.T, repo, provider, fixtureName string) (string, error) {
	t.Helper()
	for attempt := 1; attempt <= fixtureReplayAttempts; attempt++ {
		sessionID := fmt.Sprintf("fixture-%s-%d-attempt-%d", fixtureName, time.Now().UnixNano(), attempt)
		review, err := runAgentInRepo(t, repo, provider, sessionID)
		if err != nil {
			if isUpstreamRateLimit(err) {
				return "", err
			}
			if attempt < fixtureReplayAttempts && isRetryableFixtureReplayError(err) {
				t.Logf("%s attempt %d/%d hit retryable upstream error: %v", fixtureName, attempt, fixtureReplayAttempts, err)
				continue
			}
			return "", err
		}
		if attempt < fixtureReplayAttempts && isRetryableFixtureReplayReview(review) {
			t.Logf("%s attempt %d/%d produced implausible review (%d bytes); retrying", fixtureName, attempt, fixtureReplayAttempts, len(review))
			continue
		}
		return review, nil
	}
	return "", fmt.Errorf("fixture replay exhausted without a terminal attempt")
}

func isRetryableFixtureReplayError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "invalid JSON arguments") ||
		(strings.Contains(msg, "tool call") && strings.Contains(msg, "invalid JSON")) ||
		strings.Contains(msg, "context deadline exceeded")
}

func isRetryableFixtureReplayReview(review string) bool {
	trimmed := strings.TrimSpace(review)
	if trimmed == "" {
		return true
	}
	if glueReviewHeaderPattern.MatchString(trimmed) {
		return false
	}
	if len(trimmed) < 50 {
		return true
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "i'll review the current git branch") {
		return true
	}
	return len(trimmed) < 160 && !fixBlockPattern.MatchString(trimmed)
}

func isUpstreamRateLimit(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "http 429") || strings.Contains(msg, "Rate limit exceeded")
}

// helpers --------------------------------------------------------------

func gitInit(t *testing.T, repo string) {
	t.Helper()
	for _, c := range [][]string{
		{"init", "-q", "-b", "main"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
		{"checkout", "-q", "-b", "feature"},
	} {
		runHermeticGit(t, repo, c...)
	}
}

func gitCommit(t *testing.T, repo, message string, files ...string) {
	t.Helper()
	args := append([]string{"add"}, files...)
	runHermeticGit(t, repo, args...)
	runHermeticGit(t, repo, "commit", "-q", "-m", message)
}

func runHermeticGit(t *testing.T, repo string, gitArgs ...string) {
	t.Helper()
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v (%s)", gitArgs, err, errBuf.String())
	}
}

func writeFile(t *testing.T, repo, name, body string) {
	t.Helper()
	full := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// pickLiveProvider returns the first provider name whose API key is
// configured in env, in failover order.
func pickLiveProvider(t *testing.T) (string, error) {
	t.Helper()
	for _, p := range []string{"openrouter", "nvidia", "gemini"} {
		if providerKeyAvailable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("none of OPENROUTER_API_KEY / NVIDIA_API_KEY / GEMINI_API_KEY set")
}

// runAgentInRepo invokes run() against a real free model. Returns the
// captured stdout (the review markdown).
func runAgentInRepo(t *testing.T, repo, provider, sessionID string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	args := []string{
		"--provider", provider,
		"--work", repo,
		"--base", "main",
		"--store", filepath.Join(repo, ".glue"),
		"--id", sessionID,
		"--max-turns", "8",
	}
	// OpenRouter resolves to openrouter/free (the meta-router) via
	// the provider default — robust to free-tier churn. NVIDIA gets
	// pinned to llama-3.3-70b for fixture-test perf (its registry
	// default kimi-k2.6 is slower than the fixture call needs).
	switch provider {
	case "nvidia":
		args = append(args, "--model", "meta/llama-3.3-70b-instruct")
	}

	var out, errBuf bytes.Buffer
	rc := run(ctx, args, &out, &errBuf)
	if rc != 0 {
		return "", fmt.Errorf("rc=%d stderr=%s", rc, errBuf.String())
	}
	return out.String(), nil
}

func assertHasSection(t *testing.T, review, section string) {
	t.Helper()
	if !strings.Contains(review, "## "+section) {
		t.Errorf("missing `## %s` heading; review=%q", section, review)
	}
}

// glueReviewHeaderPattern accepts the canonical `## glue-review`
// header AND the common drift variants the free models produce
// (no space after `##`, extra spaces, mixed case, or an immediately
// attached severity token). Tightening the matcher leads to brittleness
// without catching real regressions — the model has correctly
// identified itself either way. (#127, #140)
var glueReviewHeaderPattern = regexp.MustCompile(`(?im)^##\s*glue-review(?:\b|(?:low|medium|major|critical)\b)`)

// glueReviewLGTMPattern accepts the canonical clean-review sentence
// plus compact variants produced by free models, such as
// `Noconcerns — LGTM`.
var glueReviewLGTMPattern = regexp.MustCompile(`(?im)\bno\s*concerns\s*[—-]?\s*lgtm\b`)

func reviewLooksLGTM(review string) bool {
	return glueReviewLGTMPattern.MatchString(review)
}

// assertGlueReviewHeader checks that the canonical `## glue-review`
// header is present at the start of the review body. Whitespace
// around / after `##` is tolerated.
func assertGlueReviewHeader(t *testing.T, review string) {
	t.Helper()
	if !glueReviewHeaderPattern.MatchString(review) {
		t.Errorf("missing `## glue-review` header (or drift variant); review=%q", review)
	}
}

// fixBlockPattern accepts the canonical fenced ```markdown fix block.
// Free models sometimes split the language marker onto its own first
// line inside an unlabeled fence, so that form is tolerated too.
var fixBlockPattern = regexp.MustCompile("(?is)```\\s*(?:markdown\\s*)?(?:\\n\\s*markdown\\s*)?Fix the following")

// assertHasFixBlock checks that the fenced ```markdown fix-instruction
// block is present. Required for Variant A (issues found); not required
// for Variant B (clean) or Variant C (rejected) — caller decides.
// Whitespace right after the opening fence is tolerated (#127).
func assertHasFixBlock(t *testing.T, review string) {
	t.Helper()
	if !fixBlockPattern.MatchString(review) {
		t.Errorf("missing fenced ```markdown fix block; review=%q", review)
	}
}

func regexpMatch(s, pattern string) bool {
	rx, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return rx.MatchString(s)
}
