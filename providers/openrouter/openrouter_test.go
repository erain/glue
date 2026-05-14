package openrouter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue/loop"
)

// openrouter.New is a thin wrapper over providers/openaicompat. Shared
// streaming/convert/SSE behaviors are covered in that package's tests;
// these tests focus on vendor-specific concerns: the default attribution
// headers and the live integration against openrouter.ai.

func TestNewSetsDefaults(t *testing.T) {
	if defaultBaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("defaultBaseURL drift: %s", defaultBaseURL)
	}
	if apiKeyEnv != "OPENROUTER_API_KEY" {
		t.Fatalf("apiKeyEnv drift: %s", apiKeyEnv)
	}
}

func TestRequestSendsAttributionHeaders(t *testing.T) {
	server, headers := newCapturingServer(t)
	provider := New(Options{APIKey: "k", BaseURL: server.URL, HTTPClient: server.Client()})
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(ch)

	if got := headers().Get("HTTP-Referer"); got != defaultRefererURL {
		t.Fatalf("HTTP-Referer: got %q want %q", got, defaultRefererURL)
	}
	if got := headers().Get("X-Title"); got != defaultTitle {
		t.Fatalf("X-Title: got %q want %q", got, defaultTitle)
	}
}

func TestUserHeadersOverrideDefaults(t *testing.T) {
	server, headers := newCapturingServer(t)
	provider := New(Options{
		APIKey:     "k",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Headers:    map[string]string{"X-Title": "my-app"},
	})
	ch, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(ch)

	if got := headers().Get("X-Title"); got != "my-app" {
		t.Fatalf("X-Title override: got %q", got)
	}
	if got := headers().Get("HTTP-Referer"); got != defaultRefererURL {
		t.Fatalf("HTTP-Referer should keep default: got %q", got)
	}
}

func TestStreamUsesAPIKeyEnv(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	provider := New(Options{})
	_, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatalf("expected OPENROUTER_API_KEY hint in error, got %v", err)
	}
}

// TestLiveSmoke runs against the real OpenRouter endpoint when
// OPENROUTER_API_KEY is set. Defaults to inclusionai/ling-2.6-1t:free —
// a deterministic free model whose upstream (Novita) is consistently
// available. Better-known free routes (google/gemma-4-*:free,
// minimax/minimax-m2.5:free) are over-subscribed and frequently 429 at
// the upstream. Local devs can swap to the openrouter/free meta-route
// (which auto-routes around 429s but is non-deterministic) via
// OPENROUTER_LIVE_MODEL.
func TestLiveSmoke(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set; skipping live smoke")
	}
	model := os.Getenv("OPENROUTER_LIVE_MODEL")
	if model == "" {
		model = "inclusionai/ling-2.6-1t:free"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	provider := New(Options{APIKey: apiKey, HTTPClient: &http.Client{Timeout: 240 * time.Second}})
	ch, err := provider.Stream(ctx, loop.ProviderRequest{
		Model: model,
		Messages: []loop.Message{
			{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "Reply with the single word: glue."}}},
		},
		Options: map[string]any{"max_tokens": 16, "temperature": 0.0},
	})
	if err != nil {
		if isUpstream429(err.Error()) {
			t.Skipf("upstream rate-limited (free tier): %v", err)
		}
		t.Fatalf("Stream: %v", err)
	}

	var text, thinking strings.Builder
	var done *loop.Message
	deadline := time.After(240 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("live smoke timed out; text=%q thinking-len=%d", text.String(), thinking.Len())
		case event, ok := <-ch:
			if !ok {
				if done == nil {
					t.Fatalf("channel closed without Done event; text=%q", text.String())
				}
				// Default model (inclusionai/ling-2.6-1t:free) emits visible
				// text. When OPENROUTER_LIVE_MODEL points at the
				// non-deterministic openrouter/free meta-route, some upstreams
				// emit only reasoning — accept thinking as a fallback.
				if strings.TrimSpace(text.String()) == "" && thinking.Len() == 0 {
					t.Fatalf("expected non-empty text or thinking; got neither (done=%+v)", done)
				}
				upstream, _ := done.Metadata["upstream_provider"].(string)
				t.Logf("%s reply: %q (upstream=%s thinking-len=%d usage=%+v)",
					model, text.String(), upstream, thinking.Len(), done.Usage)
				return
			}
			switch event.Type {
			case loop.ProviderEventTextDelta:
				text.WriteString(event.Delta)
			case loop.ProviderEventThinkingDelta:
				thinking.WriteString(event.Delta)
			case loop.ProviderEventDone:
				done = event.Message
			case loop.ProviderEventError:
				if isUpstream429(event.Error) {
					t.Skipf("upstream rate-limited (free tier): %s", event.Error)
				}
				t.Fatalf("provider error: %s", event.Error)
			}
		}
	}
}

// helpers --------------------------------------------------------------

func newCapturingServer(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var captured http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, func() http.Header { return captured }
}

func drain(ch <-chan loop.ProviderEvent) {
	for range ch {
	}
}

// isUpstream429 reports whether an error message looks like an OpenRouter
// upstream rate-limit response. Free routes (e.g. inclusionai/ling-2.6-1t:free)
// share a 20 req/min quota and 429 frequently; we treat those as a skip so
// transient upstream limits don't fail CI, while real wire-protocol
// regressions (other HTTP codes, malformed SSE) still fail loudly.
func isUpstream429(msg string) bool {
	return strings.Contains(msg, "http 429") || strings.Contains(msg, "Rate limit exceeded")
}
