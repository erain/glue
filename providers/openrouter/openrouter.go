package openrouter

import (
	"net/http"

	"github.com/erain/glue/providers/openaicompat"
)

const (
	providerName   = "openrouter"
	defaultBaseURL = "https://openrouter.ai/api/v1"
	apiKeyEnv      = "OPENROUTER_API_KEY"

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

// Provider is an alias for the shared OpenAI-compatible provider so that
// openrouter.New continues to return *openrouter.Provider for back-compat.
type Provider = openaicompat.Provider

// New creates an OpenRouter provider that streams responses from
// openrouter.ai/api/v1 (or any caller-supplied OpenAI-compatible endpoint)
// into Glue's normalized provider events. The default attribution headers
// can be overridden by setting matching keys in Options.Headers.
func New(o Options) *Provider {
	baseURL := o.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	headers := map[string]string{
		"HTTP-Referer": defaultRefererURL,
		"X-Title":      defaultTitle,
	}
	for k, v := range o.Headers {
		headers[k] = v
	}
	return openaicompat.New(openaicompat.Options{
		Name:         providerName,
		BaseURL:      baseURL,
		APIKey:       o.APIKey,
		APIKeyEnv:    apiKeyEnv,
		DefaultModel: o.DefaultModel,
		HTTPClient:   o.HTTPClient,
		Headers:      headers,
	})
}
