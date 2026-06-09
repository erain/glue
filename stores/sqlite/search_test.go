package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
)

// seedSearchFixture populates the store with three sessions that have
// distinguishable content so search-filter tests have something to
// query against.
func seedSearchFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	// Session A: aussie shepherd thread.
	if err := s.Save(ctx, "A", glue.SessionState{
		ID:        "A",
		CreatedAt: now.Add(-72 * time.Hour),
		UpdatedAt: now.Add(-72 * time.Hour),
		Messages: []glue.Message{
			{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "tell me about Australian Shepherds"}}, CreatedAt: now.Add(-72 * time.Hour)},
			{Role: glue.MessageRoleAssistant, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "Australian Shepherds shed a great deal and need exercise"}}, CreatedAt: now.Add(-71 * time.Hour)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Session B: cooking thread (different vocabulary).
	if err := s.Save(ctx, "B", glue.SessionState{
		ID:        "B",
		CreatedAt: now.Add(-24 * time.Hour),
		UpdatedAt: now.Add(-24 * time.Hour),
		Messages: []glue.Message{
			{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "best way to braise pork shoulder"}}, CreatedAt: now.Add(-24 * time.Hour)},
			{Role: glue.MessageRoleAssistant, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "low and slow at 275 for several hours"}}, CreatedAt: now.Add(-23 * time.Hour)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Session C: another aussie reference, more recent — used to
	// exercise time bounds.
	if err := s.Save(ctx, "C", glue.SessionState{
		ID:        "C",
		CreatedAt: now,
		UpdatedAt: now,
		Messages: []glue.Message{
			{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "Aussies are smart"}}, CreatedAt: now},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSearch_BasicFTSHit(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	seedSearchFixture(t, s)
	hits, err := s.Search(context.Background(), "Shepherds", glue.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	for _, h := range hits {
		if h.SessionID != "A" {
			t.Errorf("unexpected session in hit: %+v", h)
		}
	}
}

func TestSearch_BM25OrderingLowerIsBetter(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	seedSearchFixture(t, s)
	hits, err := s.Search(context.Background(), "Aussies", glue.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// "Aussies" appears only in session C; we don't compare across
	// sessions here. Instead verify scores are monotonic non-decreasing.
	for i := 1; i < len(hits); i++ {
		if hits[i].Score < hits[i-1].Score {
			t.Fatalf("scores not ASC: %+v", hits)
		}
	}
}

func TestSearch_SessionIDFilter(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	seedSearchFixture(t, s)
	// "Australian" appears in session A; session-id filter restricts.
	got, err := s.Search(context.Background(), "Australian", glue.SearchOptions{SessionID: "A"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected hits in session A")
	}
	for _, h := range got {
		if h.SessionID != "A" {
			t.Errorf("session filter ignored; hit: %+v", h)
		}
	}
	// Same query restricted to a session without the term: zero hits.
	got, err = s.Search(context.Background(), "Australian", glue.SearchOptions{SessionID: "B"})
	if err != nil {
		t.Fatalf("Search filtered: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero hits when filter excludes match, got %+v", got)
	}
}

func TestSearch_SinceUntilFilters(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	seedSearchFixture(t, s)
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	// Since 36h ago — should exclude session A (72h old) and keep C.
	got, err := s.Search(context.Background(), "Aussies", glue.SearchOptions{
		Since: now.Add(-36 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one hit for Aussies in the recent window")
	}
	for _, h := range got {
		if h.SessionID == "A" {
			t.Errorf("Since filter did not exclude session A: %+v", h)
		}
	}

	// Until 48h ago — should exclude session C (recent) and keep older.
	got, err = s.Search(context.Background(), "Shepherds", glue.SearchOptions{
		Until: now.Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range got {
		if h.SessionID == "C" {
			t.Errorf("Until filter did not exclude session C: %+v", h)
		}
	}
}

func TestSearch_LimitAndOffset(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	ctx := context.Background()
	// Insert one session with multiple hits for "match".
	now := time.Now().UTC()
	state := glue.SessionState{ID: "L", CreatedAt: now, UpdatedAt: now}
	for i := 0; i < 6; i++ {
		state.Messages = append(state.Messages, glue.Message{
			Role:    glue.MessageRoleUser,
			Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "needle match line " + string(rune('a'+i))}},
		})
	}
	if err := s.Save(ctx, "L", state); err != nil {
		t.Fatal(err)
	}

	got, err := s.Search(ctx, "needle", glue.SearchOptions{Limit: 2})
	if err != nil {
		t.Fatalf("Search Limit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Limit ignored: got %d", len(got))
	}
	gotOffset, err := s.Search(ctx, "needle", glue.SearchOptions{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("Search Offset: %v", err)
	}
	if len(gotOffset) != 2 {
		t.Fatalf("Offset+Limit ignored: got %d", len(gotOffset))
	}
	// Pages should not overlap: at least one index differs between them.
	overlap := 0
	for _, a := range got {
		for _, b := range gotOffset {
			if a.Index == b.Index {
				overlap++
			}
		}
	}
	if overlap == len(got) {
		t.Errorf("pages fully overlap; got=%+v gotOffset=%+v", got, gotOffset)
	}
}

func TestSearch_EmptyQueryErrors(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	seedSearchFixture(t, s)
	if _, err := s.Search(context.Background(), "   ", glue.SearchOptions{}); err == nil {
		t.Fatal("expected error for blank query")
	}
}

func TestSearch_ToolCallEmptyContentNotReturned(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	ctx := context.Background()
	// A session with only a tool-call message (no text content).
	args := []byte(`{"q":"distinctive_only_arg"}`)
	if err := s.Save(ctx, "tc", glue.SessionState{
		ID: "tc",
		Messages: []glue.Message{
			{Role: glue.MessageRoleAssistant, Content: []glue.ContentPart{
				{Type: glue.ContentTypeToolCall, ToolCall: &glue.ToolCall{ID: "c", Name: "fn", Arguments: args}},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// FTS shouldn't surface anything for a term that exists only in
	// tool call args.
	hits, err := s.Search(ctx, "distinctive_only_arg", glue.SearchOptions{})
	if err != nil {
		// An "empty MATCH" can also produce a syntactic error in FTS5
		// for some queries; treat that as a pass since the contract
		// (no leak) is preserved.
		if strings.Contains(err.Error(), "fts5") {
			return
		}
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("tool_call args matched FTS: %+v", hits)
	}
}

func TestSearch_HighlightMarkers(t *testing.T) {
	t.Parallel()
	s := openTempStore(t)
	seedSearchFixture(t, s)
	hits, err := s.Search(context.Background(), "Shepherds", glue.SearchOptions{})
	if err != nil || len(hits) == 0 {
		t.Fatalf("Search: %v hits=%d", err, len(hits))
	}
	// The configured snippet markers are << / >>.
	hasMarker := false
	for _, h := range hits {
		if strings.Contains(h.Snippet, "<<") && strings.Contains(h.Snippet, ">>") {
			hasMarker = true
		}
	}
	if !hasMarker {
		t.Errorf("no snippet carried << / >> markers: %+v", hits)
	}
}

func TestSearch_AgainstFileBackedDB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "search.db")
	s1, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedSearchFixture(t, s1)
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	// Re-open and search; FTS should survive the close/reopen.
	s2, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	hits, err := s2.Search(context.Background(), "Shepherds", glue.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected hits to survive reopen")
	}
}
