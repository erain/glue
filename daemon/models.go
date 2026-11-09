package daemon

import (
	"net/http"

	"github.com/erain/glue/internal/modelselection"
	"github.com/erain/glue/providers"
	_ "github.com/erain/glue/providers/codex"
	_ "github.com/erain/glue/providers/deepseek"
	_ "github.com/erain/glue/providers/gemini"
	_ "github.com/erain/glue/providers/nvidia"
	_ "github.com/erain/glue/providers/openrouter"
)

type modelChoice struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type modelProvider struct {
	Provider     string                `json:"provider"`
	DefaultModel string                `json:"default_model"`
	Models       []providers.ModelInfo `json:"models"`
}
type modelCatalog struct {
	Providers []modelProvider        `json:"providers"`
	Defaults  map[string]modelChoice `json:"defaults"`
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	catalog := modelCatalog{Providers: []modelProvider{}, Defaults: map[string]modelChoice{}}
	for _, name := range providers.Known() {
		factory, _ := providers.Lookup(name)
		catalog.Providers = append(catalog.Providers, modelProvider{Provider: name, DefaultModel: factory.DefaultModel, Models: providers.Models(name)})
	}
	for mode, coding := range map[string]bool{"chat": false, "coding": true} {
		provider, model := modelselection.Default(coding)
		catalog.Defaults[mode] = modelChoice{Provider: provider, Model: model}
	}
	writeJSON(w, http.StatusOK, catalog)
}
