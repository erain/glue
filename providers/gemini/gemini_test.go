package gemini

import (
	"context"
	"os"
	"strings"
	"testing"

	"glue/loop"

	"google.golang.org/genai"
)

func TestConvertMessagesUserAndAssistantText(t *testing.T) {
	t.Parallel()

	contents, err := ConvertMessages([]loop.Message{
		{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hi"}}},
		{Role: loop.MessageRoleAssistant, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hello"}}},
		{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "more?"}}},
	})
	if err != nil {
		t.Fatalf("ConvertMessages: %v", err)
	}
	if len(contents) != 3 {
		t.Fatalf("len(contents) = %d, want 3", len(contents))
	}
	wantRoles := []genai.Role{genai.RoleUser, genai.RoleModel, genai.RoleUser}
	for i, want := range wantRoles {
		if contents[i].Role != string(want) {
			t.Fatalf("contents[%d].Role = %q, want %q", i, contents[i].Role, want)
		}
	}
	if got := contents[0].Parts[0].Text; got != "hi" {
		t.Fatalf("contents[0].Parts[0].Text = %q, want hi", got)
	}
}

func TestConvertMessagesEmptyTextDropsMessage(t *testing.T) {
	t.Parallel()

	contents, err := ConvertMessages([]loop.Message{
		{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: ""}}},
		{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hi"}}},
	})
	if err != nil {
		t.Fatalf("ConvertMessages: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("len(contents) = %d, want 1 (empty message dropped)", len(contents))
	}
}

func TestConvertMessagesToolCallNotYetSupported(t *testing.T) {
	t.Parallel()

	_, err := ConvertMessages([]loop.Message{
		{Role: loop.MessageRoleAssistant, Content: []loop.ContentPart{
			{Type: loop.ContentTypeToolCall, ToolCall: &loop.ToolCall{ID: "x", Name: "y"}},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "function-calling") {
		t.Fatalf("err = %v, want function-calling not implemented", err)
	}
}

func TestConvertMessagesToolRoleNotYetSupported(t *testing.T) {
	t.Parallel()

	_, err := ConvertMessages([]loop.Message{
		{Role: loop.MessageRoleTool, ToolName: "x", Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "result"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "function-calling") {
		t.Fatalf("err = %v, want function-calling not implemented", err)
	}
}

func TestConvertMessagesUnsupportedRoleErrors(t *testing.T) {
	t.Parallel()

	_, err := ConvertMessages([]loop.Message{{Role: "weird", Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "x"}}}})
	if err == nil || !strings.Contains(err.Error(), "unsupported message role") {
		t.Fatalf("err = %v, want unsupported message role", err)
	}
}

func TestBuildGenerateConfigSetsSystemAndOptions(t *testing.T) {
	t.Parallel()

	cfg, err := buildGenerateConfig(loop.ProviderRequest{
		SystemPrompt: "be terse",
		Options: map[string]any{
			"temperature":       0.5,
			"max_output_tokens": 256,
		},
	})
	if err != nil {
		t.Fatalf("buildGenerateConfig: %v", err)
	}
	if cfg.SystemInstruction == nil || cfg.SystemInstruction.Parts[0].Text != "be terse" {
		t.Fatalf("SystemInstruction = %#v, want 'be terse'", cfg.SystemInstruction)
	}
	if cfg.Temperature == nil || *cfg.Temperature != float32(0.5) {
		t.Fatalf("Temperature = %v, want 0.5", cfg.Temperature)
	}
	if cfg.MaxOutputTokens != 256 {
		t.Fatalf("MaxOutputTokens = %d, want 256", cfg.MaxOutputTokens)
	}
}

func TestBuildGenerateConfigInvalidNumeric(t *testing.T) {
	t.Parallel()

	_, err := buildGenerateConfig(loop.ProviderRequest{Options: map[string]any{"temperature": "hot"}})
	if err == nil || !strings.Contains(err.Error(), "temperature") {
		t.Fatalf("err = %v, want temperature error", err)
	}
	_, err = buildGenerateConfig(loop.ProviderRequest{Options: map[string]any{"max_tokens": "lots"}})
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("err = %v, want max_tokens error", err)
	}
}

func TestMapFinishReason(t *testing.T) {
	t.Parallel()

	cases := map[genai.FinishReason]loop.StopReason{
		"":                              loop.StopReasonStop,
		genai.FinishReasonUnspecified:   loop.StopReasonStop,
		genai.FinishReasonStop:          loop.StopReasonStop,
		genai.FinishReasonMaxTokens:     loop.StopReasonLength,
		genai.FinishReasonSafety:        loop.StopReasonError,
		genai.FinishReasonRecitation:    loop.StopReasonError,
		genai.FinishReasonProhibitedContent: loop.StopReasonError,
	}
	for input, want := range cases {
		if got := mapFinishReason(input); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestApplyResponseMetadataWiresUsageAndIDs(t *testing.T) {
	t.Parallel()

	msg := loop.Message{Role: loop.MessageRoleAssistant, Metadata: map[string]any{}}
	applyResponseMetadata(&msg, &genai.GenerateContentResponse{
		ModelVersion: "gemini-2.5-flash-001",
		ResponseID:   "resp-abc",
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        10,
			CandidatesTokenCount:    7,
			ThoughtsTokenCount:      3,
			CachedContentTokenCount: 2,
			TotalTokenCount:         20,
		},
	})
	if msg.Model != "gemini-2.5-flash-001" {
		t.Fatalf("Model = %q, want gemini-2.5-flash-001", msg.Model)
	}
	if msg.Metadata["response_id"] != "resp-abc" {
		t.Fatalf("response_id = %v, want resp-abc", msg.Metadata["response_id"])
	}
	if msg.Usage == nil || msg.Usage.InputTokens != 10 || msg.Usage.OutputTokens != 10 || msg.Usage.CacheReadTokens != 2 || msg.Usage.TotalTokens != 20 {
		t.Fatalf("Usage = %#v, want input 10 output 10 cache 2 total 20", msg.Usage)
	}
}

func TestStreamRequiresModel(t *testing.T) {
	t.Parallel()

	p := New(Options{})
	_, err := p.Stream(context.Background(), loop.ProviderRequest{})
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("err = %v, want model is required", err)
	}
}

func TestStreamUsesDefaultModel(t *testing.T) {
	t.Parallel()

	// We don't actually want to hit the network; we just confirm the model
	// validation accepts the provider's DefaultModel and falls through to
	// genai client creation. The client creation will succeed even with a
	// bogus key (errors surface only on the actual API call), but we don't
	// exercise the stream goroutine here — we just check the synchronous
	// validation path doesn't reject DefaultModel.
	p := New(Options{DefaultModel: "gemini-test", APIKey: "fake"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ensures the stream goroutine exits immediately if it starts
	_, err := p.Stream(ctx, loop.ProviderRequest{
		Messages: []loop.Message{{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream with DefaultModel returned err = %v, want nil", err)
	}
}

func TestNewWithoutOptionsDoesNotPanic(t *testing.T) {
	t.Parallel()

	if got := New(Options{}); got == nil {
		t.Fatal("New returned nil")
	}
}

// TestLiveSmoke exercises a real Gemini call when GEMINI_API_KEY is set.
// CI never sets the variable, so this is a no-op there; it is the minimum
// proof that the streaming path works end-to-end.
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live smoke test")
	}

	p := New(Options{})
	events, err := p.Stream(context.Background(), loop.ProviderRequest{
		Model: "gemini-2.5-flash",
		Messages: []loop.Message{
			{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "Reply with the single word: glue"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var sawText bool
	var done *loop.Message
	for ev := range events {
		switch ev.Type {
		case loop.ProviderEventTextDelta:
			if ev.Delta != "" {
				sawText = true
			}
		case loop.ProviderEventDone:
			done = ev.Message
		case loop.ProviderEventError:
			t.Fatalf("provider error: %s", ev.Error)
		}
	}
	if !sawText {
		t.Fatal("never received a text_delta event")
	}
	if done == nil {
		t.Fatal("never received a done event with message")
	}
	if done.StopReason == "" {
		t.Fatal("done message missing stop reason")
	}
}
