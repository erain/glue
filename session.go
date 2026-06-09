package glue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/erain/glue/loop"
)

// Session is an in-memory conversation with an [Agent]. Sessions are
// goroutine-safe but a single session executes one [Session.Prompt] at a
// time.
type Session struct {
	id    string
	agent *Agent

	runMu sync.Mutex
	mu    sync.Mutex

	messages  []Message
	metadata  map[string]any
	createdAt time.Time
	updatedAt time.Time
	role      string

	nextSubscriberID int
	subscribers      map[int]func(Event)
}

// ID returns the session id.
func (s *Session) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

// Messages returns a snapshot of the session transcript.
func (s *Session) Messages() []Message {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneMessages(s.messages)
}

// Subscribe registers a session-scoped event handler that receives every
// loop event for every prompt run on this session. The returned function
// removes the handler.
func (s *Session) Subscribe(handler func(Event)) func() {
	if s == nil || handler == nil {
		return func() {}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSubscriberID
	s.nextSubscriberID++
	s.subscribers[id] = handler
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subscribers, id)
	}
}

// PromptResult is returned by [Session.Prompt].
type PromptResult struct {
	// Text concatenates all text parts of the final assistant message.
	Text string
	// Message is the final assistant message of this run, cloned. Nil if no
	// assistant message was produced.
	Message *Message
	// NewMessages contains every message produced during this run, in append
	// order (assistant + tool messages, possibly across multiple turns).
	NewMessages []Message
	// Messages is a snapshot of the full session transcript after this run.
	Messages []Message
}

// PromptOption configures one [Session.Prompt] invocation.
type PromptOption func(*promptConfig)

type promptConfig struct {
	model         string
	systemPrompt  string
	tools         []Tool
	options       map[string]any
	maxTurns      int
	emit          func(Event)
	auxEmits      []func(Event)
	jsonSchema    any
	role          string
	modelSet      bool
	permission    Permission
	permissionSet bool
}

// WithModel overrides the model for one prompt. When set, this beats
// any role's Model and the agent's Model.
func WithModel(model string) PromptOption {
	return func(c *promptConfig) {
		c.model = model
		c.modelSet = true
	}
}

// WithRole overrides the effective role for one prompt. The named role
// must exist in [AgentOptions.Roles] or the loaded WorkDir context, or
// the prompt fails with a typed error.
func WithRole(role string) PromptOption {
	return func(c *promptConfig) { c.role = role }
}

// WithSystemPrompt overrides the agent system prompt for one prompt.
func WithSystemPrompt(systemPrompt string) PromptOption {
	return func(c *promptConfig) { c.systemPrompt = systemPrompt }
}

// WithTools replaces the agent tools for one prompt.
func WithTools(tools []Tool) PromptOption {
	return func(c *promptConfig) { c.tools = cloneTools(tools) }
}

// WithProviderOptions replaces provider options for one prompt.
func WithProviderOptions(options map[string]any) PromptOption {
	return func(c *promptConfig) { c.options = cloneMap(options) }
}

// WithMaxTurns overrides the loop max-turn guard for one prompt.
func WithMaxTurns(maxTurns int) PromptOption {
	return func(c *promptConfig) { c.maxTurns = maxTurns }
}

// WithPermission overrides the agent's Permission implementation for one
// prompt. Passing nil explicitly disables permission handling for that
// prompt, so side-effecting tools are denied by the loop.
func WithPermission(permission Permission) PromptOption {
	return func(c *promptConfig) {
		c.permission = permission
		c.permissionSet = true
	}
}

// WithEvents registers a per-prompt event handler. It receives every loop
// event for that prompt in addition to any session-scoped subscribers
// installed via [Session.Subscribe].
func WithEvents(handler func(Event)) PromptOption {
	return func(c *promptConfig) { c.emit = handler }
}

// WithJSONSchema attaches a JSON Schema for [Session.PromptJSON]. The
// schema may be passed as a Go value (map / struct), a json.RawMessage, a
// []byte, or a string; bytes/string forms are JSON-decoded once. When the
// active provider supports structured output, the schema is forwarded as
// the provider's structured-response config (Gemini: `response_json_schema`).
func WithJSONSchema(schema any) PromptOption {
	return func(c *promptConfig) { c.jsonSchema = schema }
}

// Prompt sends a user message through the agent loop and stores the
// resulting transcript in memory.
func (s *Session) Prompt(ctx context.Context, text string, options ...PromptOption) (PromptResult, error) {
	if s == nil {
		return PromptResult{}, errors.New("glue: nil session")
	}
	if s.agent == nil {
		return PromptResult{}, errors.New("glue: session has no agent")
	}
	if s.agent.provider == nil {
		return PromptResult{}, errors.New("glue: agent provider is required")
	}

	s.runMu.Lock()
	defer s.runMu.Unlock()

	config, err := s.promptConfig(options)
	if err != nil {
		return PromptResult{}, err
	}
	userMessage := Message{
		Role:      MessageRoleUser,
		Content:   []ContentPart{{Type: ContentTypeText, Text: text}},
		CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	base := cloneMessages(s.messages)
	s.mu.Unlock()

	if compacted, did, err := s.maybeCompact(ctx, base); err != nil {
		return PromptResult{}, err
	} else if did {
		s.mu.Lock()
		s.messages = cloneMessages(compacted)
		s.updatedAt = time.Now().UTC()
		s.mu.Unlock()
		base = compacted
	}
	runMessages := append(base, userMessage)

	permission := s.agent.permission
	if config.permissionSet {
		permission = config.permission
	}

	run := func(messages []Message) (loop.RunResult, error) {
		return loop.Run(ctx, loop.RunRequest{
			Provider:     s.agent.provider,
			Model:        config.model,
			SystemPrompt: config.systemPrompt,
			Messages:     messages,
			Tools:        config.tools,
			Options:      config.options,
			MaxTurns:     config.maxTurns,
			SessionID:    s.id,
			Permission:   permission,
			Hooks:        cloneHooks(s.agent.hooks),
			AutoContinue: s.agent.autoContinue,
			Emit: func(event Event) {
				if config.emit != nil {
					config.emit(event)
				}
				for _, aux := range config.auxEmits {
					aux(event)
				}
				s.dispatchEvent(event)
			},
		})
	}

	result, runErr := run(runMessages)

	// Context overflow is not retryable as-is: compact once (ignoring
	// the size threshold — the provider just told us we are over) and
	// retry once. Without a compactor the overflow surfaces unchanged.
	var overflow *loop.OverflowError
	if errors.As(runErr, &overflow) && s.agent.compactor != nil {
		compacted, err := s.agent.compactor.Compact(ctx, cloneMessages(base))
		if err == nil && len(compacted) > 0 && len(compacted) < len(base) {
			s.mu.Lock()
			s.messages = cloneMessages(compacted)
			s.updatedAt = time.Now().UTC()
			s.mu.Unlock()
			base = compacted
			result, runErr = run(append(base, userMessage))
		}
	}

	s.mu.Lock()
	s.messages = append(s.messages, userMessage)
	s.messages = append(s.messages, result.NewMessages...)
	s.updatedAt = time.Now().UTC()
	transcript := cloneMessages(s.messages)
	state := s.stateLocked()
	s.mu.Unlock()

	saveErr := s.save(ctx, state)

	response := PromptResult{
		Text:        assistantText(result.NewMessages),
		Message:     lastAssistantMessage(result.NewMessages),
		NewMessages: cloneMessages(result.NewMessages),
		Messages:    transcript,
	}
	if runErr != nil && saveErr != nil {
		return response, errors.Join(runErr, saveErr)
	}
	if runErr != nil {
		return response, runErr
	}
	return response, saveErr
}

// State returns a snapshot of the durable session state.
func (s *Session) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *Session) stateLocked() SessionState {
	return SessionState{
		Version:   SessionStateVersion,
		ID:        s.id,
		Messages:  cloneMessages(s.messages),
		Metadata:  cloneMap(s.metadata),
		CreatedAt: s.createdAt,
		UpdatedAt: s.updatedAt,
	}
}

// maybeCompact runs the agent's configured Compactor against the current
// transcript when the message count exceeds the configured threshold. It
// returns the (possibly new) transcript, a flag indicating whether a
// compaction actually replaced messages, and any error.
func (s *Session) maybeCompact(ctx context.Context, messages []Message) ([]Message, bool, error) {
	if s.agent.compactor == nil || s.agent.compactionThreshold <= 0 {
		return messages, false, nil
	}
	if len(messages) <= s.agent.compactionThreshold {
		return messages, false, nil
	}
	out, err := s.agent.compactor.Compact(ctx, cloneMessages(messages))
	if err != nil {
		return nil, false, err
	}
	if len(out) == len(messages) {
		// No-op compactor; do not invalidate persistence with a no-op
		// rewrite.
		return messages, false, nil
	}
	return out, true, nil
}

func (s *Session) save(ctx context.Context, state SessionState) error {
	if s.agent == nil || s.agent.store == nil {
		return nil
	}
	return s.agent.store.Save(ctx, s.id, state)
}

// PromptJSON sends a prompt and decodes the assistant's final text into
// outPtr, which must be a non-nil pointer. It augments the user prompt with
// JSON-only instructions so non-Gemini providers can still produce
// parseable output, and sets `response_mime_type: application/json` on the
// provider request. When [WithJSONSchema] is provided, the schema is also
// forwarded as `response_json_schema`.
//
// V1 validation is intentionally limited to JSON decoding into the
// caller's Go type; full JSON Schema validation is out of scope.
func (s *Session) PromptJSON(ctx context.Context, text string, outPtr any, options ...PromptOption) (PromptResult, error) {
	if err := validateJSONOutputTarget(outPtr); err != nil {
		return PromptResult{}, err
	}

	var probe promptConfig
	for _, opt := range options {
		if opt != nil {
			opt(&probe)
		}
	}
	schema, err := normalizeJSONSchema(probe.jsonSchema)
	if err != nil {
		return PromptResult{}, err
	}

	providerOptions := cloneMap(probe.options)
	if providerOptions == nil {
		providerOptions = map[string]any{}
	}
	providerOptions["response_mime_type"] = "application/json"
	if schema != nil {
		providerOptions["response_json_schema"] = schema
	}

	merged := append([]PromptOption{}, options...)
	merged = append(merged, WithProviderOptions(providerOptions))

	result, err := s.Prompt(ctx, buildJSONPrompt(text, schema), merged...)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Text)), outPtr); err != nil {
		return result, fmt.Errorf("glue: decode JSON result: %w", err)
	}
	return result, nil
}

func validateJSONOutputTarget(out any) error {
	if out == nil {
		return errors.New("glue: PromptJSON output target is nil")
	}
	value := reflect.ValueOf(out)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("glue: PromptJSON output target must be a non-nil pointer")
	}
	return nil
}

func normalizeJSONSchema(schema any) (any, error) {
	switch value := schema.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return decodeJSONSchemaBytes(value)
	case []byte:
		return decodeJSONSchemaBytes(value)
	case string:
		return decodeJSONSchemaBytes([]byte(value))
	default:
		return value, nil
	}
}

func decodeJSONSchemaBytes(data []byte) (any, error) {
	var schema any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("glue: invalid JSON schema: %w", err)
	}
	return schema, nil
}

func buildJSONPrompt(text string, schema any) string {
	var b strings.Builder
	b.WriteString(text)
	b.WriteString("\n\nRespond with only valid JSON. Do not include markdown fences, prose, or commentary.")
	if schema != nil {
		if data, err := json.MarshalIndent(schema, "", "  "); err == nil {
			b.WriteString("\n\nThe JSON must conform to this schema:\n")
			b.Write(data)
		}
	}
	return b.String()
}

func (s *Session) promptConfig(options []PromptOption) (promptConfig, error) {
	config := promptConfig{
		model:        s.agent.model,
		systemPrompt: composeSystemPrompt(s.agent.systemPrompt, s.agent.agentsMD, s.agent.skills),
		tools:        cloneTools(s.agent.tools),
		options:      cloneMap(s.agent.options),
		maxTurns:     s.agent.maxTurns,
		role:         s.agent.role,
	}
	if s.role != "" {
		config.role = s.role
	}
	for _, opt := range options {
		if opt != nil {
			opt(&config)
		}
	}
	if config.role != "" {
		role, ok := s.agent.roles[config.role]
		if !ok {
			return promptConfig{}, fmt.Errorf("glue: role %q not found", config.role)
		}
		config.systemPrompt = appendRoleToSystemPrompt(config.systemPrompt, role)
		if role.Model != "" && !config.modelSet {
			config.model = role.Model
		}
	}
	return config, nil
}

// Skill renders the named skill (looked up from [AgentOptions.Skills] or the
// agent's WorkDir context), appends args as JSON, and runs the result
// through [Session.Prompt]. Unknown skill names return a typed error.
func (s *Session) Skill(ctx context.Context, name string, args any, options ...PromptOption) (PromptResult, error) {
	if s == nil || s.agent == nil {
		return PromptResult{}, errors.New("glue: session has no agent")
	}
	skill, ok := s.agent.skills[name]
	if !ok {
		return PromptResult{}, fmt.Errorf("glue: skill %q not found", name)
	}
	prompt, err := buildSkillPrompt(skill, args)
	if err != nil {
		return PromptResult{}, err
	}
	return s.Prompt(ctx, prompt, options...)
}

func (s *Session) dispatchEvent(event Event) {
	s.mu.Lock()
	handlers := make([]func(Event), 0, len(s.subscribers))
	for _, h := range s.subscribers {
		handlers = append(handlers, h)
	}
	s.mu.Unlock()
	for _, h := range handlers {
		h(event)
	}
}

func cloneMessages(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]Message, len(messages))
	for i, m := range messages {
		out[i] = cloneMessage(m)
	}
	return out
}

func cloneMessage(m Message) Message {
	if len(m.Content) > 0 {
		c := make([]ContentPart, len(m.Content))
		copy(c, m.Content)
		for i := range c {
			if c[i].Image != nil {
				image := *c[i].Image
				c[i].Image = &image
			}
			if c[i].ToolCall != nil {
				tc := *c[i].ToolCall
				if len(tc.Arguments) > 0 {
					tc.Arguments = append(tc.Arguments[:0:0], tc.Arguments...)
				}
				c[i].ToolCall = &tc
			}
		}
		m.Content = c
	}
	if m.Usage != nil {
		usage := *m.Usage
		m.Usage = &usage
	}
	m.Metadata = cloneMap(m.Metadata)
	return m
}

func assistantText(messages []Message) string {
	msg := lastAssistantMessage(messages)
	if msg == nil {
		return ""
	}
	var text string
	for _, p := range msg.Content {
		if p.Type == ContentTypeText {
			text += p.Text
		}
	}
	return text
}

func lastAssistantMessage(messages []Message) *Message {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == MessageRoleAssistant {
			cloned := cloneMessage(messages[i])
			return &cloned
		}
	}
	return nil
}
