// Package modelselection resolves activity-specific provider and model defaults.
package modelselection

import (
	"fmt"
	"strings"

	"github.com/erain/glue/providers"
)

// Default returns provider and model defaults for the selected activity.
func Default(coding bool) (string, string) {
	if coding {
		return "gemini", "gemini-3.8-flash"
	}
	return "openrouter", "openrouter/free"
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
