package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

func openTempStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Options{Path: filepath.Join(t.TempDir(), "glue.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openMemStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func textMessage(role glue.MessageRole, body string) glue.Message {
	return glue.Message{
		Role:    role,
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: body}},
	}
}

func ftsRowCount(t *testing.T, s *Store) int {
	t.Helper()
	row := s.DB().QueryRow(`SELECT count(*) FROM messages_fts`)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("fts count: %v", err)
	}
	return n
}

func msgRowCount(t *testing.T, s *Store) int {
	t.Helper()
	row := s.DB().QueryRow(`SELECT count(*) FROM messages`)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("msg count: %v", err)
	}
	return n
}

func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "glue.db")
	s1, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Re-open against the same file should succeed and find the schema.
	s2, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	row := s2.DB().QueryRow(`SELECT version FROM schema_version`)
	var v int
	if err := row.Scan(&v); err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if v != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", v, SchemaVersion)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	ctx := context.Background()
	created := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	updated := created.Add(2 * time.Hour)
	state := glue.SessionState{
		Version:   glue.SessionStateVersion,
		ID:        "sess-1",
		CreatedAt: created,
		UpdatedAt: updated,
		Metadata:  map[string]any{"agent": "peggy"},
		Messages: []glue.Message{
			textMessage(glue.MessageRoleUser, "I love Australian Shepherds"),
			textMessage(glue.MessageRoleAssistant, "they shed often"),
		},
	}
	if err := s.Save(ctx, "sess-1", state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, found, err := s.Load(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("found = false; want true")
	}
	if got.ID != "sess-1" {
		t.Errorf("id = %s", got.ID)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("created_at = %s want %s", got.CreatedAt, created)
	}
	if !got.UpdatedAt.Equal(updated) {
		t.Errorf("updated_at = %s want %s", got.UpdatedAt, updated)
	}
	if got.Metadata["agent"] != "peggy" {
		t.Errorf("metadata = %v", got.Metadata)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d", len(got.Messages))
	}
	if got.Messages[0].Role != glue.MessageRoleUser || got.Messages[0].Content[0].Text != "I love Australian Shepherds" {
		t.Errorf("msg[0] = %+v", got.Messages[0])
	}
	if got.Messages[1].Role != glue.MessageRoleAssistant {
		t.Errorf("msg[1] role = %s", got.Messages[1].Role)
	}
}

func TestLoadMissingReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	_, found, err := s.Load(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Fatal("found = true; want false")
	}
}

func TestSaveReplacesMessagesNoDuplicates(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	ctx := context.Background()
	id := "sess-replace"

	first := glue.SessionState{
		ID: id,
		Messages: []glue.Message{
			textMessage(glue.MessageRoleUser, "alpha alpha"),
			textMessage(glue.MessageRoleAssistant, "beta beta"),
			textMessage(glue.MessageRoleUser, "gamma gamma"),
		},
	}
	if err := s.Save(ctx, id, first); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if got := msgRowCount(t, s); got != 3 {
		t.Fatalf("first save messages count = %d", got)
	}
	if got := ftsRowCount(t, s); got != 3 {
		t.Fatalf("first save fts count = %d", got)
	}

	// Second save replaces all messages.
	second := glue.SessionState{
		ID: id,
		Messages: []glue.Message{
			textMessage(glue.MessageRoleUser, "delta delta"),
			textMessage(glue.MessageRoleAssistant, "epsilon epsilon"),
		},
	}
	if err := s.Save(ctx, id, second); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if got := msgRowCount(t, s); got != 2 {
		t.Fatalf("after replace messages count = %d", got)
	}
	if got := ftsRowCount(t, s); got != 2 {
		t.Fatalf("after replace fts count = %d (FTS triggers misfired)", got)
	}

	// FTS reflects new content, not old.
	row := s.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'alpha'`)
	var n int
	_ = row.Scan(&n)
	if n != 0 {
		t.Errorf("fts still contains 'alpha' (%d hits)", n)
	}
	row = s.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'delta'`)
	_ = row.Scan(&n)
	if n != 1 {
		t.Errorf("fts missing 'delta' (%d hits)", n)
	}
}

func TestDeleteCascadesToMessagesAndFTS(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	ctx := context.Background()
	id := "sess-del"
	if err := s.Save(ctx, id, glue.SessionState{
		ID: id,
		Messages: []glue.Message{
			textMessage(glue.MessageRoleUser, "keepable"),
			textMessage(glue.MessageRoleAssistant, "keepable"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if msgRowCount(t, s) != 2 || ftsRowCount(t, s) != 2 {
		t.Fatalf("setup invariant failed")
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := msgRowCount(t, s); got != 0 {
		t.Errorf("messages after delete = %d", got)
	}
	if got := ftsRowCount(t, s); got != 0 {
		t.Errorf("fts after delete = %d (cascade trigger misfired)", got)
	}
	if _, found, err := s.Load(ctx, id); err != nil {
		t.Fatalf("Load: %v", err)
	} else if found {
		t.Error("session still present after delete")
	}
}

func TestDeleteMissingIsNoOp(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	if err := s.Delete(context.Background(), "nope"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestFTSTriggers_InsertUpdateDelete(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	ctx := context.Background()
	id := "sess-fts"

	// INSERT trigger: text reaches FTS.
	if err := s.Save(ctx, id, glue.SessionState{
		ID: id,
		Messages: []glue.Message{
			textMessage(glue.MessageRoleUser, "the quick brown fox"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !ftsMatches(t, s, "quick") {
		t.Fatal("FTS insert trigger did not fire")
	}

	// UPDATE: simulated by re-save with new text. The save path issues
	// DELETE + INSERT (not UPDATE), so the AU trigger isn't on this
	// code path — but we still need to verify content turnover.
	if err := s.Save(ctx, id, glue.SessionState{
		ID: id,
		Messages: []glue.Message{
			textMessage(glue.MessageRoleUser, "lazy dog jumps"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if ftsMatches(t, s, "quick") {
		t.Errorf("FTS still matches old text 'quick' after re-save")
	}
	if !ftsMatches(t, s, "lazy") {
		t.Errorf("FTS missing new text 'lazy'")
	}

	// Explicit UPDATE through SQL exercises the AU trigger directly.
	row := s.DB().QueryRow(`SELECT rowid FROM messages WHERE session_id = ?`, id)
	var rowid int64
	if err := row.Scan(&rowid); err != nil {
		t.Fatalf("scan rowid: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE messages SET content_text = ? WHERE rowid = ?`, "moonshot", rowid); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !ftsMatches(t, s, "moonshot") {
		t.Errorf("AU trigger did not propagate UPDATE")
	}
	if ftsMatches(t, s, "lazy") {
		t.Errorf("AU trigger did not remove old text 'lazy'")
	}

	// DELETE trigger: directly delete the row.
	if _, err := s.DB().Exec(`DELETE FROM messages WHERE rowid = ?`, rowid); err != nil {
		t.Fatalf("delete row: %v", err)
	}
	if ftsRowCount(t, s) != 0 {
		t.Errorf("AD trigger did not remove FTS row")
	}
}

func ftsMatches(t *testing.T, s *Store, term string) bool {
	t.Helper()
	row := s.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, term)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("fts match: %v", err)
	}
	return n > 0
}

func TestTextForFTSStripsToolCallContent(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	ctx := context.Background()
	id := "sess-ignore"
	args := []byte(`{"q":"hauptbahnhof"}`)
	state := glue.SessionState{
		ID: id,
		Messages: []glue.Message{
			{Role: glue.MessageRoleAssistant, Content: []glue.ContentPart{
				{Type: glue.ContentTypeText, Text: "ok"},
				{Type: glue.ContentTypeToolCall, ToolCall: &glue.ToolCall{
					ID: "c1", Name: "lookup", Arguments: args,
				}},
			}},
		},
	}
	if err := s.Save(ctx, id, state); err != nil {
		t.Fatal(err)
	}
	// Text 'ok' must be indexed.
	if !ftsMatches(t, s, "ok") {
		t.Error("text content not indexed")
	}
	// Tool-call args must NOT be in FTS — a tool_call's arguments are
	// structural, not content. Use a distinctive single token so the
	// FTS query parser doesn't interpret it.
	if ftsMatches(t, s, "hauptbahnhof") {
		t.Error("tool call args leaked into FTS index")
	}
}

func TestSaveAppliesDefaults(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	ctx := context.Background()
	id := "sess-defaults"
	before := time.Now().UTC().Add(-time.Second)
	if err := s.Save(ctx, id, glue.SessionState{}); err != nil { // zero state
		t.Fatalf("Save: %v", err)
	}
	got, _, err := s.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != glue.SessionStateVersion {
		t.Errorf("version = %d", got.Version)
	}
	if got.CreatedAt.Before(before) {
		t.Errorf("created_at = %s (want defaulted to ~now)", got.CreatedAt)
	}
	if got.UpdatedAt.Before(before) {
		t.Errorf("updated_at = %s", got.UpdatedAt)
	}
}

func TestSave_BlankIDErrors(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	if err := s.Save(context.Background(), "", glue.SessionState{}); err == nil {
		t.Fatal("want error for empty id")
	}
}

func TestInMemoryDB(t *testing.T) {
	t.Parallel()
	s := openMemStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, "m", glue.SessionState{ID: "m", Messages: []glue.Message{
		textMessage(glue.MessageRoleUser, "memory only"),
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !ftsMatches(t, s, "memory") {
		t.Error("FTS not working in :memory: mode")
	}
}

func TestConcurrentSavesForDistinctSessions(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	const n = 8
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := "s" + string(rune('a'+i))
			err := s.Save(ctx, id, glue.SessionState{
				ID: id,
				Messages: []glue.Message{
					textMessage(glue.MessageRoleUser, "hello from "+id),
				},
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent saves deadlocked")
	}
	close(errs)
	for err := range errs {
		t.Errorf("save: %v", err)
	}
	if msgRowCount(t, s) != n {
		t.Errorf("messages = %d, want %d", msgRowCount(t, s), n)
	}
}

func TestOpenWithoutPathErrors(t *testing.T) {
	t.Parallel()
	if _, err := Open(Options{}); err == nil {
		t.Fatal("expected error for empty path")
	}
}
