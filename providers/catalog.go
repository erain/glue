package providers

import (
	"fmt"
	"slices"
	"sort"
)

// ModelInfo describes a curated model choice. An empty ThinkingLevels list
// means Glue has no verified thinking control for this model.
type ModelInfo struct {
	ID             string   `json:"id"`
	ThinkingLevels []string `json:"thinking_levels,omitempty"`
}

// Models returns curated choices, including the registered default. It does
// not contact the provider or imply that an account has access to a model.
// Unknown model IDs remain valid for requests without a thinking override.
func Models(name string) []ModelInfo {
	f, ok := Lookup(name)
	if !ok {
		return nil
	}
	models := make([]ModelInfo, 0, len(f.Models)+1)
	foundDefault := false
	for _, m := range f.Models {
		m.ThinkingLevels = slices.Clone(m.ThinkingLevels)
		models = append(models, m)
		foundDefault = foundDefault || m.ID == f.DefaultModel
	}
	if !foundDefault && f.DefaultModel != "" {
		models = append(models, ModelInfo{ID: f.DefaultModel})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

// ThinkingOptions validates an explicit thinking level against the selected
// model. Empty or "default" preserves the provider's API default.
func ThinkingOptions(provider, model, level string) (map[string]any, error) {
	if level == "" || level == "default" {
		return nil, nil
	}
	for _, m := range Models(provider) {
		if m.ID == model && slices.Contains(m.ThinkingLevels, level) {
			return map[string]any{"thinking_level": level}, nil
		}
	}
	return nil, fmt.Errorf("thinking level %q is unsupported for %s model %q; use 'glue models --provider %s' to see supported levels", level, provider, model, provider)
}
