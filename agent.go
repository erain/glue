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
// Provider, Model, SystemPrompt, Tools, Options, and MaxTurns are wired in
// this issue. Store, WorkDir, Role, and Roles are reserved for follow-up
// issues and currently have no effect:
//
//   - Store: file-backed session persistence (#11).
//   - WorkDir: AGENTS.md context and Markdown skill discovery (#13).
//   - Role / Roles: scoped role instructions and per-role models (#14).
//
// They are present so the public type stays stable as those features land.
type AgentOptions struct {
	Provider     Provider
	Model        string
	SystemPrompt string
	Tools        []Tool
	Options      map[string]any
	MaxTurns     int

	// Store is reserved for #11.
	Store any
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

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewAgent creates an agent with in-memory session state. The Provider must
// be supplied for [Session.Prompt] to succeed.
func NewAgent(options AgentOptions) *Agent {
	return &Agent{
		provider:     options.Provider,
		model:        options.Model,
		systemPrompt: options.SystemPrompt,
		tools:        cloneTools(options.Tools),
		options:      cloneMap(options.Options),
		maxTurns:     options.MaxTurns,
		sessions:     make(map[string]*Session),
	}
}

// Session returns an existing in-memory session by id or creates a new one.
// Empty ids resolve to the default session ("default").
func (a *Agent) Session(_ context.Context, id string) (*Session, error) {
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
	now := time.Now().UTC()
	session := &Session{
		id:          id,
		agent:       a,
		createdAt:   now,
		updatedAt:   now,
		subscribers: make(map[int]func(Event)),
	}
	a.sessions[id] = session
	return session, nil
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
