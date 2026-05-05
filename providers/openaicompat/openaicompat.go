package openaicompat

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

// Options configures the shared OpenAI-compatible provider.
//
// Name is required and is set on every assistant Message.Provider so
// transcripts identify which vendor produced each message. BaseURL is
// required and points at the chat-completions root (e.g.
// "https://integrate.api.nvidia.com/v1"). APIKeyEnv is optional; when
// APIKey is empty Stream consults this environment variable. Headers are
// merged into every outgoing HTTP request.
type Options struct {
	Name         string
	BaseURL      string
	APIKey       string
	APIKeyEnv    string
	DefaultModel string
	HTTPClient   *http.Client
	Headers      map[string]string
}

// Provider streams chat-completion responses from any OpenAI-compatible
// endpoint into Glue's normalized provider events. Vendor packages
// configure it via [New] and re-export its alias type.
type Provider struct {
	name         string
	baseURL      string
	apiKey       string
	apiKeyEnv    string
	defaultModel string
	httpClient   *http.Client
	headers      map[string]string
}

// New creates a Provider with the supplied vendor configuration.
// Required fields (Name, BaseURL) are validated lazily on Stream so that
// constructing a Provider does not return errors.
func New(o Options) *Provider {
	headers := map[string]string{}
	for k, v := range o.Headers {
		headers[k] = v
	}
	return &Provider{
		name:         o.Name,
		baseURL:      strings.TrimRight(o.BaseURL, "/"),
		apiKey:       o.APIKey,
		apiKeyEnv:    o.APIKeyEnv,
		defaultModel: o.DefaultModel,
		httpClient:   o.HTTPClient,
		headers:      headers,
	}
}

// Stream implements [loop.Provider].
func (p *Provider) Stream(ctx context.Context, req loop.ProviderRequest) (<-chan loop.ProviderEvent, error) {
	if p == nil {
		return nil, errors.New("openaicompat: nil provider")
	}
	if p.name == "" || p.baseURL == "" {
		return nil, errors.New("openaicompat: provider missing Name or BaseURL")
	}

	model := req.Model
	if model == "" {
		model = p.defaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("%s: model is required", p.name)
	}

	body, err := buildChatRequest(req)
	if err != nil {
		return nil, err
	}
	body.Model = model
	body.Stream = true
	body.StreamOptions = &streamOptions{IncludeUsage: true}

	apiKey := p.apiKey
	if apiKey == "" && p.apiKeyEnv != "" {
		apiKey = os.Getenv(p.apiKeyEnv)
	}
	if apiKey == "" {
		hint := p.apiKeyEnv
		if hint == "" {
			hint = "Options.APIKey"
		} else {
			hint = "set " + hint + " or Options.APIKey"
		}
		return nil, fmt.Errorf("%s: API key is required (%s)", p.name, hint)
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal request: %w", p.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", p.name, err)
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
		return nil, fmt.Errorf("%s: send request: %w", p.name, err)
	}
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, fmt.Errorf("%s: http %d: %s", p.name, httpResp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	events := make(chan loop.ProviderEvent)
	go p.stream(ctx, httpResp, model, events)
	return events, nil
}

// streamingChunk represents one SSE chat.completion.chunk frame. It
// accepts both delta.reasoning (OpenRouter) and delta.reasoning_content
// (NVIDIA) as thinking content; whichever is non-empty in any chunk is
// appended to the assistant message's thinking part.
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
	Role             string                 `json:"role"`
	Content          string                 `json:"content"`
	Reasoning        string                 `json:"reasoning"`
	ReasoningContent string                 `json:"reasoning_content"`
	ToolCalls        []streamingToolCallDel `json:"tool_calls"`
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
		Provider:  p.name,
		Model:     model,
		CreatedAt: time.Now().UTC(),
		Metadata:  map[string]any{},
	}
	if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventStart, Message: &output}) {
		return
	}

	pendingCalls := map[int]*pendingToolCall{}
	finishReason := ""

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// SSE comment lines used as keep-alives by some upstreams (e.g.,
		// OpenRouter's `: OPENROUTER PROCESSING`). Drop silently.
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
			send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: fmt.Sprintf("%s: parse chunk: %v", p.name, err)})
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
			if reason := firstNonEmpty(choice.Delta.Reasoning, choice.Delta.ReasoningContent); reason != "" {
				appendThinking(&output, reason)
				if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventThinkingDelta, Delta: reason}) {
					return
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				pc := pendingCalls[tc.Index]
				if pc == nil {
					pc = &pendingToolCall{}
					pendingCalls[tc.Index] = pc
				}
				if tc.ID != "" {
					pc.id = tc.ID
				}
				if tc.Function.Name != "" {
					pc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					pc.arguments.WriteString(tc.Function.Arguments)
				}
			}
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: fmt.Sprintf("%s: read stream: %v", p.name, err)})
		return
	}

	for _, index := range sortedIndices(pendingCalls) {
		call := pendingCalls[index]
		toolCall, err := call.finalize(p.name, index)
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

func (p *pendingToolCall) finalize(provider string, index int) (loop.ToolCall, error) {
	if p.name == "" {
		return loop.ToolCall{}, fmt.Errorf("%s: tool call at index %d missing name", provider, index)
	}
	args := p.arguments.String()
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return loop.ToolCall{}, fmt.Errorf("%s: tool call %q invalid JSON arguments", provider, p.name)
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
