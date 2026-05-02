package glue

import (
	"context"
	"errors"
	"sync"
	"time"

	"glue/loop"
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
	model        string
	systemPrompt string
	tools        []Tool
	options      map[string]any
	maxTurns     int
	emit         func(Event)
}

// WithModel overrides the agent model for one prompt.
func WithModel(model string) PromptOption {
	return func(c *promptConfig) { c.model = model }
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

// WithEvents registers a per-prompt event handler. It receives every loop
// event for that prompt in addition to any session-scoped subscribers
// installed via [Session.Subscribe].
func WithEvents(handler func(Event)) PromptOption {
	return func(c *promptConfig) { c.emit = handler }
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

	config := s.promptConfig(options)
	userMessage := Message{
		Role:      MessageRoleUser,
		Content:   []ContentPart{{Type: ContentTypeText, Text: text}},
		CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	base := cloneMessages(s.messages)
	s.mu.Unlock()
	runMessages := append(base, userMessage)

	result, runErr := loop.Run(ctx, loop.RunRequest{
		Provider:     s.agent.provider,
		Model:        config.model,
		SystemPrompt: config.systemPrompt,
		Messages:     runMessages,
		Tools:        config.tools,
		Options:      config.options,
		MaxTurns:     config.maxTurns,
		Emit: func(event Event) {
			if config.emit != nil {
				config.emit(event)
			}
			s.dispatchEvent(event)
		},
	})

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

func (s *Session) save(ctx context.Context, state SessionState) error {
	if s.agent == nil || s.agent.store == nil {
		return nil
	}
	return s.agent.store.Save(ctx, s.id, state)
}

func (s *Session) promptConfig(options []PromptOption) promptConfig {
	config := promptConfig{
		model:        s.agent.model,
		systemPrompt: s.agent.systemPrompt,
		tools:        cloneTools(s.agent.tools),
		options:      cloneMap(s.agent.options),
		maxTurns:     s.agent.maxTurns,
	}
	for _, opt := range options {
		if opt != nil {
			opt(&config)
		}
	}
	return config
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
