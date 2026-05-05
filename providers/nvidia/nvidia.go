package nvidia

import (
	"net/http"

	"github.com/erain/glue/providers/openaicompat"
)

const (
	providerName   = "nvidia"
	defaultBaseURL = "https://integrate.api.nvidia.com/v1"
	apiKeyEnv      = "NVIDIA_API_KEY"
)

// Options configures the NVIDIA provider.
//
// APIKey is consulted first; when empty the NVIDIA_API_KEY environment
// variable is used. DefaultModel applies when [loop.ProviderRequest.Model]
// is empty. BaseURL defaults to https://integrate.api.nvidia.com/v1 and
// may be overridden to point at any OpenAI-compatible endpoint. HTTPClient
// and Headers are optional; Headers are merged into every outgoing request.
type Options struct {
	APIKey       string
	DefaultModel string
	BaseURL      string
	HTTPClient   *http.Client
	Headers      map[string]string
}

// Provider is an alias for the shared OpenAI-compatible provider so that
// nvidia.New continues to return *nvidia.Provider for back-compat.
type Provider = openaicompat.Provider

// New creates an NVIDIA provider that streams responses from
// integrate.api.nvidia.com (or any caller-supplied OpenAI-compatible
// endpoint) into Glue's normalized provider events.
func New(o Options) *Provider {
	baseURL := o.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return openaicompat.New(openaicompat.Options{
		Name:         providerName,
		BaseURL:      baseURL,
		APIKey:       o.APIKey,
		APIKeyEnv:    apiKeyEnv,
		DefaultModel: o.DefaultModel,
		HTTPClient:   o.HTTPClient,
		Headers:      o.Headers,
	})
}
