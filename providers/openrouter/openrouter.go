package openrouter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/erain/glue/loop"
)

const (
	providerName   = "openrouter"
	defaultBaseURL = "https://openrouter.ai/api/v1"

	// Attribution headers OpenRouter recommends for clients so requests
	// surface the calling project in their analytics. Both are overridable
	// via Options.Headers.
	defaultRefererURL = "https://github.com/erain/glue"
	defaultTitle      = "glue"
)

// Options configures the OpenRouter provider.
//
// APIKey is consulted first; when empty the OPENROUTER_API_KEY environment
// variable is used. DefaultModel applies when [loop.ProviderRequest.Model]
// is empty. BaseURL defaults to https://openrouter.ai/api/v1 and may be
// overridden. HTTPClient is optional. Headers are merged into every
// outgoing request and override the default attribution headers
// (HTTP-Referer / X-Title) on key collision.
type Options struct {
	APIKey       string
	DefaultModel string
	BaseURL      string
	HTTPClient   *http.Client
	Headers      map[string]string
}

// Provider streams responses from OpenRouter into Glue's normalized
// provider events.
type Provider struct {
	apiKey       string
	defaultModel string
	baseURL      string
	httpClient   *http.Client
	headers      map[string]string
}

// New creates an OpenRouter provider.
func New(options Options) *Provider {
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	headers := map[string]string{
		"HTTP-Referer": defaultRefererURL,
		"X-Title":      defaultTitle,
	}
	for k, v := range options.Headers {
		headers[k] = v
	}
	return &Provider{
		apiKey:       options.APIKey,
		defaultModel: options.DefaultModel,
		baseURL:      baseURL,
		httpClient:   options.HTTPClient,
		headers:      headers,
	}
}

// Stream implements [loop.Provider].
func (p *Provider) Stream(ctx context.Context, req loop.ProviderRequest) (<-chan loop.ProviderEvent, error) {
	if p == nil {
		return nil, errors.New("openrouter: nil provider")
	}

	model := req.Model
	if model == "" {
		model = p.defaultModel
	}
	if model == "" {
		return nil, errors.New("openrouter: model is required")
	}

	body, err := buildChatRequest(req)
	if err != nil {
		return nil, err
	}
	body.Model = model
	body.Stream = true
	body.StreamOptions = &streamOptions{IncludeUsage: true}

	apiKey := p.apiKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENROUTER_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("openrouter: API key is required (set OPENROUTER_API_KEY or Options.APIKey)")
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openrouter: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openrouter: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range p.headers {
		httpReq.Header.Set(k, v)
	}

	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openrouter: send request: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, fmt.Errorf("openrouter: http %d: %s", httpResp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	events := make(chan loop.ProviderEvent)
	go p.stream(ctx, httpResp, model, events)
	return events, nil
}

// streamingChunk is a single SSE chat.completion.chunk frame as emitted by
// OpenRouter. Compared with strict OpenAI shape, OpenRouter:
//   - exposes the underlying upstream provider name in `provider`
//   - emits reasoning text in `delta.reasoning` (string) rather than
//     `delta.reasoning_content`
type streamingChunk struct {
	ID       string         `json:"id"`
	Model    string         `json:"model"`
	Provider string         `json:"provider"`
	Choices  []streamChoice `json:"choices"`
	Usage    *streamUsage   `json:"usage"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

type streamDelta struct {
	Role      string                 `json:"role"`
	Content   string                 `json:"content"`
	Reasoning string                 `json:"reasoning"`
	ToolCalls []streamingToolCallDel `json:"tool_calls"`
}

type streamingToolCallDel struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type streamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (p *Provider) stream(ctx context.Context, resp *http.Response, model string, events chan<- loop.ProviderEvent) {
	defer close(events)
	defer resp.Body.Close()

	output := loop.Message{
		Role:      loop.MessageRoleAssistant,
		Provider:  providerName,
		Model:     model,
		CreatedAt: time.Now().UTC(),
		Metadata:  map[string]any{},
	}
	if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventStart, Message: &output}) {
		return
	}

	pendingCalls := map[int]*pendingToolCall{}
	emittedCallIDs := map[int]string{}
	finishReason := ""

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// SSE comment lines (starting with ":") are used by OpenRouter as
		// keep-alives during cold routing — drop them silently.
		if strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var chunk streamingChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: fmt.Sprintf("openrouter: parse chunk: %v", err)})
			return
		}
		if chunk.ID != "" {
			output.Metadata["response_id"] = chunk.ID
		}
		if chunk.Model != "" {
			output.Model = chunk.Model
		}
		if chunk.Provider != "" {
			output.Metadata["upstream_provider"] = chunk.Provider
		}
		if chunk.Usage != nil {
			output.Usage = &loop.Usage{
				InputTokens:  int64(chunk.Usage.PromptTokens),
				OutputTokens: int64(chunk.Usage.CompletionTokens),
				TotalTokens:  int64(chunk.Usage.TotalTokens),
			}
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				appendText(&output, choice.Delta.Content)
				if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventTextDelta, Delta: choice.Delta.Content}) {
					return
				}
			}
			if choice.Delta.Reasoning != "" {
				appendThinking(&output, choice.Delta.Reasoning)
				if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventThinkingDelta, Delta: choice.Delta.Reasoning}) {
					return
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				p := pendingCalls[tc.Index]
				if p == nil {
					p = &pendingToolCall{}
					pendingCalls[tc.Index] = p
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					p.arguments.WriteString(tc.Function.Arguments)
				}
			}
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: fmt.Sprintf("openrouter: read stream: %v", err)})
		return
	}

	for _, index := range sortedIndices(pendingCalls) {
		call := pendingCalls[index]
		if _, already := emittedCallIDs[index]; already {
			continue
		}
		emittedCallIDs[index] = call.id
		toolCall, err := call.finalize(index)
		if err != nil {
			send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: err.Error()})
			return
		}
		output.Content = append(output.Content, loop.ContentPart{Type: loop.ContentTypeToolCall, ToolCall: &toolCall})
		if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventToolCall, ToolCall: &toolCall}) {
			return
		}
	}

	switch {
	case len(pendingCalls) > 0:
		output.StopReason = loop.StopReasonToolUse
	case finishReason != "":
		output.StopReason = mapFinishReason(finishReason)
	default:
		output.StopReason = loop.StopReasonStop
	}
	if len(output.Metadata) == 0 {
		output.Metadata = nil
	}
	send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventDone, Message: &output})
}

type pendingToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

func (p *pendingToolCall) finalize(index int) (loop.ToolCall, error) {
	if p.name == "" {
		return loop.ToolCall{}, fmt.Errorf("openrouter: tool call at index %d missing name", index)
	}
	args := p.arguments.String()
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return loop.ToolCall{}, fmt.Errorf("openrouter: tool call %q invalid JSON arguments", p.name)
	}
	id := p.id
	if id == "" {
		id = fmt.Sprintf("%s_%d", p.name, index)
	}
	return loop.ToolCall{ID: id, Name: p.name, Arguments: json.RawMessage(args)}, nil
}

func sortedIndices(calls map[int]*pendingToolCall) []int {
	indices := make([]int, 0, len(calls))
	for index := range calls {
		indices = append(indices, index)
	}
	for i := 1; i < len(indices); i++ {
		for j := i; j > 0 && indices[j-1] > indices[j]; j-- {
			indices[j-1], indices[j] = indices[j], indices[j-1]
		}
	}
	return indices
}

func send(ctx context.Context, events chan<- loop.ProviderEvent, event loop.ProviderEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case events <- event:
		return true
	}
}

func appendText(message *loop.Message, delta string) {
	appendPart(message, loop.ContentTypeText, delta)
}

func appendThinking(message *loop.Message, delta string) {
	appendPart(message, loop.ContentTypeThinking, delta)
}

func appendPart(message *loop.Message, kind loop.ContentType, delta string) {
	last := len(message.Content) - 1
	if last >= 0 && message.Content[last].Type == kind {
		switch kind {
		case loop.ContentTypeText:
			message.Content[last].Text += delta
		case loop.ContentTypeThinking:
			message.Content[last].Thinking += delta
		}
		return
	}
	part := loop.ContentPart{Type: kind}
	switch kind {
	case loop.ContentTypeText:
		part.Text = delta
	case loop.ContentTypeThinking:
		part.Thinking = delta
	}
	message.Content = append(message.Content, part)
}
