// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	MaxAXTaskYAMLBytes   = 31 << 10 // 31 KiB limit on encoded AX Task YAML
	MaxAXTaskConfigBytes = 16 << 10 // 16 KiB limit on encoded spec.config
)

// JobSpecFromAXTaskYAML leniently decodes an AX Task YAML document and extracts
// its spec.config as a job Spec, enforcing the 31 KiB task and 16 KiB config limits.
func JobSpecFromAXTaskYAML(raw []byte) (Spec, error) {
	if len(raw) > MaxAXTaskYAMLBytes {
		return Spec{}, fmt.Errorf("jobs: ax task yaml is %d bytes, exceeding the %d-byte limit", len(raw), MaxAXTaskYAMLBytes)
	}
	var doc struct {
		Spec struct {
			Workspace struct {
				Path string `yaml:"path"`
			} `yaml:"workspace"`
			Config map[string]any `yaml:"config"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Spec{}, fmt.Errorf("jobs: decode ax task yaml: %w", err)
	}
	if len(doc.Spec.Config) == 0 {
		return Spec{}, errors.New("jobs: ax task yaml is missing spec.config")
	}
	cfgYAML, err := yaml.Marshal(doc.Spec.Config)
	if err != nil {
		return Spec{}, err
	}
	if rawVerify, ok := doc.Spec.Config["verify"].([]any); ok {
		for i, item := range rawVerify {
			if s, ok := item.(string); ok {
				rawVerify[i] = strings.Fields(s)
			}
		}
	}
	cfgJSON, err := json.Marshal(doc.Spec.Config)
	if err != nil {
		return Spec{}, err
	}
	if size := max(len(cfgJSON), len(cfgYAML)); size > MaxAXTaskConfigBytes {
		return Spec{}, fmt.Errorf("jobs: ax spec.config is %d bytes, exceeding the %d-byte limit", size, MaxAXTaskConfigBytes)
	}
	var spec Spec
	if err := json.Unmarshal(cfgJSON, &spec); err != nil {
		return Spec{}, fmt.Errorf("jobs: decode ax spec.config: %w", err)
	}
	if strings.TrimSpace(spec.Objective) == "" {
		return Spec{}, errors.New("jobs: ax spec.config.objective is required")
	}
	if strings.TrimSpace(spec.Repo.Path) == "" && strings.TrimSpace(spec.Repo.URL) == "" {
		spec.Repo.Path = cmp.Or(strings.TrimSpace(doc.Spec.Workspace.Path), "/workspace")
	}
	return spec, nil
}

// LoadAXTaskYAML reads the AX Task YAML from $AX_METADATA_URL/metadata/v1alpha1/ax/task
// when AX_METADATA_URL is set, or from AX_TASK_YAML otherwise.
func LoadAXTaskYAML(ctx context.Context, client *http.Client) ([]byte, error) {
	if base := strings.TrimSpace(os.Getenv("AX_METADATA_URL")); base != "" {
		if client == nil {
			client = &http.Client{Timeout: 5 * time.Second}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/metadata/v1alpha1/ax/task", nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("jobs: fetch ax task metadata: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("jobs: fetch ax task metadata: status %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, MaxAXTaskYAMLBytes+1))
	}
	if raw := os.Getenv("AX_TASK_YAML"); strings.TrimSpace(raw) != "" {
		return []byte(raw), nil
	}
	return nil, errors.New("jobs: AX_METADATA_URL or AX_TASK_YAML is required for --job-from-ax")
}

// EnsureJobFromAX creates a job from AX task metadata or AX_TASK_YAML if no job
// exists in m yet. It never starts the job and is idempotent across restarts.
func (m *Manager) EnsureJobFromAX(ctx context.Context, client *http.Client) (Job, error) {
	raw, err := LoadAXTaskYAML(ctx, client)
	if err != nil {
		return Job{}, err
	}
	spec, err := JobSpecFromAXTaskYAML(raw)
	if err != nil {
		return Job{}, err
	}
	existing, err := m.List(ctx)
	if err != nil || len(existing) > 0 {
		if len(existing) > 0 {
			return existing[0], nil
		}
		return Job{}, err
	}
	return m.Create(ctx, spec)
}
