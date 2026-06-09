package glue

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// memStore is an in-memory glue.Store for testing without the file store.
type memStore struct {
	mu     sync.Mutex
	states map[string]SessionState
	saves  int
	loads  int
}

func newMemStore() *memStore { return &memStore{states: map[string]SessionState{}} }

func (m *memStore) Load(_ context.Context, id string) (SessionState, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loads++
	state, ok := m.states[id]
	if !ok {
		return SessionState{}, false, nil
	}
	return state, true, nil
}

func (m *memStore) Save(_ context.Context, id string, state SessionState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	m.states[id] = state
	return nil
}

func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, id)
	return nil
}

func TestSessionPromptSavesViaStore(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("hello")}}
	agent := NewAgent(AgentOptions{Provider: provider, Store: store})

	session, err := agent.Session(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if store.saves != 1 {
		t.Fatalf("saves = %d, want 1", store.saves)
	}
	state := store.states["dev"]
	if state.Version != SessionStateVersion || state.ID != "dev" {
		t.Fatalf("state = %#v, want version %d id dev", state, SessionStateVersion)
	}
	if len(state.Messages) != 2 {
		t.Fatalf("state messages = %d, want 2 (user + assistant)", len(state.Messages))
	}
}

func TestSessionContinuesAcrossAgentInstancesWithStore(t *testing.T) {
	t.Parallel()

	store := newMemStore()

	// First "process": run a prompt through one Agent, save state.
	{
		provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("hello")}}
		agent := NewAgent(AgentOptions{Provider: provider, Store: store})
		session, err := agent.Session(context.Background(), "dev")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.Prompt(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
	}

	// Second "process": brand-new Agent, same store, same id.
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("again")}}
	agent := NewAgent(AgentOptions{Provider: provider, Store: store})
	session, err := agent.Session(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Messages(); len(got) != 2 {
		t.Fatalf("Messages = %d, want 2 loaded from store", len(got))
	}

	res, err := session.Prompt(context.Background(), "continue")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "again" {
		t.Fatalf("Text = %q, want again", res.Text)
	}

	// Provider should now have been called once and seen 3 messages
	// (loaded user/assistant + new user).
	if provider.calls != 1 {
		t.Fatalf("calls = %d, want 1 in second process", provider.calls)
	}
	wantRoles := []MessageRole{MessageRoleUser, MessageRoleAssistant, MessageRoleUser}
	got := provider.requests[0].Messages
	if len(got) != len(wantRoles) {
		t.Fatalf("provider saw %d messages, want %d", len(got), len(wantRoles))
	}
	for i, r := range wantRoles {
		if got[i].Role != r {
			t.Fatalf("messages[%d].Role = %q, want %q", i, got[i].Role, r)
		}
	}
	if len(session.Messages()) != 4 {
		t.Fatalf("transcript = %d, want 4 (u/a/u/a)", len(session.Messages()))
	}
	if store.saves != 2 {
		t.Fatalf("saves = %d, want 2 across the two prompts", store.saves)
	}
}

type savingErrStore struct{ err error }

func (s savingErrStore) Load(context.Context, string) (SessionState, bool, error) {
	return SessionState{}, false, nil
}
func (s savingErrStore) Save(context.Context, string, SessionState) error { return s.err }
func (savingErrStore) Delete(context.Context, string) error               { return nil }

func TestSessionPromptSurfacesSaveError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("disk full")
	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider, Store: savingErrStore{err: wantErr}})
	session, _ := agent.Session(context.Background(), "x")

	_, err := session.Prompt(context.Background(), "hi")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want disk full", err)
	}
}

func TestSessionStateSnapshot(t *testing.T) {
	t.Parallel()

	provider := &recordingProvider{turns: [][]ProviderEvent{textTurn("ok")}}
	agent := NewAgent(AgentOptions{Provider: provider})
	session, _ := agent.Session(context.Background(), "x")
	if _, err := session.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	state := session.State()
	if state.ID != "x" || state.Version != SessionStateVersion {
		t.Fatalf("state = %#v, want id x version %d", state, SessionStateVersion)
	}
	if len(state.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(state.Messages))
	}
}
