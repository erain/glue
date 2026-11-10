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

	"gopkg.in/yaml.v3"
)

const (
	DefaultSuspendAfter  = 2 * time.Minute
	DefaultResumeTimeout = time.Minute
)

// DefaultEgressHosts is the default TLS passthrough allowlist for AX tasks.
var DefaultEgressHosts = []string{
	"github.com", "api.github.com", "codeload.github.com",
	"proxy.golang.org", "sum.golang.org", "storage.googleapis.com",
	"generativelanguage.googleapis.com", "openrouter.ai",
}

// AuthConfig configures client authentication for glue-gateway.
type AuthConfig struct{ Mode, Token string }

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
	SuspendOnFinish                                *bool           `yaml:"suspend_on_finish" json:"suspend_on_finish,omitempty"`
}

// Config is the top-level YAML configuration for glue-gateway.
type Config struct {
	Listen         string          `yaml:"listen"`
	PublicURL      string          `yaml:"public_url"`
	StateDir       string          `yaml:"state_dir"`
	Auth           AuthConfig      `yaml:"auth"`
	Backends       []BackendConfig `yaml:"backends"`
	DefaultBackend string          `yaml:"default_backend"`
	// Task suspension settings and test hooks.
	SuspendAfter    time.Duration      `yaml:"suspend_after"`
	ResumeTimeout   time.Duration      `yaml:"resume_timeout"`
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
func ParseConfig(data []byte) (Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("gateway: parse config: %w", err)
	}
	return cfg.Validate()
}
func (cfg Config) Validate() (Config, error) {
	cfg.Listen = cmp.Or(strings.TrimSpace(cfg.Listen), "127.0.0.1:8800")
	cfg.PublicURL = strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/")
	cfg.StateDir = strings.TrimSpace(cfg.StateDir)
	cfg.Auth.Mode = cmp.Or(strings.TrimSpace(cfg.Auth.Mode), "dev-token")
	cfg.Auth.Token = cmp.Or(strings.TrimSpace(cfg.Auth.Token), strings.TrimSpace(os.Getenv("GLUE_GATEWAY_TOKEN")))
	if envSA := strings.TrimSpace(os.Getenv("GLUE_SUSPEND_AFTER")); cfg.SuspendAfter == 0 && envSA != "" {
		if d, err := time.ParseDuration(envSA); err == nil && d > 0 {
			cfg.SuspendAfter = d
		} else {
			return Config{}, fmt.Errorf("gateway: invalid GLUE_SUSPEND_AFTER %q", envSA)
		}
	}
	cfg.SuspendAfter, cfg.ResumeTimeout = cmp.Or(cfg.SuspendAfter, DefaultSuspendAfter), cmp.Or(cfg.ResumeTimeout, DefaultResumeTimeout)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SuspendAfter < 0 || cfg.ResumeTimeout < 0 || cfg.StateDir == "" || cfg.Auth.Mode != "dev-token" || cfg.Auth.Token == "" || len(cfg.Backends) == 0 {
		return Config{}, fmt.Errorf("gateway: invalid config (state_dir=%q, auth.mode=%q, has_token=%v, backends=%d)", cfg.StateDir, cfg.Auth.Mode, cfg.Auth.Token != "", len(cfg.Backends))
	}
	seen := make(map[string]bool, len(cfg.Backends))
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		b.Name, b.Type = strings.TrimSpace(b.Name), strings.TrimSpace(b.Type)
		b.URL, b.Token = strings.TrimRight(strings.TrimSpace(b.URL), "/"), strings.TrimSpace(b.Token)
		if b.SuspendAfter < 0 || b.ResumeTimeout < 0 || b.Name == "" || seen[b.Name] || !validateBackend(b) {
			return Config{}, fmt.Errorf("gateway: invalid backend %q (duplicate=%v, type=%q)", b.Name, seen[b.Name], b.Type)
		}
		if b.Type == "ax" {
			b.SuspendAfter, b.ResumeTimeout = cmp.Or(b.SuspendAfter, cfg.SuspendAfter), cmp.Or(b.ResumeTimeout, cfg.ResumeTimeout)
			if b.SuspendOnFinish == nil {
				b.SuspendOnFinish = cfg.SuspendOnFinish
			}
			cfg.SuspendAfter, cfg.ResumeTimeout, cfg.SuspendOnFinish = b.SuspendAfter, b.ResumeTimeout, b.SuspendOnFinish
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
