// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package modelselection

import "testing"

func TestDefaultThinking(t *testing.T) {
	for _, tc := range []struct{ provider, model, thinking, want string }{
		{CodingProvider, CodingModel, "", CodingThinking},
		{CodingProvider, "models/" + CodingModel, "", CodingThinking},
		{CodingProvider, CodingModel, "high", "high"},
		{CodingProvider, "gemini-3.8-flash", "", ""},
		{"openrouter", "openrouter/free", "", ""},
	} {
		if got := DefaultThinking(tc.provider, tc.model, tc.thinking); got != tc.want {
			t.Fatalf("DefaultThinking(%q, %q, %q) = %q, want %q", tc.provider, tc.model, tc.thinking, got, tc.want)
		}
	}
	if p, m := Default(true); p != "gemini" || m != "gemini-3-flash-preview" {
		t.Fatalf("coding default = %s/%s", p, m)
	}
}
