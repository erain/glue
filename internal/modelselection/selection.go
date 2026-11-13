// Package modelselection resolves activity-specific provider and model defaults.
package modelselection

import (
	"fmt"
	"strings"

	"github.com/erain/glue/providers"
)

// Coding defaults. gemini-3-flash-preview at low thinking measured about
// 1 s per model turn on remote jobs, against 14-17 s for gemini-3.8-flash.
const (
	CodingProvider = "gemini"
	CodingModel    = "gemini-3-flash-preview"
	CodingThinking = "low"
)

// Default returns provider and model defaults for the selected activity.
func Default(coding bool) (string, string) {
	if coding {
		return CodingProvider, CodingModel
	}
	return "openrouter", "openrouter/free"
}

// DefaultThinking returns the thinking level to use when none was chosen:
// CodingThinking for the default coding model, otherwise "" (the API default).
func DefaultThinking(provider, model, thinking string) string {
	if strings.TrimSpace(thinking) != "" {
		return thinking
	}
	if strings.EqualFold(strings.TrimSpace(provider), CodingProvider) && strings.TrimPrefix(strings.TrimSpace(model), "models/") == CodingModel {
		return CodingThinking
	}
	return thinking
}

// Resolve combines activity defaults with explicit selections.
func Resolve(coding bool, name, model string) (string, string, error) {
	p, m := Default(coding)
	if strings.TrimSpace(name) == "" {
		name = p
		if strings.TrimSpace(model) == "" {
			model = m
		}
	}
	return Provider(name, model)
}

// Provider validates the provider name against the registry and
// resolves the effective model: an explicit --model wins, otherwise the
// provider's registry default model is used.
func Provider(name, model string) (string, string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name, _ = Default(false)
	}
	factory, ok := providers.Lookup(name)
	if !ok {
		return "", "", fmt.Errorf("unknown provider %q (known: %s)", name, strings.Join(providers.Known(), ", "))
	}
	if strings.TrimSpace(model) == "" {
		model = factory.DefaultModel
	}
	return name, model, nil
}
