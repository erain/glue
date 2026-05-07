package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/erain/glue/loop"
)

type fakeProv struct{ name string }

func (fakeProv) Stream(_ context.Context, _ loop.ProviderRequest) (<-chan loop.ProviderEvent, error) {
	ch := make(chan loop.ProviderEvent)
	close(ch)
	return ch, nil
}

func TestRegistry_RegisterAndNew(t *testing.T) {
	Register("test-fake", Factory{
		New:          func() loop.Provider { return fakeProv{name: "fake"} },
		DefaultModel: "fake/model",
		EnvKey:       "TEST_FAKE_KEY",
	})

	p, model, env, err := New("test-fake")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	if model != "fake/model" {
		t.Fatalf("model: got %q want %q", model, "fake/model")
	}
	if env != "TEST_FAKE_KEY" {
		t.Fatalf("env: got %q want %q", env, "TEST_FAKE_KEY")
	}
}

func TestRegistry_Unknown(t *testing.T) {
	_, _, _, err := New("definitely-not-registered")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("error should include 'unknown provider': %v", err)
	}
}

func TestRegistry_KeyAvailable(t *testing.T) {
	Register("test-key-probe", Factory{
		New:    func() loop.Provider { return fakeProv{} },
		EnvKey: "TEST_KEY_PROBE_VAR",
	})

	t.Setenv("TEST_KEY_PROBE_VAR", "")
	if KeyAvailable("test-key-probe") {
		t.Fatal("expected false when env var unset")
	}
	t.Setenv("TEST_KEY_PROBE_VAR", "value")
	if !KeyAvailable("test-key-probe") {
		t.Fatal("expected true when env var set")
	}

	if KeyAvailable("not-registered") {
		t.Fatal("unknown provider must not report KeyAvailable=true")
	}
}

func TestRegistry_KnownIsSorted(t *testing.T) {
	got := Known()
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("Known() not sorted: %v", got)
		}
	}
}

func TestRegistry_CaseInsensitive(t *testing.T) {
	Register("MixedCase", Factory{
		New: func() loop.Provider { return fakeProv{} },
	})
	if _, ok := Lookup("mixedcase"); !ok {
		t.Fatal("Lookup must be case-insensitive")
	}
	if _, ok := Lookup("MIXEDCASE"); !ok {
		t.Fatal("Lookup must be case-insensitive on uppercase too")
	}
}
