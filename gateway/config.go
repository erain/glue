// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

// Package gateway implements the glue-gateway job registry, backend routing,
// event stream proxy, and browser UI host.
package gateway

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/erain/glue/jobs"
	"gopkg.in/yaml.v3"
)

const (
	DefaultSuspendAfter       = 2 * time.Minute
	DefaultResumeTimeout      = time.Minute
	DefaultRetainTasks        = 168 * time.Hour
	DefaultSessionAbsoluteTTL = 168 * time.Hour
	DefaultSessionIdleTTL     = 24 * time.Hour
	DefaultAuthRateLimit      = 30
)

// DefaultEgressHosts is the default TLS passthrough allowlist for AX tasks.
var DefaultEgressHosts = []string{
	"github.com", "api.github.com", "codeload.github.com",
	"proxy.golang.org", "sum.golang.org", "storage.googleapis.com",
	"generativelanguage.googleapis.com", "openrouter.ai",
}

// AuthConfig configures client authentication for glue-gateway.
type AuthConfig struct {
	Mode, Token        string
	ClientID           string        `yaml:"client_id"`
	ClientSecret       string        `yaml:"client_secret"`
	AllowedLogins      []string      `yaml:"allowed_logins"`
	SessionAbsoluteTTL time.Duration `yaml:"session_absolute_ttl"`
	SessionIdleTTL     time.Duration `yaml:"session_idle_ttl"`
	AuthorizeURL       string        `yaml:"authorize_url"`
	TokenURL           string        `yaml:"token_url"`
	UserURL            string        `yaml:"user_url"`
	RateLimitPerMin    int           `yaml:"rate_limit_per_min"`
	InsecureDev        bool          `yaml:"insecure_dev"`
}

// ResourceLimits configures CPU and memory limits for an AX task.
type ResourceLimits struct{ CPU, Memory string }

// ResourcesConfig configures AX task resource limits (top-level or nested limits).
type ResourcesConfig struct {
	CPU, Memory string
	Limits      ResourceLimits `json:"-"`
}

// BackendConfig configures one upstream job execution backend.
type BackendConfig struct {
	Name, Type, Atespace, Image, Substrate, Router string
	URL                                            string          `yaml:"url" json:"url,omitempty"`
	Token                                          string          `yaml:"token" json:"-"`
	Resources                                      ResourcesConfig `json:"resources,omitempty"`
	EgressHosts                                    []string        `yaml:"egress_hosts" json:"egress_hosts,omitempty"`
	AXServer                                       string          `yaml:"ax_server" json:"ax_server,omitempty"`
	CertFile                                       string          `yaml:"cert_file" json:"-"`
	CAFile                                         string          `yaml:"ca_file" json:"-"`
	TokenFile                                      string          `yaml:"token_file" json:"-"`
	SuspendAfter                                   time.Duration   `yaml:"suspend_after" json:"suspend_after,omitempty"`
	ResumeTimeout                                  time.Duration   `yaml:"resume_timeout" json:"resume_timeout,omitempty"`
	RetainTasks                                    time.Duration   `yaml:"retain_tasks" json:"retain_tasks,omitempty"`
	SuspendOnFinish                                *bool           `yaml:"suspend_on_finish" json:"suspend_on_finish,omitempty"`
}

// Config is the top-level YAML configuration for glue-gateway.
type Config struct {
	Listen         string           `yaml:"listen"`
	PublicURL      string           `yaml:"public_url"`
	StateDir       string           `yaml:"state_dir"`
	Auth           AuthConfig       `yaml:"auth"`
	Credentials    jobs.Credentials `yaml:"credentials" json:"-"`
	Backends       []BackendConfig  `yaml:"backends"`
	DefaultBackend string           `yaml:"default_backend"`
	// Task suspension, retention settings, and test hooks.
	SuspendAfter    time.Duration      `yaml:"suspend_after"`
	ResumeTimeout   time.Duration      `yaml:"resume_timeout"`
	RetainTasks     time.Duration      `yaml:"retain_tasks"`
	SuspendOnFinish *bool              `yaml:"suspend_on_finish"`
	Now             func() time.Time   `yaml:"-" json:"-"`
	CustomBackends  map[string]Backend `yaml:"-" json:"-"`
}

// LoadConfig reads and validates a YAML gateway configuration file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("gateway: read config %s: %w", path, err)
	}
	return ParseConfig(data)
}
func DecodeConfig(data []byte) (Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("gateway: parse config: %w", err)
	}
	return cfg, nil
}
func ParseConfig(data []byte) (Config, error) {
	cfg, err := DecodeConfig(data)
	if err != nil {
		return Config{}, err
	}
	return cfg.Validate()
}
func (cfg Config) Validate() (Config, error) {
	cfg.Listen = cmp.Or(strings.TrimSpace(cfg.Listen), "127.0.0.1:8800")
	cfg.PublicURL = strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/")
	cfg.StateDir = strings.TrimSpace(cfg.StateDir)
	cfg.Auth.Mode = cmp.Or(strings.TrimSpace(cfg.Auth.Mode), "dev-token")
	cfg.Auth.Token = cmp.Or(strings.TrimSpace(cfg.Auth.Token), strings.TrimSpace(os.Getenv("GLUE_GATEWAY_TOKEN")))
	cfg.Auth.ClientID, cfg.Auth.ClientSecret = cmp.Or(strings.TrimSpace(cfg.Auth.ClientID), strings.TrimSpace(os.Getenv("GLUE_GITHUB_CLIENT_ID"))), cmp.Or(strings.TrimSpace(cfg.Auth.ClientSecret), strings.TrimSpace(os.Getenv("GLUE_GITHUB_CLIENT_SECRET")))
	if envAL := strings.TrimSpace(os.Getenv("GLUE_ALLOWED_LOGINS")); len(cfg.Auth.AllowedLogins) == 0 && envAL != "" {
		cfg.Auth.AllowedLogins = strings.Split(envAL, ",")
	}
	for i := range cfg.Auth.AllowedLogins {
		if cfg.Auth.AllowedLogins[i] = strings.TrimSpace(cfg.Auth.AllowedLogins[i]); cfg.Auth.AllowedLogins[i] == "" {
			return Config{}, errors.New("gateway: invalid empty entry in auth.allowed_logins")
		}
	}
	cfg.Auth.SessionAbsoluteTTL, cfg.Auth.SessionIdleTTL, cfg.Auth.RateLimitPerMin = cmp.Or(cfg.Auth.SessionAbsoluteTTL, DefaultSessionAbsoluteTTL), cmp.Or(cfg.Auth.SessionIdleTTL, DefaultSessionIdleTTL), cmp.Or(cfg.Auth.RateLimitPerMin, DefaultAuthRateLimit)
	cfg.Auth.AuthorizeURL, cfg.Auth.TokenURL, cfg.Auth.UserURL = cmp.Or(strings.TrimSpace(cfg.Auth.AuthorizeURL), "https://github.com/login/oauth/authorize"), cmp.Or(strings.TrimSpace(cfg.Auth.TokenURL), "https://github.com/login/oauth/access_token"), cmp.Or(strings.TrimSpace(cfg.Auth.UserURL), "https://api.github.com/user")
	cfg.Credentials.GitHubToken = cmp.Or(strings.TrimSpace(cfg.Credentials.GitHubToken), strings.TrimSpace(os.Getenv("GLUE_GITHUB_TOKEN")))
	if cfg.Credentials.ProviderKeys == nil {
		cfg.Credentials.ProviderKeys = map[string]string{}
	}
	for p, env := range map[string]string{"gemini": "GEMINI_API_KEY", "openrouter": "OPENROUTER_API_KEY", "deepseek": "DEEPSEEK_API_KEY", "nvidia": "NVIDIA_API_KEY"} {
		if cfg.Credentials.ProviderKeys[p] = cmp.Or(strings.TrimSpace(cfg.Credentials.ProviderKeys[p]), strings.TrimSpace(os.Getenv(env))); cfg.Credentials.ProviderKeys[p] == "" {
			delete(cfg.Credentials.ProviderKeys, p)
		}
	}
	if envSA := strings.TrimSpace(os.Getenv("GLUE_SUSPEND_AFTER")); cfg.SuspendAfter == 0 && envSA != "" {
		if d, err := time.ParseDuration(envSA); err == nil && d > 0 {
			cfg.SuspendAfter = d
		} else {
			return Config{}, fmt.Errorf("gateway: invalid GLUE_SUSPEND_AFTER %q", envSA)
		}
	}
	if envRT := strings.TrimSpace(os.Getenv("GLUE_RETAIN_TASKS")); cfg.RetainTasks == 0 && envRT != "" {
		if d, err := time.ParseDuration(envRT); err == nil && d > 0 {
			cfg.RetainTasks = d
		} else {
			return Config{}, fmt.Errorf("gateway: invalid GLUE_RETAIN_TASKS %q", envRT)
		}
	}
	cfg.SuspendAfter, cfg.ResumeTimeout, cfg.RetainTasks = cmp.Or(cfg.SuspendAfter, DefaultSuspendAfter), cmp.Or(cfg.ResumeTimeout, DefaultResumeTimeout), cmp.Or(cfg.RetainTasks, DefaultRetainTasks)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Auth.Mode == "dev-token" && strings.HasPrefix(strings.ToLower(cfg.PublicURL), "https://") && !cfg.Auth.InsecureDev {
		return Config{}, errors.New("gateway: dev-token mode is not allowed when public_url is https:// unless --insecure-dev is passed")
	}
	authOK := (cfg.Auth.Mode == "dev-token" && cfg.Auth.Token != "") || (cfg.Auth.Mode == "github" && cfg.PublicURL != "" && cfg.Auth.ClientID != "" && cfg.Auth.ClientSecret != "" && len(cfg.Auth.AllowedLogins) > 0)
	if cfg.SuspendAfter < 0 || cfg.ResumeTimeout < 0 || cfg.RetainTasks < 0 || cfg.Auth.SessionAbsoluteTTL <= 0 || cfg.Auth.SessionIdleTTL <= 0 || cfg.Auth.RateLimitPerMin <= 0 || cfg.StateDir == "" || !authOK || len(cfg.Backends) == 0 {
		return Config{}, fmt.Errorf("gateway: invalid config (state_dir=%q, auth.mode=%q, has_token=%v, backends=%d)", cfg.StateDir, cfg.Auth.Mode, cfg.Auth.Token != "", len(cfg.Backends))
	}
	seen := make(map[string]bool, len(cfg.Backends))
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		b.Name, b.Type = strings.TrimSpace(b.Name), strings.TrimSpace(b.Type)
		b.URL, b.Token = strings.TrimRight(strings.TrimSpace(b.URL), "/"), strings.TrimSpace(b.Token)
		if b.SuspendAfter < 0 || b.ResumeTimeout < 0 || b.RetainTasks < 0 || b.Name == "" || seen[b.Name] || !validateBackend(b) {
			return Config{}, fmt.Errorf("gateway: invalid backend %q (duplicate=%v, type=%q)", b.Name, seen[b.Name], b.Type)
		}
		if b.Type == "ax" {
			b.SuspendAfter, b.ResumeTimeout, b.RetainTasks = cmp.Or(b.SuspendAfter, cfg.SuspendAfter), cmp.Or(b.ResumeTimeout, cfg.ResumeTimeout), cmp.Or(b.RetainTasks, cfg.RetainTasks)
			if b.SuspendOnFinish == nil {
				b.SuspendOnFinish = cfg.SuspendOnFinish
			}
			cfg.SuspendAfter, cfg.ResumeTimeout, cfg.RetainTasks, cfg.SuspendOnFinish = b.SuspendAfter, b.ResumeTimeout, b.RetainTasks, b.SuspendOnFinish
		}
		seen[b.Name] = true
	}
	if cfg.DefaultBackend = strings.TrimSpace(cfg.DefaultBackend); cfg.DefaultBackend == "" && len(cfg.Backends) == 1 {
		cfg.DefaultBackend = cfg.Backends[0].Name
	}
	if !seen[cfg.DefaultBackend] {
		return Config{}, fmt.Errorf("gateway: default_backend %q does not match any configured backend", cfg.DefaultBackend)
	}
	return cfg, nil
}
func validateBackend(b *BackendConfig) bool {
	if b.Type == "local" {
		return b.URL != "" && b.Token != ""
	}
	if b.Image = strings.TrimSpace(b.Image); b.Type != "ax" || b.Image == "" || b.URL != "" {
		return false
	}
	b.Atespace = cmp.Or(strings.TrimSpace(b.Atespace), "default")
	b.AXServer = cmp.Or(strings.TrimSpace(b.AXServer), "ax-server.ax-system:8080")
	b.Substrate = cmp.Or(strings.TrimSpace(b.Substrate), "api.ate-system.svc.cluster.local:443")
	b.Router = cmp.Or(strings.TrimSpace(b.Router), "atenet-router.ate-system:8444")
	b.Resources.CPU = cmp.Or(strings.TrimSpace(b.Resources.CPU), strings.TrimSpace(b.Resources.Limits.CPU), "4")
	b.Resources.Memory = cmp.Or(strings.TrimSpace(b.Resources.Memory), strings.TrimSpace(b.Resources.Limits.Memory), "8Gi")
	b.Resources.Limits = ResourceLimits{}
	if len(b.EgressHosts) == 0 {
		b.EgressHosts = slices.Clone(DefaultEgressHosts)
	}
	for i, h := range b.EgressHosts {
		if b.EgressHosts[i] = strings.TrimSpace(h); b.EgressHosts[i] == "" {
			return false
		}
	}
	return true
}
