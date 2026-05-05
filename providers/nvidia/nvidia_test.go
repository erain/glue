package nvidia

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue/loop"
)

// nvidia.New is a thin wrapper over providers/openaicompat. The shared
// streaming/convert/SSE behaviors are exercised in that package's tests;
// these tests cover only the vendor-specific defaults and the live
// integration against integrate.api.nvidia.com.

func TestNewSetsDefaults(t *testing.T) {
	if defaultBaseURL != "https://integrate.api.nvidia.com/v1" {
		t.Fatalf("defaultBaseURL drift: %s", defaultBaseURL)
	}
	if apiKeyEnv != "NVIDIA_API_KEY" {
		t.Fatalf("apiKeyEnv drift: %s", apiKeyEnv)
	}
	if providerName != "nvidia" {
		t.Fatalf("providerName drift: %s", providerName)
	}
}

func TestStreamUsesAPIKeyEnv(t *testing.T) {
	t.Setenv("NVIDIA_API_KEY", "")
	provider := New(Options{})
	_, err := provider.Stream(context.Background(), loop.ProviderRequest{Model: "x"})
	if err == nil || !strings.Contains(err.Error(), "NVIDIA_API_KEY") {
		t.Fatalf("expected NVIDIA_API_KEY hint in error, got %v", err)
	}
}

// TestLiveSmoke runs against the real NVIDIA endpoint when NVIDIA_API_KEY
// is set. Defaults to moonshotai/kimi-k2.6 (the package's flagship target);
// CI overrides to a faster, more stable model via NVIDIA_LIVE_MODEL.
func TestLiveSmoke(t *testing.T) {
	apiKey := os.Getenv("NVIDIA_API_KEY")
	if apiKey == "" {
		t.Skip("NVIDIA_API_KEY not set; skipping live smoke")
	}
	model := os.Getenv("NVIDIA_LIVE_MODEL")
	if model == "" {
		model = "moonshotai/kimi-k2.6"
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
		t.Fatalf("Stream: %v", err)
	}

	var text strings.Builder
	var done *loop.Message
	deadline := time.After(240 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("live smoke timed out; partial=%q", text.String())
		case event, ok := <-ch:
			if !ok {
				if done == nil {
					t.Fatalf("channel closed without Done event; partial=%q", text.String())
				}
				if strings.TrimSpace(text.String()) == "" {
					t.Fatalf("expected non-empty assistant text; got %q", text.String())
				}
				t.Logf("%s reply: %q (usage=%+v)", model, text.String(), done.Usage)
				return
			}
			switch event.Type {
			case loop.ProviderEventTextDelta:
				text.WriteString(event.Delta)
			case loop.ProviderEventDone:
				done = event.Message
			case loop.ProviderEventError:
				t.Fatalf("provider error: %s", event.Error)
			}
		}
	}
}
