package nvidia

import (
	"net/http"

	"github.com/erain/glue/loop"
	"github.com/erain/glue/providers"
	"github.com/erain/glue/providers/openaicompat"
)

// EnvKey is the environment variable the provider reads when Options.APIKey
// is empty. Exposed so the providers registry and downstream agents can
// probe key availability without hard-coding the name.
const EnvKey = "NVIDIA_API_KEY"

// DefaultModel is the registry-level default model for this provider.
// Kimi K2.6 on NVIDIA build is currently the strongest free model
// exposed through build.nvidia.com.
const DefaultModel = "moonshotai/kimi-k2.6"

const (
	providerName   = "nvidia"
	defaultBaseURL = "https://integrate.api.nvidia.com/v1"
	apiKeyEnv      = EnvKey
)

func init() {
	providers.Register(providerName, providers.Factory{
		New:          func() loop.Provider { return New(Options{}) },
		DefaultModel: DefaultModel,
		EnvKey:       EnvKey,
	})
}

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
