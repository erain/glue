package peggy

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
	filestore "github.com/erain/glue/stores/file"
)

// fakeProvider returns a deterministic assistant message.
type fakeProvider struct {
	text     string
	requests []glue.ProviderRequest
	calls    int
	failWith error
}

func (p *fakeProvider) Stream(_ context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.requests = append(p.requests, req)
	p.calls++
	if p.failWith != nil {
		return nil, p.failWith
	}
	ch := make(chan glue.ProviderEvent, 4)
	ch <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	if p.text != "" {
		ch <- glue.ProviderEvent{Type: glue.ProviderEventTextDelta, Delta: p.text}
	}
	ch <- glue.ProviderEvent{Type: glue.ProviderEventDone, Message: &glue.Message{
		Role:    glue.MessageRoleAssistant,
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: p.text}},
	}}
	close(ch)
	return ch, nil
}

func newTestPeggy(t *testing.T, provider glue.Provider) *Peggy {
	t.Helper()
	store := filestore.New(filepath.Join(t.TempDir(), "sessions"))
	p, err := New(Options{
		Settings: Settings{}, // defaults applied inside New
		Provider: provider,
		Store:    store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestPeggy_PromptEndToEnd(t *testing.T) {
	fp := &fakeProvider{text: "Aussies shed a lot."}
	p := newTestPeggy(t, fp)

	var stdout bytes.Buffer
	text, err := p.Prompt(context.Background(), "default", "tell me about Aussies", &stdout)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if text != "Aussies shed a lot." {
		t.Errorf("text = %q", text)
	}
	if !strings.Contains(stdout.String(), "Aussies shed a lot.") {
		t.Errorf("stdout missing streamed text: %q", stdout.String())
	}
	if fp.calls != 1 {
		t.Errorf("provider calls = %d", fp.calls)
	}
}

func TestPeggy_SoulFlowsIntoSystemPrompt(t *testing.T) {
	fp := &fakeProvider{text: "ok"}
	store := filestore.New(filepath.Join(t.TempDir(), "sessions"))
	p, err := New(Options{
		Settings: Settings{},
		Soul:     "# Identity\nI am Peggy.\n\n# About me\nUser likes Australian Shepherds.",
		Provider: fp,
		Store:    store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	if _, err := p.Prompt(context.Background(), "x", "ping", nil); err != nil {
		t.Fatal(err)
	}
	if len(fp.requests) == 0 {
		t.Fatal("provider not called")
	}
	sys := fp.requests[0].SystemPrompt
	if !strings.Contains(sys, "Australian Shepherds") {
		t.Errorf("system prompt missing SOUL content: %q", sys)
	}
}

func TestPeggy_PromptPersistsTranscript(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "sessions")
	fp := &fakeProvider{text: "first"}
	p, err := New(Options{
		Settings: Settings{},
		Provider: fp,
		Store:    filestore.New(storeDir),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Prompt(context.Background(), "s1", "hello", nil); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()
	// Re-load with a fresh Peggy and a fresh provider; the transcript
	// should be on disk and replayed on the next prompt as context.
	fp2 := &fakeProvider{text: "second"}
	p2, err := New(Options{
		Settings: Settings{},
		Provider: fp2,
		Store:    filestore.New(storeDir),
	})
	if err != nil {
		t.Fatalf("New 2: %v", err)
	}
	defer p2.Close()
	if _, err := p2.Prompt(context.Background(), "s1", "follow-up", nil); err != nil {
		t.Fatal(err)
	}
	// Provider's request should include the prior transcript (1 user + 1
	// assistant from session 1) plus the new user message.
	if len(fp2.requests) == 0 {
		t.Fatal("fp2 not called")
	}
	msgs := fp2.requests[0].Messages
	if len(msgs) != 3 {
		t.Fatalf("transcript = %d, want 3 (prior user + prior assistant + new user)", len(msgs))
	}
	if msgs[0].Content[0].Text != "hello" {
		t.Errorf("first message wrong: %+v", msgs[0])
	}
	if msgs[2].Content[0].Text != "follow-up" {
		t.Errorf("new user message wrong: %+v", msgs[2])
	}
}

func TestPeggy_EmptyPromptErrors(t *testing.T) {
	p := newTestPeggy(t, &fakeProvider{text: "x"})
	if _, err := p.Prompt(context.Background(), "default", "   ", nil); err == nil {
		t.Fatal("expected error for blank prompt")
	}
}

func TestPeggy_ProviderErrorPropagates(t *testing.T) {
	p := newTestPeggy(t, &fakeProvider{failWith: errors.New("upstream down")})
	if _, err := p.Prompt(context.Background(), "default", "hi", nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestPeggy_AgentExposed(t *testing.T) {
	p := newTestPeggy(t, &fakeProvider{text: "x"})
	if p.Agent() == nil {
		t.Fatal("Agent() returned nil")
	}
	if p.Settings().Provider == "" {
		t.Errorf("Settings provider should default; got %q", p.Settings().Provider)
	}
}

func TestPeggy_New_DefaultProviderRequiresAuth(t *testing.T) {
	// With Provider == nil and no overrides, New will try to build a
	// codex provider (the default). The codex constructor itself
	// doesn't fail without auth; it only fails on Stream. So
	// New should succeed but Prompt will fail without a fake provider.
	// We exercise that path here.
	t.Setenv("PEGGY_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	storeDir := filepath.Join(t.TempDir(), "sessions")
	p, err := New(Options{
		Settings: Settings{Provider: "codex", Store: StoreSettings{Type: "file", Path: storeDir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	if p.Agent() == nil {
		t.Fatal("agent nil")
	}
}

func TestPeggy_New_UnknownProviderErrors(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "sessions")
	_, err := New(Options{
		Settings: Settings{Provider: "bogus-provider", Store: StoreSettings{Type: "file", Path: storeDir}},
	})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestPeggy_New_UnknownStoreErrors(t *testing.T) {
	_, err := New(Options{
		Settings: Settings{Provider: "codex", Store: StoreSettings{Type: "bogus", Path: "/tmp/x"}},
	})
	if err == nil {
		t.Fatal("expected error for unknown store type")
	}
}

func TestPeggy_Close_NilSafe(t *testing.T) {
	var p *Peggy
	if err := p.Close(); err != nil {
		t.Fatalf("nil Close err = %v", err)
	}
}
