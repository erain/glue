package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
)

// fixedProvider always replies with the same text, regardless of how many
// times it is called — convenient for eval tests where the agent under
// test and the judge agent share one provider instance.
type fixedProvider struct{ text string }

func (p fixedProvider) Stream(_ context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	ch := make(chan glue.ProviderEvent, 3)
	ch <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	ch <- glue.ProviderEvent{Type: glue.ProviderEventTextDelta, Delta: p.text}
	ch <- glue.ProviderEvent{Type: glue.ProviderEventDone, Message: &glue.Message{
		Role:       glue.MessageRoleAssistant,
		Content:    []glue.ContentPart{{Type: glue.ContentTypeText, Text: p.text}},
		StopReason: glue.StopReasonStop,
	}}
	close(ch)
	return ch, nil
}

func writeSuite(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "suite.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const twoCaseSuite = `{
  "name": "smoke",
  "cases": [
    {"name": "ok",   "prompt": "say hello", "scorers": [{"type": "contains", "values": ["hello"]}]},
    {"name": "fail", "prompt": "say hello", "scorers": [{"type": "contains", "values": ["absent"]}]}
  ]
}`

func runEval(t *testing.T, provider glue.Provider, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	full := append([]string{"eval"}, args...)
	code = runCLI(context.Background(), full, &out, &errb, fakeFactory(provider))
	return code, out.String(), errb.String()
}

func TestEvalGateFailsBelowThreshold(t *testing.T) {
	suite := writeSuite(t, twoCaseSuite)
	store := t.TempDir()

	// One of two cases passes; requiring 100% must exit non-zero.
	code, stdout, stderr := runEval(t, fixedProvider{text: "hello!"},
		suite, "--store", store, "--min-pass-rate", "1")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "[PASS] ok") || !strings.Contains(stdout, "[FAIL] fail") {
		t.Errorf("report missing per-case marks:\n%s", stdout)
	}
	if !strings.Contains(stderr, "gate failed") {
		t.Errorf("expected a gate-failed message on stderr, got: %s", stderr)
	}
}

func TestEvalGatePassesAtRelaxedThreshold(t *testing.T) {
	suite := writeSuite(t, twoCaseSuite)
	code, stdout, stderr := runEval(t, fixedProvider{text: "hello!"},
		suite, "--store", t.TempDir(), "--min-pass-rate", "0.5")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "1/2 passed") {
		t.Errorf("report missing totals line:\n%s", stdout)
	}
}

func TestEvalJSONOutput(t *testing.T) {
	suite := writeSuite(t, twoCaseSuite)
	code, stdout, _ := runEval(t, fixedProvider{text: "hello!"},
		suite, "--store", t.TempDir(), "--min-pass-rate", "0", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"suite": "smoke"`) || !strings.Contains(stdout, `"pass_rate"`) {
		t.Errorf("expected JSON report, got:\n%s", stdout)
	}
}

func TestEvalRejectsMissingSuiteArg(t *testing.T) {
	code, _, stderr := runEval(t, fixedProvider{text: "x"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "usage: glue eval") {
		t.Errorf("expected usage error, got: %s", stderr)
	}
}
