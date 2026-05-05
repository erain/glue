package nvidia

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
	providerName   = "nvidia"
	defaultBaseURL = "https://integrate.api.nvidia.com/v1"
)

// Options configures the NVIDIA provider.
//
// APIKey is consulted first; when empty the NVIDIA_API_KEY environment
// variable is used. DefaultModel applies when [loop.ProviderRequest.Model]
// is empty. BaseURL defaults to https://integrate.api.nvidia.com/v1 and may
// be overridden to point at any OpenAI-compatible endpoint. HTTPClient and
// Headers are optional; Headers are merged into every outgoing request.
type Options struct {
	APIKey       string
	DefaultModel string
	BaseURL      string
	HTTPClient   *http.Client
	Headers      map[string]string
}

// Provider streams responses from an OpenAI-compatible NVIDIA build endpoint
// into Glue's normalized provider events.
type Provider struct {
	apiKey       string
	defaultModel string
	baseURL      string
	httpClient   *http.Client
	headers      map[string]string
}

// New creates an NVIDIA provider. The HTTP client is created lazily on the
// first [Provider.Stream] call so that constructing a provider does not
// require credentials.
func New(options Options) *Provider {
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	headers := map[string]string{}
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

// Stream implements [loop.Provider]. It posts to /chat/completions with
// stream=true and translates the SSE response into provider events.
func (p *Provider) Stream(ctx context.Context, req loop.ProviderRequest) (<-chan loop.ProviderEvent, error) {
	if p == nil {
		return nil, errors.New("nvidia: nil provider")
	}

	model := req.Model
	if model == "" {
		model = p.defaultModel
	}
	if model == "" {
		return nil, errors.New("nvidia: model is required")
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
		apiKey = os.Getenv("NVIDIA_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("nvidia: API key is required (set NVIDIA_API_KEY or Options.APIKey)")
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("nvidia: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("nvidia: build request: %w", err)
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
		return nil, fmt.Errorf("nvidia: send request: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, fmt.Errorf("nvidia: http %d: %s", httpResp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	events := make(chan loop.ProviderEvent)
	go p.stream(ctx, httpResp, model, events)
	return events, nil
}

// streamingChunk is a single SSE chat.completion.chunk frame.
type streamingChunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *streamUsage   `json:"usage"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

type streamDelta struct {
	Role             string                 `json:"role"`
	Content          string                 `json:"content"`
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
	ReasoningTokens  int `json:"reasoning_tokens"`
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
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			if data == "[DONE]" {
				break
			}
			continue
		}

		var chunk streamingChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: fmt.Sprintf("nvidia: parse chunk: %v", err)})
			return
		}
		if chunk.ID != "" {
			output.Metadata["response_id"] = chunk.ID
		}
		if chunk.Model != "" {
			output.Model = chunk.Model
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
			if choice.Delta.ReasoningContent != "" {
				appendThinking(&output, choice.Delta.ReasoningContent)
				if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventThinkingDelta, Delta: choice.Delta.ReasoningContent}) {
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
		send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: fmt.Sprintf("nvidia: read stream: %v", err)})
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
		return loop.ToolCall{}, fmt.Errorf("nvidia: tool call at index %d missing name", index)
	}
	args := p.arguments.String()
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return loop.ToolCall{}, fmt.Errorf("nvidia: tool call %q invalid JSON arguments", p.name)
	}
	id := p.id
	if id == "" {
		id = fmt.Sprintf("%s_%d", p.name, index)
	}
	return loop.ToolCall{ID: id, Name: p.name, Arguments: json.RawMessage(args)}, nil
}

// sortedIndices returns pending tool-call indices in ascending order so the
// emitted ContentParts are deterministic regardless of map iteration order.
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
