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
// All fields are wired. When WorkDir is set, the agent loads
// `<WorkDir>/AGENTS.md` (non-fatal if missing), discovers Markdown skills
// under `<WorkDir>/.agents/skills/<name>/SKILL.md`, and discovers Markdown
// roles under `<WorkDir>/roles/*.md`. Programmatic entries supplied via
// Skills and Roles are merged with the on-disk catalog; programmatic
// entries win on name collision.
//
// Role is the agent-default role applied to every prompt unless overridden
// at session or call level. Effective role precedence is
// call ([WithRole]) > session ([WithSessionRole]) > agent (Role).
// Effective model precedence is call ([WithModel]) > effective role's
// Model > Model.
type AgentOptions struct {
	Provider     Provider
	Model        string
	SystemPrompt string
	Tools        []Tool
	Options      map[string]any
	MaxTurns     int
	Store        Store
	WorkDir      string
	Skills       map[string]Skill
	Roles        []Role
	Role         string
	Permission   Permission
	Hooks        []Hook

	// Compactor, if non-nil and CompactionThreshold > 0, is invoked
	// before every prompt whenever the in-memory transcript has more
	// than CompactionThreshold messages. The compactor's output replaces
	// the in-memory transcript before [loop.Run] is called and is
	// persisted by the next save.
	Compactor           Compactor
	CompactionThreshold int
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
	workDir      string
	role         string
	permission   Permission
	hooks        []Hook

	compactor           Compactor
	compactionThreshold int

	agentsMD      string
	skills        map[string]Skill
	roles         map[string]Role
	contextLoaded bool

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewAgent creates an agent. When [AgentOptions.Store] is set, sessions are
// loaded from and saved to that store; otherwise sessions are in-memory.
// The Provider must be supplied for [Session.Prompt] to succeed.
func NewAgent(options AgentOptions) *Agent {
	return &Agent{
		provider:            options.Provider,
		model:               options.Model,
		systemPrompt:        options.SystemPrompt,
		tools:               cloneTools(options.Tools),
		options:             cloneMap(options.Options),
		maxTurns:            options.MaxTurns,
		store:               options.Store,
		workDir:             options.WorkDir,
		skills:              cloneSkills(options.Skills),
		roles:               rolesFromSlice(options.Roles),
		role:                options.Role,
		permission:          options.Permission,
		hooks:               cloneHooks(options.Hooks),
		compactor:           options.Compactor,
		compactionThreshold: options.CompactionThreshold,
		sessions:            make(map[string]*Session),
	}
}

// ListSessions returns a metadata-only catalog of stored sessions if
// the agent's [Store] implements [SessionLister]. Returns
// [ErrSessionListingNotSupported] when the store does not.
//
// Mirrors [Agent.SearchSessions] in shape: provider-free, no transcript
// content, intended for picker UIs and dashboards.
func (a *Agent) ListSessions(ctx context.Context, opts ListSessionsOptions) ([]SessionSummary, error) {
	if a == nil {
		return nil, errors.New("glue: nil agent")
	}
	lister, ok := a.store.(SessionLister)
	if !ok || a.store == nil {
		return nil, ErrSessionListingNotSupported
	}
	return lister.ListSessions(ctx, opts)
}

// ToolCatalog returns a cloned provider-visible catalog of the agent's
// configured tools, including permission metadata for hosts that need to
// display or expose the tool surface without starting a session.
func (a *Agent) ToolCatalog() []ToolSpec {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tools := cloneTools(a.tools)
	if len(tools) == 0 {
		return nil
	}
	specs := make([]ToolSpec, len(tools))
	for i, tool := range tools {
		specs[i] = tool.ToolSpec
	}
	return specs
}

func rolesFromSlice(roles []Role) map[string]Role {
	if len(roles) == 0 {
		return nil
	}
	out := make(map[string]Role, len(roles))
	for _, r := range roles {
		out[r.Name] = r
	}
	return out
}

// SessionOption configures a session at creation time.
type SessionOption func(*sessionConfig)

type sessionConfig struct {
	role string
}

// WithSessionRole sets the session-default role used when no per-call
// [WithRole] is provided.
func WithSessionRole(role string) SessionOption {
	return func(c *sessionConfig) { c.role = role }
}

// Session returns an existing session by id, or creates a new one. When the
// agent has a configured [Store], an existing on-disk state is loaded into
// the new in-memory session. Empty ids resolve to the default session
// ("default").
func (a *Agent) Session(ctx context.Context, id string, options ...SessionOption) (*Session, error) {
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

	if err := a.ensureContextLoaded(); err != nil {
		return nil, err
	}

	cfg := sessionConfig{}
	for _, opt := range options {
		if opt != nil {
			opt(&cfg)
		}
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
		role:        cfg.role,
		subscribers: make(map[int]func(Event)),
	}
	if !found {
		session.createdAt = now
		session.updatedAt = now
	}
	a.sessions[id] = session
	return session, nil
}

func (a *Agent) ensureContextLoaded() error {
	if a.contextLoaded {
		return nil
	}
	if a.workDir != "" {
		loaded, err := LoadContext(a.workDir)
		if err != nil {
			return err
		}
		a.agentsMD = loaded.AgentsMD
		if len(loaded.Skills) > 0 {
			if a.skills == nil {
				a.skills = map[string]Skill{}
			}
			for name, skill := range loaded.Skills {
				if _, exists := a.skills[name]; exists {
					// programmatic entries win on collision
					continue
				}
				a.skills[name] = skill
			}
		}
		if len(loaded.Roles) > 0 {
			if a.roles == nil {
				a.roles = map[string]Role{}
			}
			for name, role := range loaded.Roles {
				if _, exists := a.roles[name]; exists {
					continue
				}
				a.roles[name] = role
			}
		}
	}
	a.contextLoaded = true
	return nil
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

func cloneHooks(in []Hook) []Hook {
	if len(in) == 0 {
		return nil
	}
	out := make([]Hook, len(in))
	copy(out, in)
	return out
}
