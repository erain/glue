package peggy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erain/glue"
	filestore "github.com/erain/glue/stores/file"
	sqlitestore "github.com/erain/glue/stores/sqlite"
)

func newFileBackedPeggy(t *testing.T) *Peggy {
	t.Helper()
	store := filestore.New(filepath.Join(t.TempDir(), "sessions"))
	p, err := New(Options{
		Settings: Settings{},
		Provider: &fakeProvider{text: "ok"},
		Store:    store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func newSQLitePeggy(t *testing.T) *Peggy {
	t.Helper()
	store, err := sqlitestore.Open(sqlitestore.Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	p, err := New(Options{
		Settings: Settings{},
		Provider: &fakeProvider{text: "ok"},
		Store:    store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestAddMemory_RoundTripsThroughStore(t *testing.T) {
	p := newFileBackedPeggy(t)
	ctx := context.Background()

	if _, err := p.AddMemory(ctx, "User's Aussie is named Inkblot.", []string{"pet"}); err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	if _, err := p.AddMemory(ctx, "User prefers terse responses.", []string{"preference"}); err != nil {
		t.Fatalf("AddMemory 2: %v", err)
	}

	memories, err := p.ListMemories(ctx)
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("memories = %d, want 2", len(memories))
	}
	// Newest first.
	if !strings.Contains(memories[0].Content, "terse") {
		t.Errorf("ListMemories not newest-first: %+v", memories)
	}
	if memories[0].Tags == nil || memories[0].Tags[0] != "preference" {
		t.Errorf("tags missing: %+v", memories[0])
	}
	if memories[0].ID == "" || !strings.HasPrefix(memories[0].ID, "mem_") {
		t.Errorf("memory id missing: %+v", memories[0])
	}
}

func TestAddMemory_PersistsAcrossPeggyRebuild(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions")

	first, err := New(Options{
		Settings: Settings{},
		Provider: &fakeProvider{text: "ok"},
		Store:    filestore.New(storePath),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := first.AddMemory(context.Background(), "User's Aussie is named Inkblot.", []string{"pet"}); err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	_ = first.Close()

	second, err := New(Options{
		Settings: Settings{},
		Provider: &fakeProvider{text: "ok"},
		Store:    filestore.New(storePath),
	})
	if err != nil {
		t.Fatalf("New 2: %v", err)
	}
	defer second.Close()

	got, err := second.ListMemories(context.Background())
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Content, "Inkblot") {
		t.Fatalf("memories not persisted across rebuild: %+v", got)
	}
}

func TestAddMemory_RejectsBlankContent(t *testing.T) {
	p := newFileBackedPeggy(t)
	if _, err := p.AddMemory(context.Background(), "   ", nil); err == nil {
		t.Fatal("expected error for blank content")
	}
}

func TestListMemoriesSynthesizesIDsForExistingRecords(t *testing.T) {
	p := newFileBackedPeggy(t)
	ctx := context.Background()
	mem, err := p.AddMemory(ctx, "User likes green tea.", []string{"preference"})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := p.store.Load(ctx, MemoriesSessionID)
	if err != nil {
		t.Fatal(err)
	}
	delete(state.Messages[0].Metadata, "id")
	if err := p.store.Save(ctx, MemoriesSessionID, state); err != nil {
		t.Fatal(err)
	}
	memories, err := p.ListMemories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].ID == "" || memories[0].ID != mem.ID {
		t.Fatalf("memories = %+v, want synthesized stable id %s", memories, mem.ID)
	}
}

func TestForgetMemory_RemovesByID(t *testing.T) {
	p := newFileBackedPeggy(t)
	ctx := context.Background()
	keep, err := p.AddMemory(ctx, "User likes green tea.", []string{"preference"})
	if err != nil {
		t.Fatal(err)
	}
	remove, err := p.AddMemory(ctx, "User dislikes stale context.", []string{"preference"})
	if err != nil {
		t.Fatal(err)
	}
	forgotten, err := p.ForgetMemory(ctx, remove.ID)
	if err != nil {
		t.Fatalf("ForgetMemory: %v", err)
	}
	if forgotten.ID != remove.ID {
		t.Fatalf("forgotten = %+v, want %s", forgotten, remove.ID)
	}
	memories, err := p.ListMemories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].ID != keep.ID {
		t.Fatalf("memories after forget = %+v, want only %s", memories, keep.ID)
	}
}

func TestForgetMemory_UnknownIDErrors(t *testing.T) {
	p := newFileBackedPeggy(t)
	if _, err := p.ForgetMemory(context.Background(), "mem_missing"); err == nil {
		t.Fatal("expected error for unknown memory id")
	}
}

func TestRecall_FindsMemoriesViaFTS(t *testing.T) {
	p := newSQLitePeggy(t)
	ctx := context.Background()

	if _, err := p.AddMemory(ctx, "User's Australian Shepherd is named Inkblot.", []string{"pet"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddMemory(ctx, "User braises pork shoulder at 275 for several hours.", []string{"cooking"}); err != nil {
		t.Fatal(err)
	}
	hits, err := p.Recall(ctx, "Australian", WithMemoriesOnly())
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	for _, h := range hits {
		if h.SessionID != MemoriesSessionID {
			t.Errorf("hit outside memories session: %+v", h)
		}
	}
}

func TestRecall_AcrossAllSessions(t *testing.T) {
	p := newSQLitePeggy(t)
	ctx := context.Background()
	// Drop a memory and run a prompt against an ordinary session so
	// the conversation history also gets indexed.
	if _, err := p.AddMemory(ctx, "User's Australian Shepherd is named Inkblot.", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Prompt(ctx, "casual", "Hello", nil); err != nil {
		t.Fatal(err)
	}

	// Recall without WithMemoriesOnly should reach both sessions.
	hits, err := p.Recall(ctx, "Australian")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected hits across all sessions")
	}
}

func TestRecall_LimitClampedAndDefaulted(t *testing.T) {
	p := newSQLitePeggy(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		if _, err := p.AddMemory(ctx, "fact about cooking onions and garlic", nil); err != nil {
			t.Fatal(err)
		}
	}
	// Default 5.
	hits, _ := p.Recall(ctx, "cooking", WithMemoriesOnly())
	if len(hits) != 5 {
		t.Errorf("default limit hits = %d, want 5", len(hits))
	}
	// Over-cap clamped to maxRecallLimit (20).
	hits, _ = p.Recall(ctx, "cooking", WithMemoriesOnly(), WithRecallLimit(100))
	if len(hits) != 20 {
		t.Errorf("clamped hits = %d, want 20", len(hits))
	}
}

func TestRecall_BlankQueryErrors(t *testing.T) {
	p := newSQLitePeggy(t)
	if _, err := p.Recall(context.Background(), "  "); err == nil {
		t.Fatal("expected error")
	}
}

func TestMemoryTools_DefaultRegistration(t *testing.T) {
	p := newSQLitePeggy(t)
	tools := p.agent
	if tools == nil {
		t.Fatal("agent nil")
	}
	// Run a prompt to see what tools the provider gets.
	fp := &fakeProvider{text: "hi"}
	// Swap provider by wrapping in a custom recall.
	pCustom, err := New(Options{
		Settings: Settings{},
		Provider: fp,
		Store:    p.store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pCustom.Close()
	if _, err := pCustom.Prompt(context.Background(), "x", "hello", nil); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, t := range fp.requests[0].Tools {
		names[t.Name] = true
	}
	if !names["remember"] || !names["recall"] {
		t.Errorf("memory tools missing from ProviderRequest.Tools: %+v", fp.requests[0].Tools)
	}
}

func TestMemoryTools_DisabledViaOption(t *testing.T) {
	fp := &fakeProvider{text: "hi"}
	store, _ := sqlitestore.Open(sqlitestore.Options{Path: ":memory:"})
	p, err := New(Options{
		Settings:           Settings{},
		Provider:           fp,
		Store:              store,
		DisableMemoryTools: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Prompt(context.Background(), "x", "hi", nil); err != nil {
		t.Fatal(err)
	}
	for _, tool := range fp.requests[0].Tools {
		if tool.Name == "remember" || tool.Name == "recall" {
			t.Errorf("memory tool present despite DisableMemoryTools: %s", tool.Name)
		}
	}
	if strings.Contains(fp.requests[0].SystemPrompt, "remember(") {
		t.Errorf("memory hint leaked into system prompt despite disable: %q", fp.requests[0].SystemPrompt)
	}
}

func TestMemoryHint_ReachesSystemPrompt(t *testing.T) {
	fp := &fakeProvider{text: "hi"}
	store, _ := sqlitestore.Open(sqlitestore.Options{Path: ":memory:"})
	p, err := New(Options{
		Settings: Settings{},
		Soul:     "# Identity\nYou are Peggy.",
		Provider: fp,
		Store:    store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Prompt(context.Background(), "x", "hi", nil); err != nil {
		t.Fatal(err)
	}
	sys := fp.requests[0].SystemPrompt
	if !strings.Contains(sys, "You are Peggy.") {
		t.Errorf("SOUL.md content missing: %q", sys)
	}
	if !strings.Contains(sys, "remember(") {
		t.Errorf("memory hint missing: %q", sys)
	}
}

func TestMemoryHint_CustomOverride(t *testing.T) {
	fp := &fakeProvider{text: "hi"}
	store, _ := sqlitestore.Open(sqlitestore.Options{Path: ":memory:"})
	p, err := New(Options{
		Settings:   Settings{},
		Provider:   fp,
		Store:      store,
		MemoryHint: "Custom hint: use them wisely.",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Prompt(context.Background(), "x", "hi", nil); err != nil {
		t.Fatal(err)
	}
	sys := fp.requests[0].SystemPrompt
	if !strings.Contains(sys, "Custom hint") {
		t.Errorf("custom hint missing: %q", sys)
	}
	if strings.Contains(sys, "DefaultMemoryHint") || strings.Contains(sys, "Phrase content in third person") {
		t.Errorf("default hint should have been replaced; got %q", sys)
	}
}

func TestRememberTool_PersistsViaExecutor(t *testing.T) {
	p := newSQLitePeggy(t)
	tool := RememberTool(p)
	args, _ := json.Marshal(rememberArgs{Content: "User likes terse responses.", Tags: []string{"preference"}})
	res, err := tool.Execute(context.Background(), glue.ToolCall{ID: "c1", Name: "remember", Arguments: args})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	memories, _ := p.ListMemories(context.Background())
	if len(memories) != 1 {
		t.Fatalf("memory not stored: %+v", memories)
	}
	if !strings.Contains(memories[0].Content, "terse") {
		t.Errorf("stored content wrong: %+v", memories[0])
	}
}

func TestRememberTool_BlankContentErrors(t *testing.T) {
	p := newSQLitePeggy(t)
	tool := RememberTool(p)
	args, _ := json.Marshal(rememberArgs{Content: "   "})
	res, err := tool.Execute(context.Background(), glue.ToolCall{ID: "c1", Name: "remember", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected error tool result for blank content")
	}
}

func TestRememberTool_MalformedArgsSurface(t *testing.T) {
	p := newSQLitePeggy(t)
	tool := RememberTool(p)
	res, err := tool.Execute(context.Background(), glue.ToolCall{ID: "c1", Name: "remember", Arguments: []byte("{not json")})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected decode error to surface as error result")
	}
}

func TestRecallTool_ReturnsHits(t *testing.T) {
	p := newSQLitePeggy(t)
	ctx := context.Background()
	if _, err := p.AddMemory(ctx, "User's dog is an Aussie named Inkblot.", []string{"pet"}); err != nil {
		t.Fatal(err)
	}
	tool := RecallTool(p)
	args, _ := json.Marshal(recallArgs{Query: "Aussie", OnlyMemories: true})
	res, err := tool.Execute(ctx, glue.ToolCall{ID: "c1", Name: "recall", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "hits:") || !strings.Contains(text, "Aussie") {
		t.Errorf("unexpected result text: %q", text)
	}
}

func TestRecallTool_BlankQueryErrors(t *testing.T) {
	p := newSQLitePeggy(t)
	tool := RecallTool(p)
	args, _ := json.Marshal(recallArgs{Query: "  "})
	res, err := tool.Execute(context.Background(), glue.ToolCall{ID: "c", Name: "recall", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected error tool result for blank query")
	}
}

func TestRecallTool_FileStoreSurfaceFriendlyError(t *testing.T) {
	// stores/file doesn't implement Searcher; the tool should surface
	// a clear "sqlite store required" message rather than a generic
	// ErrSearchNotSupported.
	store := filestore.New(filepath.Join(t.TempDir(), "sessions"))
	p, err := New(Options{Settings: Settings{}, Provider: &fakeProvider{text: "ok"}, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	tool := RecallTool(p)
	args, _ := json.Marshal(recallArgs{Query: "anything"})
	res, err := tool.Execute(context.Background(), glue.ToolCall{ID: "c", Name: "recall", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected error tool result")
	}
	if !strings.Contains(res.Content[0].Text, "sqlite store") {
		t.Errorf("unhelpful error: %q", res.Content[0].Text)
	}
}

func TestFormatRecallHits_NoHits(t *testing.T) {
	if got := formatRecallHits(nil); got != "No hits." {
		t.Errorf("got %q", got)
	}
}
