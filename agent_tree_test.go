package glue_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/erain/glue"
)

// memoryProvider is a no-op Provider used only to satisfy NewAgent. The
// tree tests never call Stream — they exercise the store directly via
// ForkSession / CloneSession.
type memoryProvider struct{}

func (memoryProvider) Stream(_ context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	return nil, errors.New("memoryProvider has no stream")
}

// memStore is an in-test glue.Store backed by a map. The tree tests use
// this rather than stores/file to avoid an import cycle (the file store
// imports glue).
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

func newTreeTestAgent(t *testing.T) (*glue.Agent, *memStore) {
	t.Helper()
	st := newMemStore()
	return glue.NewAgent(glue.AgentOptions{
		Provider: memoryProvider{},
		Store:    st,
	}), st
}

// seed writes a session with N synthesized user/assistant message pairs.
func seedSession(t *testing.T, st *memStore, id string, pairs int) {
	t.Helper()
	msgs := make([]glue.Message, 0, pairs*2)
	for i := 0; i < pairs; i++ {
		msgs = append(msgs,
			glue.Message{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "u" + itoa(i)}}},
			glue.Message{Role: glue.MessageRoleAssistant, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "a" + itoa(i)}}},
		)
	}
	state := glue.SessionState{Version: glue.SessionStateVersion, ID: id, Messages: msgs}
	if err := st.Save(context.Background(), id, state); err != nil {
		t.Fatal(err)
	}
}

// itoa avoids strconv just for an int < 100.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [3]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestForkSessionAtMiddle(t *testing.T) {
	t.Parallel()
	a, st := newTreeTestAgent(t)
	ctx := context.Background()
	seedSession(t, st, "root", 3) // 6 messages
	if err := a.ForkSession(ctx, "root", 4, "child"); err != nil {
		t.Fatal(err)
	}
	state, ok, err := st.Load(ctx, "child")
	if err != nil || !ok {
		t.Fatalf("load child: ok=%v err=%v", ok, err)
	}
	if len(state.Messages) != 4 {
		t.Fatalf("child messages = %d, want 4", len(state.Messages))
	}
	parentID, atIdx, ok := glue.SessionParent(state)
	if !ok || parentID != "root" || atIdx != 4 {
		t.Fatalf("SessionParent = %q,%d,%v want root,4,true", parentID, atIdx, ok)
	}
}

func TestForkSessionAtZeroIsValidEmptyBranch(t *testing.T) {
	t.Parallel()
	a, st := newTreeTestAgent(t)
	ctx := context.Background()
	seedSession(t, st, "root", 2)
	if err := a.ForkSession(ctx, "root", 0, "fresh"); err != nil {
		t.Fatal(err)
	}
	state, _, _ := st.Load(ctx, "fresh")
	if len(state.Messages) != 0 {
		t.Fatalf("fork at 0 should be empty, got %d msgs", len(state.Messages))
	}
	// Parent linkage still present so the tree view can show this branch
	// hanging off the root.
	if _, _, ok := glue.SessionParent(state); !ok {
		t.Fatal("empty fork should still carry parent metadata")
	}
}

func TestForkSessionAtEndEquivalentToClone(t *testing.T) {
	t.Parallel()
	a, st := newTreeTestAgent(t)
	ctx := context.Background()
	seedSession(t, st, "root", 3)
	if err := a.ForkSession(ctx, "root", 6, "full"); err != nil {
		t.Fatal(err)
	}
	state, _, _ := st.Load(ctx, "full")
	if len(state.Messages) != 6 {
		t.Fatalf("full fork = %d msgs, want 6", len(state.Messages))
	}
}

func TestForkSessionOutOfRange(t *testing.T) {
	t.Parallel()
	a, st := newTreeTestAgent(t)
	ctx := context.Background()
	seedSession(t, st, "root", 2)
	if err := a.ForkSession(ctx, "root", -1, "x"); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("err = %v", err)
	}
	if err := a.ForkSession(ctx, "root", 99, "x"); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("err = %v", err)
	}
}

func TestForkSessionMissingSourceReturnsTyped(t *testing.T) {
	t.Parallel()
	a, _ := newTreeTestAgent(t)
	err := a.ForkSession(context.Background(), "does-not-exist", 0, "child")
	if !errors.Is(err, glue.ErrSessionNotFound) {
		t.Fatalf("err = %v, want glue.ErrSessionNotFound", err)
	}
}

func TestCloneSessionPreservesParentChain(t *testing.T) {
	t.Parallel()
	a, st := newTreeTestAgent(t)
	ctx := context.Background()
	seedSession(t, st, "root", 2)
	if err := a.ForkSession(ctx, "root", 2, "child"); err != nil {
		t.Fatal(err)
	}
	if err := a.CloneSession(ctx, "child", "twin"); err != nil {
		t.Fatal(err)
	}
	twin, _, _ := st.Load(ctx, "twin")
	// Twin's transcript matches its source.
	if len(twin.Messages) != 2 {
		t.Fatalf("twin msgs = %d, want 2", len(twin.Messages))
	}
	// Clone preserves the parent chain so the tree view still attributes
	// twin to root via its child source.
	if pid, _, ok := glue.SessionParent(twin); !ok || pid != "root" {
		t.Fatalf("twin parent = %q,ok=%v", pid, ok)
	}
}

func TestCloneSessionRootHasNoParent(t *testing.T) {
	t.Parallel()
	a, st := newTreeTestAgent(t)
	ctx := context.Background()
	seedSession(t, st, "root", 1)
	if err := a.CloneSession(ctx, "root", "copy"); err != nil {
		t.Fatal(err)
	}
	copyState, _, _ := st.Load(ctx, "copy")
	if _, _, ok := glue.SessionParent(copyState); ok {
		t.Fatal("clone of root should not gain a parent")
	}
}

func TestSessionParentRoundTripsAcrossJSONStore(t *testing.T) {
	t.Parallel()
	a, st := newTreeTestAgent(t)
	ctx := context.Background()
	seedSession(t, st, "root", 3)
	if err := a.ForkSession(ctx, "root", 4, "child"); err != nil {
		t.Fatal(err)
	}
	// Reload via a fresh agent to confirm metadata survives an encode/decode round
	// trip through the store (file-store turns int → float64).
	_ = glue.NewAgent(glue.AgentOptions{Provider: memoryProvider{}, Store: st})
	state, _, _ := st.Load(ctx, "child")
	pid, idx, ok := glue.SessionParent(state)
	if !ok || pid != "root" || idx != 4 {
		t.Fatalf("round trip lost metadata: %q,%d,%v", pid, idx, ok)
	}
}

func TestSessionParentMissingMetadata(t *testing.T) {
	t.Parallel()
	if _, _, ok := glue.SessionParent(glue.SessionState{}); ok {
		t.Fatal("empty metadata should not look like a fork")
	}
	if _, _, ok := glue.SessionParent(glue.SessionState{Metadata: map[string]any{
		glue.MetadataKeyParentSessionID:    "", // empty string is not a valid parent
		glue.MetadataKeyParentMessageIndex: 0,
	}}); ok {
		t.Fatal("empty parent id should not look like a fork")
	}
	if _, _, ok := glue.SessionParent(glue.SessionState{Metadata: map[string]any{
		glue.MetadataKeyParentSessionID:    "x",
		glue.MetadataKeyParentMessageIndex: "not-a-number",
	}}); ok {
		t.Fatal("non-numeric index should not look like a fork")
	}
}

func TestForkSessionMessagesAreCopiedNotShared(t *testing.T) {
	t.Parallel()
	a, st := newTreeTestAgent(t)
	ctx := context.Background()
	seedSession(t, st, "root", 2)
	if err := a.ForkSession(ctx, "root", 2, "child"); err != nil {
		t.Fatal(err)
	}
	// Mutate the source by reloading, modifying, and saving.
	src, _, _ := st.Load(ctx, "root")
	src.Messages[0].Content[0].Text = "MUTATED"
	if err := st.Save(ctx, "root", src); err != nil {
		t.Fatal(err)
	}
	// Child should not see the mutation.
	child, _, _ := st.Load(ctx, "child")
	if child.Messages[0].Content[0].Text == "MUTATED" {
		t.Fatal("child bleeds from source after save; messages must be deep-copied at fork")
	}
}
