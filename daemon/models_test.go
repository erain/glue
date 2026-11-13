package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/erain/glue/loop"
	"github.com/erain/glue/providers"
)

func TestModelsCatalogEndpoint(t *testing.T) {
	s := eventServer(t, t.TempDir())
	factory, ok := providers.Lookup("gemini")
	if !ok {
		t.Fatal("Gemini not registered")
	}
	original := factory
	factory.New = func() loop.Provider { t.Fatal("catalog constructed provider"); return nil }
	providers.Register("gemini", factory)
	t.Cleanup(func() { providers.Register("gemini", original) })
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if authorized {
			req.Header.Set("Authorization", "Bearer token")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if !authorized {
			if w.Code != 401 {
				t.Fatal(w.Code)
			}
			continue
		}
		if w.Code != 200 {
			t.Fatalf("status %d %s", w.Code, w.Body.String())
		}
		var body modelCatalog
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Defaults["chat"].Model != "openrouter/free" || body.Defaults["coding"].Model != "gemini-3-flash-preview" {
			t.Fatal(body)
		}
		found := false
		for _, p := range body.Providers {
			if p.Provider == "gemini" {
				for _, m := range p.Models {
					if m.ID == "gemini-3-flash-preview" && len(m.ThinkingLevels) >= 3 {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatal("missing thinking catalog")
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}
