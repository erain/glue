package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGlamourThemeJSONIsValid catches typos in glamour-mocha.json /
// glamour-latte.json at unit-test time, before the renderer silently
// falls back to AutoStyle in production.
func TestGlamourThemeJSONIsValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		data []byte
	}{
		{"mocha", glamourMochaJSON},
		{"latte", glamourLatteJSON},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var probe map[string]any
			if err := json.Unmarshal(c.data, &probe); err != nil {
				t.Fatalf("%s json: %v", c.name, err)
			}
			// Sanity-check the keys the assistant turns most depend on.
			for _, key := range []string{"document", "heading", "code", "code_block", "strong", "emph", "link"} {
				if _, ok := probe[key]; !ok {
					t.Errorf("%s json missing %q", c.name, key)
				}
			}
		})
	}
}

// TestMarkdownRendererRendersNonEmpty asserts the renderer constructs
// successfully under the Catppuccin theme and emits a styled (ANSI-
// containing) output rather than degrading to plain text.
func TestMarkdownRendererRendersNonEmpty(t *testing.T) {
	t.Parallel()
	r := newMarkdownRenderer(80)
	if r.glam == nil {
		t.Fatal("glamour renderer is nil — theme JSON likely failed to parse")
	}
	in := "# hello\n\nthis is **bold** and `inline`.\n"
	out := r.Render(in)
	if out == "" {
		t.Fatal("Render returned empty")
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("rendered output missing heading text: %q", out)
	}
	// ANSI escape — proof we got styling rather than plain markdown.
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected ANSI styling in output, got plain: %q", out)
	}
}

func TestMarkdownRendererFallsBackOnEmpty(t *testing.T) {
	t.Parallel()
	r := newMarkdownRenderer(80)
	if got := r.Render(""); got != "" {
		t.Errorf("Render(\"\") = %q, want empty", got)
	}
	if got := r.Render("   "); got != "   " {
		t.Errorf("Render whitespace = %q, want passthrough", got)
	}
}
