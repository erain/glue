package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDefaultSelectionByMode(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		coding                                   bool
		provider, model, wantProvider, wantModel string
	}{
		{"run", false, "", "", "openrouter", "openrouter/free"},
		{"coding run", true, "", "", "gemini", "gemini-3-flash-preview"},
		{"goal", true, "", "", "gemini", "gemini-3-flash-preview"},
		{"serve", false, "", "", "openrouter", "openrouter/free"},
		{"coding serve", true, "", "", "gemini", "gemini-3-flash-preview"},
		{"provider override", true, "codex", "", "codex", "gpt-5-codex"},
		{"chat override", false, "gemini", "custom", "gemini", "custom"},
		{"explicit", true, "openrouter", "custom", "openrouter", "custom"},
		{"model", false, "", "custom", "openrouter", "custom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, m, err := resolveSelection(tc.coding, tc.provider, tc.model)
			if err != nil || p != tc.wantProvider || m != tc.wantModel {
				t.Fatalf("got %s/%s, %v", p, m, err)
			}
		})
	}
}

func TestModelsLabelsModeDefaults(t *testing.T) {
	var out, errs bytes.Buffer
	if err := modelsCommand(nil, &out, &errs); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"gemini-3-flash-preview", "openrouter/free"} {
		found := false
		for _, row := range strings.Split(out.String(), "\n") {
			if strings.Contains(row, line) && (strings.Contains(row, "coding") || strings.Contains(row, "chat")) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing default marker for %s: %s", line, out.String())
		}
	}
}
