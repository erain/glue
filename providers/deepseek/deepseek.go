// Package deepseek connects Glue to DeepSeek's Chat Completions API.
// Tool conversations preserve reasoning_content as required by the API.
package deepseek

import (
	"context"
	"fmt"
	"net/http"

	"github.com/erain/glue/loop"
	"github.com/erain/glue/providers"
	"github.com/erain/glue/providers/openaicompat"
)

const EnvKey = "DEEPSEEK_API_KEY"
const DefaultModel = "deepseek-flash"

func init() {
	providers.Register("deepseek", providers.Factory{
		New: func() loop.Provider { return New(Options{}) }, DefaultModel: DefaultModel, EnvKey: EnvKey,
		ModelsListURL: "https://api.deepseek.com/models",
		Models:        []providers.ModelInfo{{ID: DefaultModel, ThinkingLevels: []string{"off", "low", "high", "max"}}},
	})
}

// Options configures credentials, the default model, and optional transport
// overrides. Empty APIKey reads DEEPSEEK_API_KEY. Empty BaseURL uses the
// official https://api.deepseek.com endpoint.
type Options struct {
	APIKey       string
	DefaultModel string
	BaseURL      string
	HTTPClient   *http.Client
}

type Provider struct {
	inner        *openaicompat.Provider
	defaultModel string
}

func New(o Options) *Provider {
	if o.BaseURL == "" {
		o.BaseURL = "https://api.deepseek.com"
	}
	if o.DefaultModel == "" {
		o.DefaultModel = DefaultModel
	}
	return &Provider{defaultModel: o.DefaultModel, inner: openaicompat.New(openaicompat.Options{
		Name: "deepseek", BaseURL: o.BaseURL, APIKey: o.APIKey, APIKeyEnv: EnvKey,
		DefaultModel: o.DefaultModel, HTTPClient: o.HTTPClient, PreserveReasoning: true,
	})}
}

func (p *Provider) Stream(ctx context.Context, req loop.ProviderRequest) (<-chan loop.ProviderEvent, error) {
	if p == nil {
		return nil, fmt.Errorf("deepseek: nil provider")
	}
	if req.Model == "" {
		req.Model = p.defaultModel
	}
	if raw, exists := req.Options["thinking_level"]; exists {
		level, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("deepseek: thinking_level must be a string")
		}
		if _, err := providers.ThinkingOptions("deepseek", req.Model, level); err != nil {
			return nil, err
		}
		options := make(map[string]any, len(req.Options)+1)
		for k, v := range req.Options {
			if k != "thinking_level" {
				options[k] = v
			}
		}
		if level != "" && level != "default" {
			if _, exists := options["thinking"]; exists {
				return nil, fmt.Errorf("deepseek: thinking_level conflicts with thinking")
			}
			if _, exists := options["reasoning_effort"]; exists {
				return nil, fmt.Errorf("deepseek: thinking_level conflicts with reasoning_effort")
			}
			if level == "off" {
				options["thinking"] = map[string]any{"type": "disabled"}
			} else {
				options["thinking"] = map[string]any{"type": "enabled"}
				options["reasoning_effort"] = level
			}
		}
		req.Options = options
	}
	return p.inner.Stream(ctx, req)
}
