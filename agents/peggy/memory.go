package peggy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/erain/glue"
)

// MemoriesSessionID is the session id under which Peggy persists
// curated memories — facts the model decided are worth keeping. The
// session is searchable through the same FTS5 index as everything
// else, but isolating it under this id gives Recall an easy way to
// scope, and protects memories from the SummarizingCompactor (which
// only runs on whatever session is being prompted).
const MemoriesSessionID = "__memories__"

// DefaultMemoryHint is appended to the system prompt when memory
// tools are registered, so the model knows the surface exists. Short
// on purpose — heavy instructions belong in SOUL.md.
const DefaultMemoryHint = `
You have two tools for durable memory:

- remember(content, tags?) — call this when the user shares a fact
  worth keeping across sessions (their name, a preference, a pet, a
  project name, etc). Phrase the content as a self-contained
  statement in third person ("the user prefers …"). Do not over-call;
  small-talk and one-off context are not memories.

- recall(query, limit?, only_memories?) — call this when answering a
  question that benefits from prior context you don't have in this
  session. Hits include the source session id and a snippet; cite
  them naturally in your answer when relevant.
`

// Memory is a single curated memory record.
type Memory struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// RecallOptions configures a Recall call.
type RecallOptions struct {
	Limit        int
	OnlyMemories bool
}

// RecallOption is a functional option for Peggy.Recall.
type RecallOption func(*RecallOptions)

// WithRecallLimit overrides the default limit (5).
func WithRecallLimit(n int) RecallOption {
	return func(o *RecallOptions) { o.Limit = n }
}

// WithMemoriesOnly restricts Recall to the dedicated __memories__
// session — useful when the model wants curated facts rather than
// the full conversational history.
func WithMemoriesOnly() RecallOption {
	return func(o *RecallOptions) { o.OnlyMemories = true }
}

const (
	defaultRecallLimit = 5
	maxRecallLimit     = 20
)

var memoryWriteMu sync.Mutex

// AddMemory appends a curated memory to the __memories__ session.
//
// Implementation note: we drive the underlying Store directly rather
// than going through agent.Session(__memories__).Prompt — that route
// would register an in-memory *Session in the agent map and conflict
// with any concurrent ordinary prompt traffic. The Store's atomic
// write semantics are sufficient (sqlite serializes via
// MaxOpenConns=1; file does temp+rename).
func (p *Peggy) AddMemory(ctx context.Context, content string, tags []string) (Memory, error) {
	if p == nil || p.store == nil {
		return Memory{}, errors.New("peggy: AddMemory: not initialised")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return Memory{}, errors.New("peggy: AddMemory: content is required")
	}
	memoryWriteMu.Lock()
	defer memoryWriteMu.Unlock()

	state, _, err := p.store.Load(ctx, MemoriesSessionID)
	if err != nil {
		return Memory{}, fmt.Errorf("peggy: load memories: %w", err)
	}
	now := time.Now().UTC()
	if state.ID == "" {
		state.ID = MemoriesSessionID
		state.Version = glue.SessionStateVersion
		state.CreatedAt = now
	}
	tagsCopy := append([]string(nil), tags...)
	id := memoryID(now, content)
	state.Messages = append(state.Messages, glue.Message{
		Role:    glue.MessageRoleAssistant,
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: content}},
		Metadata: map[string]any{
			"id":     id,
			"memory": true,
			"tags":   tagsCopy,
		},
		CreatedAt: now,
	})
	state.UpdatedAt = now
	if err := p.store.Save(ctx, MemoriesSessionID, state); err != nil {
		return Memory{}, fmt.Errorf("peggy: save memories: %w", err)
	}
	return Memory{ID: id, Content: content, Tags: tagsCopy, Timestamp: now}, nil
}

// ListMemories returns the curated memory list, newest first. Useful
// for debugging, tests, and a future `peggy memories` subcommand.
func (p *Peggy) ListMemories(ctx context.Context) ([]Memory, error) {
	if p == nil || p.store == nil {
		return nil, errors.New("peggy: ListMemories: not initialised")
	}
	state, _, err := p.store.Load(ctx, MemoriesSessionID)
	if err != nil {
		return nil, err
	}
	out := make([]Memory, 0, len(state.Messages))
	for _, m := range state.Messages {
		mem, ok := memoryFromMessage(m)
		if !ok {
			continue
		}
		out = append(out, mem)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out, nil
}

// ForgetMemory deletes one curated memory by id from the __memories__
// session and returns the removed memory.
func (p *Peggy) ForgetMemory(ctx context.Context, id string) (Memory, error) {
	if p == nil || p.store == nil {
		return Memory{}, errors.New("peggy: ForgetMemory: not initialised")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return Memory{}, errors.New("peggy: ForgetMemory: id is required")
	}
	memoryWriteMu.Lock()
	defer memoryWriteMu.Unlock()

	state, _, err := p.store.Load(ctx, MemoriesSessionID)
	if err != nil {
		return Memory{}, fmt.Errorf("peggy: load memories: %w", err)
	}
	var removed Memory
	found := false
	kept := state.Messages[:0]
	for _, m := range state.Messages {
		mem, ok := memoryFromMessage(m)
		if ok && mem.ID == id && !found {
			removed = mem
			found = true
			continue
		}
		kept = append(kept, m)
	}
	if !found {
		return Memory{}, fmt.Errorf("peggy: memory %q not found", id)
	}
	state.Messages = kept
	state.UpdatedAt = time.Now().UTC()
	if err := p.store.Save(ctx, MemoriesSessionID, state); err != nil {
		return Memory{}, fmt.Errorf("peggy: save memories: %w", err)
	}
	return removed, nil
}

func memoryFromMessage(m glue.Message) (Memory, bool) {
	if m.Role != glue.MessageRoleAssistant {
		return Memory{}, false
	}
	mem := Memory{Timestamp: m.CreatedAt}
	for _, p := range m.Content {
		if p.Type == glue.ContentTypeText {
			if mem.Content != "" {
				mem.Content += "\n"
			}
			mem.Content += p.Text
		}
	}
	mem.Content = strings.TrimSpace(mem.Content)
	if mem.Content == "" {
		return Memory{}, false
	}
	if id, ok := m.Metadata["id"].(string); ok {
		mem.ID = strings.TrimSpace(id)
	}
	if mem.ID == "" {
		mem.ID = memoryID(mem.Timestamp, mem.Content)
	}
	if tags, ok := m.Metadata["tags"].([]string); ok {
		mem.Tags = append([]string(nil), tags...)
	} else if anyTags, ok := m.Metadata["tags"].([]any); ok {
		for _, t := range anyTags {
			if s, ok := t.(string); ok {
				mem.Tags = append(mem.Tags, s)
			}
		}
	}
	return mem, true
}

func memoryID(timestamp time.Time, content string) string {
	content = strings.TrimSpace(content)
	stamp := timestamp.UTC().Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(stamp + "\n" + content))
	return fmt.Sprintf("mem_%d_%s", timestamp.UTC().UnixNano(), hex.EncodeToString(sum[:4]))
}

// Recall searches Peggy's history (everything by default; restrict
// to curated memories with WithMemoriesOnly()).
func (p *Peggy) Recall(ctx context.Context, query string, opts ...RecallOption) ([]glue.SearchHit, error) {
	if p == nil || p.agent == nil {
		return nil, errors.New("peggy: Recall: not initialised")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("peggy: Recall: query is required")
	}
	o := RecallOptions{Limit: defaultRecallLimit}
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	if o.Limit <= 0 {
		o.Limit = defaultRecallLimit
	}
	if o.Limit > maxRecallLimit {
		o.Limit = maxRecallLimit
	}
	gluOpts := []glue.SearchOption{glue.WithLimit(o.Limit)}
	if o.OnlyMemories {
		gluOpts = append(gluOpts, glue.WithSessionID(MemoriesSessionID))
	}
	return p.agent.SearchSessions(ctx, query, gluOpts...)
}

// rememberArgs is the argument schema for the remember tool.
type rememberArgs struct {
	Content string   `json:"content"`
	Tags    []string `json:"tags,omitempty"`
}

// recallArgs is the argument schema for the recall tool.
type recallArgs struct {
	Query        string `json:"query"`
	Limit        int    `json:"limit,omitempty"`
	OnlyMemories bool   `json:"only_memories,omitempty"`
}

// RememberTool returns a glue.Tool the model can call to persist a
// fact. Closes over the supplied *Peggy.
func RememberTool(p *Peggy) glue.Tool {
	return glue.NewTool[rememberArgs](
		glue.ToolSpec{
			Name:        "remember",
			Description: "Persist a single fact about the user to durable cross-session memory. Use sparingly: only when the user shares something worth remembering across many future conversations (name, preferences, people, projects, pets, recurring decisions). Phrase content in third person.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "content": {"type": "string", "description": "The fact, as a one- or two-sentence statement in third person (e.g. 'The user's Australian Shepherd is named Inkblot.')."},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "Optional categorization tags (e.g. 'pet', 'preference', 'person', 'project'). Helpful for later filtering."}
  },
  "required": ["content"]
}`),
		},
		func(ctx context.Context, args rememberArgs) (glue.ToolResult, error) {
			content := strings.TrimSpace(args.Content)
			if content == "" {
				return glue.ErrorResult(errors.New("remember: content is required")), nil
			}
			mem, err := p.AddMemory(ctx, content, args.Tags)
			if err != nil {
				return glue.ErrorResult(err), nil
			}
			return glue.TextResult(fmt.Sprintf("Remembered: %q (tags=%v) at %s", mem.Content, mem.Tags, mem.Timestamp.Format(time.RFC3339))), nil
		},
	)
}

// RecallTool returns a glue.Tool the model can call to search prior
// context. Closes over the supplied *Peggy.
func RecallTool(p *Peggy) glue.Tool {
	return glue.NewTool[recallArgs](
		glue.ToolSpec{
			Name:        "recall",
			Description: "Search prior conversational history and curated memories. Returns up to `limit` snippets with their source session id. Use this when answering questions that depend on facts you don't have in the current turn's context.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Search query. FTS5 syntax: bare words match exact words, \"quoted phrases\" match a phrase, AND / OR / NOT combine terms. Keep it short."},
    "limit": {"type": "integer", "description": "Max snippets to return. Default 5, max 20.", "default": 5},
    "only_memories": {"type": "boolean", "description": "When true, restrict to the curated memory list (results from explicit remember() calls). Default false (searches everything).", "default": false}
  },
  "required": ["query"]
}`),
		},
		func(ctx context.Context, args recallArgs) (glue.ToolResult, error) {
			query := strings.TrimSpace(args.Query)
			if query == "" {
				return glue.ErrorResult(errors.New("recall: query is required")), nil
			}
			var opts []RecallOption
			if args.Limit > 0 {
				opts = append(opts, WithRecallLimit(args.Limit))
			}
			if args.OnlyMemories {
				opts = append(opts, WithMemoriesOnly())
			}
			hits, err := p.Recall(ctx, query, opts...)
			if err != nil {
				if errors.Is(err, glue.ErrSearchNotSupported) {
					return glue.ErrorResult(fmt.Errorf("recall: configured store does not support search; use a sqlite store")), nil
				}
				return glue.ErrorResult(err), nil
			}
			return glue.TextResult(formatRecallHits(hits)), nil
		},
	)
}

// formatRecallHits renders search hits as a numbered text block the
// model can read inline. One line per hit, source first then a
// truncated snippet.
func formatRecallHits(hits []glue.SearchHit) string {
	if len(hits) == 0 {
		return "No hits."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d hits:\n", len(hits))
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. [%s#%d %s] (score=%.2f) %s\n",
			i+1, h.SessionID, h.Index, h.Timestamp.Format("2006-01-02"), h.Score, strings.ReplaceAll(h.Snippet, "\n", " "))
	}
	return strings.TrimRight(b.String(), "\n")
}
