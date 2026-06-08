package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/erain/glue/loop"
	"github.com/erain/glue/providers"

	"google.golang.org/genai"
)

// EnvKey is the environment variable the provider reads when Options.APIKey
// is empty. Exposed so the providers registry and downstream agents can
// probe key availability without hard-coding the name.
const EnvKey = "GEMINI_API_KEY"

// DefaultModel is the registry-level default model for this provider.
//
// gemini-3.1-pro is the daily-driver default per maintainer direction: it
// is the model the project's primary key has unmetered access to, and it
// is currently the most capable Gemini for coding-agent workloads. The
// previous default (gemini-2.5-flash) was removed from the v1beta API and
// began returning 404 on generateContent.
const DefaultModel = "gemini-3.1-pro"

const providerName = "gemini"

func init() {
	providers.Register(providerName, providers.Factory{
		New:          func() loop.Provider { return New(Options{}) },
		DefaultModel: DefaultModel,
		EnvKey:       EnvKey,
	})
}

// Options configures the Gemini provider.
//
// APIKey is consulted first; when empty the GEMINI_API_KEY environment
// variable is used. DefaultModel applies when [loop.ProviderRequest.Model]
// is empty. Client, when non-nil, is used as-is and APIKey is ignored.
type Options struct {
	APIKey       string
	DefaultModel string
	Client       *genai.Client
}

// Provider streams Gemini responses into Glue's normalized provider events.
type Provider struct {
	apiKey       string
	defaultModel string
	client       *genai.Client
}

// New creates a Gemini provider. The genai client is created lazily on the
// first [Provider.Stream] call so that constructing a provider does not
// require credentials.
func New(options Options) *Provider {
	return &Provider{
		apiKey:       options.APIKey,
		defaultModel: options.DefaultModel,
		client:       options.Client,
	}
}

// Stream implements [loop.Provider]. It supports text-only conversations
// and Gemini function calling: tool specs in [loop.ProviderRequest.Tools]
// are converted to function declarations, inbound function calls become
// [loop.ProviderEventToolCall] events, and tool-role messages in the
// transcript are converted to function responses.
func (p *Provider) Stream(ctx context.Context, req loop.ProviderRequest) (<-chan loop.ProviderEvent, error) {
	if p == nil {
		return nil, errors.New("gemini: nil provider")
	}

	model := req.Model
	if model == "" {
		model = p.defaultModel
	}
	if model == "" {
		return nil, errors.New("gemini: model is required")
	}

	contents, err := ConvertMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	config, err := buildGenerateConfig(req)
	if err != nil {
		return nil, err
	}
	client, err := p.clientFor(ctx)
	if err != nil {
		return nil, err
	}

	events := make(chan loop.ProviderEvent)
	go p.stream(ctx, client, model, contents, config, events)
	return events, nil
}

func (p *Provider) clientFor(ctx context.Context) (*genai.Client, error) {
	if p.client != nil {
		return p.client, nil
	}
	apiKey := p.apiKey
	if apiKey == "" {
		apiKey = os.Getenv(EnvKey)
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: create client: %w", err)
	}
	return client, nil
}

func (p *Provider) stream(
	ctx context.Context,
	client *genai.Client,
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
	events chan<- loop.ProviderEvent,
) {
	defer close(events)

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

	toolCallCount := 0
	for response, err := range client.Models.GenerateContentStream(ctx, model, contents, config) {
		if err != nil {
			send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: err.Error()})
			return
		}
		applyResponseMetadata(&output, response)

		if len(response.Candidates) == 0 || response.Candidates[0] == nil {
			continue
		}
		candidate := response.Candidates[0]
		if candidate.FinishReason != "" {
			output.StopReason = mapFinishReason(candidate.FinishReason)
		}
		if candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if part == nil {
				continue
			}
			if part.FunctionCall != nil {
				toolCallCount++
				toolCall, err := convertFunctionCall(part.FunctionCall, toolCallCount)
				if err != nil {
					send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventError, Error: err.Error()})
					return
				}
				output.Content = append(output.Content, loop.ContentPart{Type: loop.ContentTypeToolCall, ToolCall: &toolCall})
				if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventToolCall, ToolCall: &toolCall}) {
					return
				}
				continue
			}
			if part.Text == "" {
				continue
			}
			if part.Thought {
				appendThinking(&output, part.Text)
				if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventThinkingDelta, Delta: part.Text}) {
					return
				}
				continue
			}
			appendText(&output, part.Text)
			if !send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventTextDelta, Delta: part.Text}) {
				return
			}
		}
	}

	if hasToolCall(output) {
		output.StopReason = loop.StopReasonToolUse
	}
	if output.StopReason == "" {
		output.StopReason = loop.StopReasonStop
	}
	if len(output.Metadata) == 0 {
		output.Metadata = nil
	}
	send(ctx, events, loop.ProviderEvent{Type: loop.ProviderEventDone, Message: &output})
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

func applyResponseMetadata(message *loop.Message, response *genai.GenerateContentResponse) {
	if response == nil {
		return
	}
	if response.ModelVersion != "" {
		message.Model = response.ModelVersion
	}
	if response.ResponseID != "" {
		if message.Metadata == nil {
			message.Metadata = map[string]any{}
		}
		message.Metadata["response_id"] = response.ResponseID
	}
	if response.UsageMetadata != nil {
		usage := response.UsageMetadata
		message.Usage = &loop.Usage{
			InputTokens:     int64(usage.PromptTokenCount),
			OutputTokens:    int64(usage.CandidatesTokenCount + usage.ThoughtsTokenCount),
			CacheReadTokens: int64(usage.CachedContentTokenCount),
			TotalTokens:     int64(usage.TotalTokenCount),
		}
	}
}

func convertFunctionCall(call *genai.FunctionCall, fallbackIndex int) (loop.ToolCall, error) {
	if call == nil {
		return loop.ToolCall{}, errors.New("gemini: nil function call")
	}
	args, err := json.Marshal(call.Args)
	if err != nil {
		return loop.ToolCall{}, fmt.Errorf("gemini: marshal function call args: %w", err)
	}
	if string(args) == "null" {
		args = []byte(`{}`)
	}

	id := call.ID
	if id == "" {
		id = fmt.Sprintf("%s_%d", call.Name, fallbackIndex)
	}
	return loop.ToolCall{
		ID:        id,
		Name:      call.Name,
		Arguments: args,
	}, nil
}

func hasToolCall(message loop.Message) bool {
	for _, part := range message.Content {
		if part.Type == loop.ContentTypeToolCall && part.ToolCall != nil {
			return true
		}
	}
	return false
}

func mapFinishReason(reason genai.FinishReason) loop.StopReason {
	switch reason {
	case "", genai.FinishReasonUnspecified, genai.FinishReasonStop:
		return loop.StopReasonStop
	case genai.FinishReasonMaxTokens:
		return loop.StopReasonLength
	default:
		return loop.StopReasonError
	}
}
