package glue

import (
	"context"
	"errors"
	"sync"
	"time"
)

const defaultSessionID = "default"

// AgentOptions configures a Glue agent.
//
// Provider, Model, SystemPrompt, Tools, Options, MaxTurns, and Store are
// wired. WorkDir, Role, and Roles are reserved for follow-up issues:
//
//   - WorkDir: AGENTS.md context and Markdown skill discovery (#13).
//   - Role / Roles: scoped role instructions and per-role models (#14).
//
// Store, when set, persists session state across processes. The default
// in-memory behavior is preserved when Store is nil.
type AgentOptions struct {
	Provider     Provider
	Model        string
	SystemPrompt string
	Tools        []Tool
	Options      map[string]any
	MaxTurns     int
	Store        Store

	// WorkDir is reserved for #13.
	WorkDir string
	// Role is reserved for #14.
	Role string
	// Roles is reserved for #14.
	Roles map[string]any
}

// Agent owns shared configuration and an in-memory session map.
type Agent struct {
	provider     Provider
	model        string
	systemPrompt string
	tools        []Tool
	options      map[string]any
	maxTurns     int
	store        Store

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewAgent creates an agent. When [AgentOptions.Store] is set, sessions are
// loaded from and saved to that store; otherwise sessions are in-memory.
// The Provider must be supplied for [Session.Prompt] to succeed.
func NewAgent(options AgentOptions) *Agent {
	return &Agent{
		provider:     options.Provider,
		model:        options.Model,
		systemPrompt: options.SystemPrompt,
		tools:        cloneTools(options.Tools),
		options:      cloneMap(options.Options),
		maxTurns:     options.MaxTurns,
		store:        options.Store,
		sessions:     make(map[string]*Session),
	}
}

// Session returns an existing session by id, or creates a new one. When the
// agent has a configured [Store], an existing on-disk state is loaded into
// the new in-memory session. Empty ids resolve to the default session
// ("default").
func (a *Agent) Session(ctx context.Context, id string) (*Session, error) {
	if a == nil {
		return nil, errors.New("glue: nil agent")
	}
	if id == "" {
		id = defaultSessionID
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if existing := a.sessions[id]; existing != nil {
		return existing, nil
	}

	state, found, err := a.loadSessionState(ctx, id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	session := &Session{
		id:          id,
		agent:       a,
		messages:    cloneMessages(state.Messages),
		metadata:    cloneMap(state.Metadata),
		createdAt:   state.CreatedAt,
		updatedAt:   state.UpdatedAt,
		subscribers: make(map[int]func(Event)),
	}
	if !found {
		session.createdAt = now
		session.updatedAt = now
	}
	a.sessions[id] = session
	return session, nil
}

func (a *Agent) loadSessionState(ctx context.Context, id string) (SessionState, bool, error) {
	if a.store == nil {
		return SessionState{Version: SessionStateVersion, ID: id}, false, nil
	}
	state, found, err := a.store.Load(ctx, id)
	if err != nil {
		return SessionState{}, false, err
	}
	if !found {
		return SessionState{Version: SessionStateVersion, ID: id}, false, nil
	}
	if state.ID == "" {
		state.ID = id
	}
	if state.Version == 0 {
		state.Version = SessionStateVersion
	}
	return state, true, nil
}

func cloneTools(tools []Tool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]Tool, len(tools))
	copy(out, tools)
	for i := range out {
		if len(out[i].Parameters) > 0 {
			out[i].Parameters = append(out[i].Parameters[:0:0], out[i].Parameters...)
		}
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
