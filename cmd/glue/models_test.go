package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestModelsCommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		valid bool
	}{
		{"all", nil, true}, {"gemini", []string{"--provider", "gemini", "--json"}, true},
		{"unknown", []string{"--provider", "missing"}, false}, {"extra", []string{"extra"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			err := modelsCommand(tc.args, &out, &errs)
			if (err == nil) != tc.valid {
				t.Fatalf("err=%v", err)
			}
			if tc.name == "gemini" {
				var entries []catalogProvider
				if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 || entries[0].EnvKey != "GEMINI_API_KEY" || len(entries[0].Models) < 2 {
					t.Fatal(out.String())
				}
			}
			if tc.name == "all" && !strings.Contains(out.String(), "gemini-4-argon") {
				t.Fatal(out.String())
			}
		})
	}
	var out, errs bytes.Buffer
	code := runCLI(context.Background(), []string{"models"}, &out, &errs, func(string) (glue.Provider, error) { panic("must not construct provider") })
	if code != 0 {
		t.Fatal(errs.String())
	}
}
