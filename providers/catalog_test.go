package providers

import "testing"

func TestCatalog(t *testing.T) {
	Register("catalog-test", Factory{DefaultModel: "default", Models: []ModelInfo{{ID: "other", ThinkingLevels: []string{"low", "high"}}}})
	t.Cleanup(func() { mu.Lock(); delete(factories, "catalog-test"); mu.Unlock() })
	models := Models("catalog-test")
	if len(models) != 2 || models[0].ID != "default" || models[1].ID != "other" {
		t.Fatalf("models = %#v", models)
	}
	models[1].ThinkingLevels[0] = "mutated"
	if Models("catalog-test")[1].ThinkingLevels[0] != "low" {
		t.Fatal("catalog mutated")
	}
	for _, tc := range []struct {
		model, level string
		valid        bool
	}{
		{"other", "low", true}, {"other", "high", true}, {"other", "medium", false}, {"unknown", "low", false}, {"unknown", "", true}, {"unknown", "default", true},
	} {
		t.Run(tc.model+"/"+tc.level, func(t *testing.T) {
			opts, err := ThinkingOptions("catalog-test", tc.model, tc.level)
			if (err == nil) != tc.valid {
				t.Fatalf("options=%v error=%v", opts, err)
			}
			if tc.valid && tc.level == "low" && opts["thinking_level"] != "low" {
				t.Fatal(opts)
			}
		})
	}
	if Models("missing") != nil {
		t.Fatal("unknown provider has models")
	}
}
