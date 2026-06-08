package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

// memStore mirrors the in-test glue.Store used by the library's tree
// tests. Kept package-private so the TUI tree tests don't reach across
// packages.
type memStore struct {
	mu   sync.Mutex
	data map[string]glue.SessionState
}

func newMemStore() *memStore { return &memStore{data: map[string]glue.SessionState{}} }

func (s *memStore) Load(_ context.Context, id string) (glue.SessionState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	return v, ok, nil
}
func (s *memStore) Save(_ context.Context, id string, state glue.SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[id] = state
	return nil
}
func (s *memStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return nil
}

// ListSessions makes memStore qualify as glue.SessionLister, so
// Agent.ListSessions inside the tree builder finds our sessions.
func (s *memStore) ListSessions(_ context.Context, _ glue.ListSessionsOptions) ([]glue.SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]glue.SessionSummary, 0, len(s.data))
	for id, st := range s.data {
		out = append(out, glue.SessionSummary{
			ID:        id,
			CreatedAt: st.CreatedAt,
			UpdatedAt: st.UpdatedAt,
			Messages:  len(st.Messages),
		})
	}
	return out, nil
}

type fakeProvider struct{}

func (fakeProvider) Stream(_ context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	return nil, nil
}

// buildTreeFixture seeds: root → child1 → grandchild; root → child2.
// All forked from message index 2 unless noted.
func buildTreeFixture(t *testing.T) (*glue.Agent, *memStore) {
	t.Helper()
	st := newMemStore()
	a := glue.NewAgent(glue.AgentOptions{Provider: fakeProvider{}, Store: st})

	base := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	mk := func(id string, parent string, atIdx int, msgs int, created time.Time) {
		meta := map[string]any{}
		if parent != "" {
			meta[glue.MetadataKeyParentSessionID] = parent
			meta[glue.MetadataKeyParentMessageIndex] = atIdx
		}
		body := make([]glue.Message, 0, msgs)
		for i := 0; i < msgs; i++ {
			body = append(body, glue.Message{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "x"}}})
		}
		_ = st.Save(context.Background(), id, glue.SessionState{
			Version:   glue.SessionStateVersion,
			ID:        id,
			Messages:  body,
			Metadata:  meta,
			CreatedAt: created,
			UpdatedAt: created,
		})
	}
	mk("root", "", 0, 4, base)
	mk("child1", "root", 2, 5, base.Add(1*time.Hour))
	mk("grandchild", "child1", 3, 6, base.Add(2*time.Hour))
	mk("child2", "root", 2, 4, base.Add(30*time.Minute))
	mk("unrelated", "", 0, 1, base.Add(3*time.Hour))
	return a, st
}

func TestBuildSessionTreeFindsRootAndDescendants(t *testing.T) {
	t.Parallel()
	a, st := buildTreeFixture(t)
	root, flat, cursor, err := buildSessionTree(context.Background(), a, st, "grandchild")
	if err != nil {
		t.Fatal(err)
	}
	if root.Summary.ID != "root" {
		t.Fatalf("root id = %q, want root", root.Summary.ID)
	}
	// Flat order is DFS. With the fixture, child2 was created before
	// child1, so the DFS visits root → child2 → child1 → grandchild.
	var ids []string
	for _, n := range flat {
		ids = append(ids, n.Summary.ID)
	}
	wantOrder := []string{"root", "child2", "child1", "grandchild"}
	if strings.Join(ids, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("DFS order = %v, want %v", ids, wantOrder)
	}
	if cursor != 3 || flat[cursor].Summary.ID != "grandchild" {
		t.Fatalf("cursor = %d (id %q), want 3 (grandchild)", cursor, flat[cursor].Summary.ID)
	}
	// Unrelated session must be absent.
	for _, n := range flat {
		if n.Summary.ID == "unrelated" {
			t.Fatal("unrelated session leaked into the lineage")
		}
	}
}

func TestTreeModalNavigationClamps(t *testing.T) {
	t.Parallel()
	a, st := buildTreeFixture(t)
	_, flat, cursor, err := buildSessionTree(context.Background(), a, st, "root")
	if err != nil {
		t.Fatal(err)
	}
	m := &treeModal{flat: flat, cursor: cursor}
	for i := 0; i < 10; i++ {
		m.down()
	}
	if m.cursor != len(flat)-1 {
		t.Fatalf("down past end: cursor = %d, want %d", m.cursor, len(flat)-1)
	}
	for i := 0; i < 10; i++ {
		m.up()
	}
	if m.cursor != 0 {
		t.Fatalf("up past start: cursor = %d, want 0", m.cursor)
	}
}

func TestRenderTreeModalContainsExpectedLabels(t *testing.T) {
	t.Parallel()
	a, st := buildTreeFixture(t)
	root, flat, _, err := buildSessionTree(context.Background(), a, st, "grandchild")
	if err != nil {
		t.Fatal(err)
	}
	m := &treeModal{root: root, flat: flat, cursor: 3} // grandchild
	out := stripANSI(renderTreeModal(m, 100, "grandchild"))
	for _, want := range []string{"Session tree", "root", "child1", "child2", "grandchild", "forked@2", "forked@3", "↑/↓"} {
		if !strings.Contains(out, want) {
			t.Errorf("tree render missing %q in:\n%s", want, out)
		}
	}
	// Root must NOT have a "forked@" tag.
	idx := strings.Index(out, "root")
	if idx >= 0 {
		// Check the root's line doesn't include "forked@".
		// Slice up to end-of-line for that occurrence.
		end := strings.IndexByte(out[idx:], '\n')
		if end < 0 {
			end = len(out) - idx
		}
		if strings.Contains(out[idx:idx+end], "forked@") {
			t.Fatalf("root line should not say forked@; got %q", out[idx:idx+end])
		}
	}
}

func TestRenderTreeModalMarksCurrent(t *testing.T) {
	t.Parallel()
	a, st := buildTreeFixture(t)
	root, flat, _, err := buildSessionTree(context.Background(), a, st, "child1")
	if err != nil {
		t.Fatal(err)
	}
	m := &treeModal{root: root, flat: flat, cursor: 0}
	out := stripANSI(renderTreeModal(m, 100, "child1"))
	// "◉ <id>" marks the active session; "● <id>" marks others.
	if !strings.Contains(out, "◉ child1") {
		t.Fatalf("child1 not marked current\n%s", out)
	}
	if !strings.Contains(out, "● root") {
		t.Fatalf("root not marked non-current\n%s", out)
	}
}

func TestLastUserMessageIndex(t *testing.T) {
	t.Parallel()
	msgs := []glue.Message{
		{Role: glue.MessageRoleUser},
		{Role: glue.MessageRoleAssistant},
		{Role: glue.MessageRoleUser},
		{Role: glue.MessageRoleAssistant},
	}
	if got := lastUserMessageIndex(msgs); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
	if got := lastUserMessageIndex([]glue.Message{{Role: glue.MessageRoleAssistant}}); got != -1 {
		t.Fatalf("got %d, want -1 (no user msg)", got)
	}
}

func TestParseForkArg(t *testing.T) {
	t.Parallel()
	if n, err := parseForkArg("3", 10); err != nil || n != 3 {
		t.Fatalf("3/10 -> %d,%v", n, err)
	}
	if n, err := parseForkArg("10", 10); err != nil || n != 10 {
		t.Fatalf("10/10 -> %d,%v", n, err)
	}
	if _, err := parseForkArg("11", 10); err == nil {
		t.Fatal("11/10 should error")
	}
	if _, err := parseForkArg("-1", 10); err == nil {
		t.Fatal("-1/10 should error")
	}
	if _, err := parseForkArg("abc", 10); err == nil {
		t.Fatal("abc should error")
	}
}
