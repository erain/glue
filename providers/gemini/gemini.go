package gemini

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"glue/loop"

	"google.golang.org/genai"
)

const providerName = "gemini"

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

// Stream implements [loop.Provider]. The current implementation supports
// text-only conversations; function calling lands in a follow-up issue and
// will extend ConvertMessages and buildGenerateConfig accordingly. Until
// then, [loop.ProviderRequest.Tools] is silently ignored so the provider
// never produces tool-call parts.
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
		apiKey = os.Getenv("GEMINI_API_KEY")
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
			if part == nil || part.Text == "" {
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
