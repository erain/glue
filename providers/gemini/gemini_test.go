package gemini

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/erain/glue/loop"

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

func TestConvertMessagesToolCallContent(t *testing.T) {
	t.Parallel()

	contents, err := ConvertMessages([]loop.Message{
		{Role: loop.MessageRoleAssistant, Content: []loop.ContentPart{
			{Type: loop.ContentTypeToolCall, ToolCall: &loop.ToolCall{
				ID:        "call_1",
				Name:      "weather",
				Arguments: json.RawMessage(`{"city":"Toronto"}`),
			}},
		}},
	})
	if err != nil {
		t.Fatalf("ConvertMessages: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("len(contents) = %d, want 1", len(contents))
	}
	if contents[0].Role != string(genai.RoleModel) {
		t.Fatalf("role = %q, want model", contents[0].Role)
	}
	fc := contents[0].Parts[0].FunctionCall
	if fc == nil {
		t.Fatal("FunctionCall part missing")
	}
	if fc.Name != "weather" || fc.ID != "call_1" {
		t.Fatalf("FunctionCall = %#v, want name=weather id=call_1", fc)
	}
	if fc.Args["city"] != "Toronto" {
		t.Fatalf("Args[city] = %v, want Toronto", fc.Args["city"])
	}
}

func TestConvertMessagesToolResultGroupsConsecutiveAsOneContent(t *testing.T) {
	t.Parallel()

	contents, err := ConvertMessages([]loop.Message{
		{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "hi"}}},
		{Role: loop.MessageRoleTool, ToolCallID: "c1", ToolName: "weather", Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "sunny"}}},
		{Role: loop.MessageRoleTool, ToolCallID: "c2", ToolName: "time", Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "noon"}}, IsError: true},
	})
	if err != nil {
		t.Fatalf("ConvertMessages: %v", err)
	}
	if len(contents) != 2 {
		t.Fatalf("len(contents) = %d, want 2 (user + grouped tool responses)", len(contents))
	}
	tools := contents[1]
	if tools.Role != string(genai.RoleUser) {
		t.Fatalf("tool group role = %q, want user", tools.Role)
	}
	if len(tools.Parts) != 2 {
		t.Fatalf("tool group parts = %d, want 2", len(tools.Parts))
	}
	r1 := tools.Parts[0].FunctionResponse
	if r1 == nil || r1.Name != "weather" || r1.Response["output"] != "sunny" {
		t.Fatalf("response[0] = %#v, want weather output=sunny", r1)
	}
	r2 := tools.Parts[1].FunctionResponse
	if r2 == nil || r2.Name != "time" || r2.Response["error"] != "noon" {
		t.Fatalf("response[1] = %#v, want time error=noon", r2)
	}
}

func TestConvertToolsBuildsFunctionDeclarations(t *testing.T) {
	t.Parallel()

	tools, err := ConvertTools([]loop.ToolSpec{
		{
			Name:        "weather",
			Description: "Get weather for a city.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		},
		{Name: "time"},
	})
	if err != nil {
		t.Fatalf("ConvertTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1 (single bundle)", len(tools))
	}
	decls := tools[0].FunctionDeclarations
	if len(decls) != 2 {
		t.Fatalf("len(decls) = %d, want 2", len(decls))
	}
	if decls[0].Name != "weather" || decls[0].ParametersJsonSchema == nil {
		t.Fatalf("decls[0] = %#v, want weather with schema", decls[0])
	}
	if decls[1].Name != "time" || decls[1].ParametersJsonSchema != nil {
		t.Fatalf("decls[1] = %#v, want time with no schema", decls[1])
	}
}

func TestConvertToolsBadSchemaErrors(t *testing.T) {
	t.Parallel()

	_, err := ConvertTools([]loop.ToolSpec{{Name: "x", Parameters: json.RawMessage(`not-json`)}})
	if err == nil || !strings.Contains(err.Error(), "tool \"x\"") {
		t.Fatalf("err = %v, want tool x parameters error", err)
	}
}

func TestConvertFunctionCallFillsIDAndCleansNullArgs(t *testing.T) {
	t.Parallel()

	tc, err := convertFunctionCall(&genai.FunctionCall{Name: "weather"}, 7)
	if err != nil {
		t.Fatalf("convertFunctionCall: %v", err)
	}
	if tc.ID != "weather_7" {
		t.Fatalf("ID = %q, want weather_7", tc.ID)
	}
	if string(tc.Arguments) != "{}" {
		t.Fatalf("Arguments = %q, want {}", string(tc.Arguments))
	}

	tc2, err := convertFunctionCall(&genai.FunctionCall{ID: "abc", Name: "weather", Args: map[string]any{"city": "T"}}, 1)
	if err != nil {
		t.Fatalf("convertFunctionCall: %v", err)
	}
	if tc2.ID != "abc" {
		t.Fatalf("ID = %q, want abc", tc2.ID)
	}
	if !strings.Contains(string(tc2.Arguments), `"city":"T"`) {
		t.Fatalf("Arguments = %q, want {\"city\":\"T\"}", string(tc2.Arguments))
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
	}, "gemini-2.5-flash")
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

	_, err := buildGenerateConfig(loop.ProviderRequest{Options: map[string]any{"temperature": "hot"}}, "gemini-2.5-flash")
	if err == nil || !strings.Contains(err.Error(), "temperature") {
		t.Fatalf("err = %v, want temperature error", err)
	}
	_, err = buildGenerateConfig(loop.ProviderRequest{Options: map[string]any{"max_tokens": "lots"}}, "gemini-2.5-flash")
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("err = %v, want max_tokens error", err)
	}
}

func TestBuildGenerateConfigStructuredOutput(t *testing.T) {
	t.Parallel()

	cfg, err := buildGenerateConfig(loop.ProviderRequest{
		Options: map[string]any{
			"response_mime_type":   "application/json",
			"response_json_schema": map[string]any{"type": "object"},
		},
	}, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("buildGenerateConfig: %v", err)
	}
	if cfg.ResponseMIMEType != "application/json" {
		t.Fatalf("ResponseMIMEType = %q, want application/json", cfg.ResponseMIMEType)
	}
	asMap, ok := cfg.ResponseJsonSchema.(map[string]any)
	if !ok {
		t.Fatalf("ResponseJsonSchema = %T, want map[string]any", cfg.ResponseJsonSchema)
	}
	if asMap["type"] != "object" {
		t.Fatalf("schema.type = %v, want object", asMap["type"])
	}
}

func TestBuildGenerateConfigBadResponseMIMEType(t *testing.T) {
	t.Parallel()

	_, err := buildGenerateConfig(loop.ProviderRequest{Options: map[string]any{"response_mime_type": 42}}, "gemini-2.5-flash")
	if err == nil || !strings.Contains(err.Error(), "response_mime_type") {
		t.Fatalf("err = %v, want response_mime_type error", err)
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

func TestEncodeDecodeSignatureRoundTrip(t *testing.T) {
	t.Parallel()

	if encodeSignature(nil) != "" || encodeSignature([]byte{}) != "" {
		t.Fatal("empty signature should encode to empty string")
	}
	sig := []byte{0x00, 0x01, 0xfe, 0xff, 'a', 'b'}
	enc := encodeSignature(sig)
	if enc == "" {
		t.Fatal("non-empty signature encoded to empty string")
	}
	if got := decodeSignature(enc); string(got) != string(sig) {
		t.Fatalf("round-trip = %v, want %v", got, sig)
	}
	if decodeSignature("") != nil {
		t.Fatal("empty string should decode to nil")
	}
	if decodeSignature("not!base64!!") != nil {
		t.Fatal("corrupt signature should decode to nil, not panic")
	}
}

func TestIsModernGeminiModel(t *testing.T) {
	t.Parallel()

	modern := []string{"gemini-3.1-pro-preview", "gemini-3-pro-preview", "gemini-3-flash"}
	legacy := []string{"gemini-2.5-flash", "gemini-2.5-pro", "gemini-pro-latest", ""}
	for _, m := range modern {
		if !isModernGeminiModel(m) {
			t.Errorf("isModernGeminiModel(%q) = false, want true", m)
		}
	}
	for _, m := range legacy {
		if isModernGeminiModel(m) {
			t.Errorf("isModernGeminiModel(%q) = true, want false", m)
		}
	}
}

func TestBuildGenerateConfigIncludeThoughts(t *testing.T) {
	t.Parallel()

	modern, err := buildGenerateConfig(loop.ProviderRequest{}, "gemini-3.1-pro-preview")
	if err != nil {
		t.Fatalf("buildGenerateConfig modern: %v", err)
	}
	if modern.ThinkingConfig == nil || !modern.ThinkingConfig.IncludeThoughts {
		t.Fatalf("ThinkingConfig = %#v, want IncludeThoughts=true for gemini-3.x", modern.ThinkingConfig)
	}

	legacy, err := buildGenerateConfig(loop.ProviderRequest{}, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("buildGenerateConfig legacy: %v", err)
	}
	if legacy.ThinkingConfig != nil {
		t.Fatalf("ThinkingConfig = %#v, want nil for non-reasoning id", legacy.ThinkingConfig)
	}
}

func TestConvertMessagesEchoesToolCallSignature(t *testing.T) {
	t.Parallel()

	sig := []byte("the-opaque-signature-bytes")
	contents, err := ConvertMessages([]loop.Message{
		{Role: loop.MessageRoleAssistant, Content: []loop.ContentPart{
			{Type: loop.ContentTypeToolCall, Signature: encodeSignature(sig), ToolCall: &loop.ToolCall{
				ID: "c1", Name: "weather", Arguments: json.RawMessage(`{"city":"T"}`),
			}},
		}},
	})
	if err != nil {
		t.Fatalf("ConvertMessages: %v", err)
	}
	part := contents[0].Parts[0]
	if part.FunctionCall == nil {
		t.Fatal("FunctionCall part missing")
	}
	if string(part.ThoughtSignature) != string(sig) {
		t.Fatalf("ThoughtSignature = %q, want %q", part.ThoughtSignature, sig)
	}
}

func TestConvertMessagesEchoesThinkingSignatureAsThought(t *testing.T) {
	t.Parallel()

	sig := []byte("thinking-signature")
	contents, err := ConvertMessages([]loop.Message{
		{Role: loop.MessageRoleAssistant, Content: []loop.ContentPart{
			{Type: loop.ContentTypeThinking, Thinking: "let me think", Signature: encodeSignature(sig)},
		}},
	})
	if err != nil {
		t.Fatalf("ConvertMessages: %v", err)
	}
	part := contents[0].Parts[0]
	if !part.Thought {
		t.Fatal("thinking content must convert to a part with Thought=true")
	}
	if part.Text != "let me think" {
		t.Fatalf("Text = %q, want 'let me think'", part.Text)
	}
	if string(part.ThoughtSignature) != string(sig) {
		t.Fatalf("ThoughtSignature = %q, want %q", part.ThoughtSignature, sig)
	}
}

func TestAppendThinkingLatchesSignature(t *testing.T) {
	t.Parallel()

	msg := &loop.Message{}
	appendThinking(msg, "reasoning ", "")    // text, no signature yet
	appendThinking(msg, "continues", "")     // coalesces
	appendThinking(msg, "", "c2ln")          // signature-only terminator latches
	if len(msg.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1 coalesced thinking block", len(msg.Content))
	}
	if msg.Content[0].Thinking != "reasoning continues" {
		t.Fatalf("Thinking = %q, want 'reasoning continues'", msg.Content[0].Thinking)
	}
	if msg.Content[0].Signature != "c2ln" {
		t.Fatalf("Signature = %q, want latched c2ln", msg.Content[0].Signature)
	}

	// A lone empty delta with no signature and no prior block is a no-op.
	empty := &loop.Message{}
	appendThinking(empty, "", "")
	if len(empty.Content) != 0 {
		t.Fatalf("empty thinking delta created %d parts, want 0", len(empty.Content))
	}
}

// TestLiveToolLoopRoundTripsSignature reproduces the original failure: a
// second turn that replays a Gemini 3.x function call. Without the signature
// round-trip the API returns "Function call is missing a thought_signature".
// Gated on GEMINI_API_KEY; CI never sets it.
func TestLiveToolLoopRoundTripsSignature(t *testing.T) {
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live tool-loop test")
	}

	p := New(Options{})
	tools := []loop.ToolSpec{{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}}

	// Turn 1: force a tool call and capture the assistant message verbatim
	// (including the signature stored on the tool-call content part).
	events, err := p.Stream(context.Background(), loop.ProviderRequest{
		Model:    "gemini-3.1-pro-preview",
		Tools:    tools,
		Messages: []loop.Message{{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "What's the weather in Toronto? Use the get_weather tool."}}}},
	})
	if err != nil {
		t.Fatalf("turn 1 Stream: %v", err)
	}
	var assistant *loop.Message
	var call *loop.ToolCall
	for ev := range events {
		switch ev.Type {
		case loop.ProviderEventToolCall:
			call = ev.ToolCall
		case loop.ProviderEventDone:
			assistant = ev.Message
		case loop.ProviderEventError:
			t.Fatalf("turn 1 provider error: %s", ev.Error)
		}
	}
	if assistant == nil || call == nil {
		t.Fatal("turn 1 did not produce an assistant message with a tool call")
	}
	var gotSig bool
	for _, part := range assistant.Content {
		if part.Type == loop.ContentTypeToolCall && part.Signature != "" {
			gotSig = true
		}
	}
	if !gotSig {
		t.Fatal("turn 1 tool call did not capture a thought signature")
	}

	// Turn 2: replay turn 1 plus a tool result. This is the request that
	// previously 400'd.
	events2, err := p.Stream(context.Background(), loop.ProviderRequest{
		Model: "gemini-3.1-pro-preview",
		Tools: tools,
		Messages: []loop.Message{
			{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "What's the weather in Toronto? Use the get_weather tool."}}},
			*assistant,
			{Role: loop.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "sunny, 22C"}}},
		},
	})
	if err != nil {
		t.Fatalf("turn 2 Stream: %v", err)
	}
	var sawText bool
	for ev := range events2 {
		switch ev.Type {
		case loop.ProviderEventTextDelta:
			if ev.Delta != "" {
				sawText = true
			}
		case loop.ProviderEventError:
			t.Fatalf("turn 2 provider error (the bug if 'thought_signature'): %s", ev.Error)
		}
	}
	if !sawText {
		t.Fatal("turn 2 produced no text after the tool result")
	}
}

func modelTurnWithCall(sig []byte) *genai.Content {
	call := &genai.Part{FunctionCall: &genai.FunctionCall{Name: "weather", Args: map[string]any{"city": "T"}}}
	if len(sig) > 0 {
		call.ThoughtSignature = sig
	}
	return genai.NewContentFromParts([]*genai.Part{call}, genai.RoleModel)
}

func firstCallSignature(content *genai.Content) []byte {
	for _, part := range content.Parts {
		if part.FunctionCall != nil {
			return part.ThoughtSignature
		}
	}
	return nil
}

func TestEnsureActiveLoopSignaturesInjectsSynthetic(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		genai.NewContentFromText("weather in Toronto?", genai.RoleUser),
		modelTurnWithCall(nil), // unsigned: should get synthetic
		genai.NewContentFromParts([]*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "weather", Response: map[string]any{"output": "sunny"}}}}, genai.RoleUser),
		modelTurnWithCall(nil), // unsigned: should get synthetic
	}
	ensureActiveLoopSignatures(contents, "gemini-3.1-pro-preview")

	if string(firstCallSignature(contents[1])) != string(syntheticThoughtSignature) {
		t.Fatalf("turn 1 signature = %q, want synthetic", firstCallSignature(contents[1]))
	}
	if string(firstCallSignature(contents[3])) != string(syntheticThoughtSignature) {
		t.Fatalf("turn 3 signature = %q, want synthetic", firstCallSignature(contents[3]))
	}
}

func TestEnsureActiveLoopSignaturesPreservesReal(t *testing.T) {
	t.Parallel()

	real := []byte("a-real-signature")
	contents := []*genai.Content{
		genai.NewContentFromText("weather?", genai.RoleUser),
		modelTurnWithCall(real),
	}
	ensureActiveLoopSignatures(contents, "gemini-3.1-pro-preview")
	if string(firstCallSignature(contents[1])) != string(real) {
		t.Fatalf("real signature was overwritten: %q", firstCallSignature(contents[1]))
	}
}

func TestEnsureActiveLoopSignaturesSkipsBeforeActiveLoop(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		genai.NewContentFromText("first question", genai.RoleUser),
		modelTurnWithCall(nil), // BEFORE the active loop: must stay unsigned
		genai.NewContentFromText("second question", genai.RoleUser),
		modelTurnWithCall(nil), // active loop: gets synthetic
	}
	ensureActiveLoopSignatures(contents, "gemini-3.1-pro-preview")

	if firstCallSignature(contents[1]) != nil {
		t.Fatalf("pre-active-loop turn was signed: %q", firstCallSignature(contents[1]))
	}
	if string(firstCallSignature(contents[3])) != string(syntheticThoughtSignature) {
		t.Fatalf("active-loop turn signature = %q, want synthetic", firstCallSignature(contents[3]))
	}
}

func TestEnsureActiveLoopSignaturesGatedByModel(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		genai.NewContentFromText("weather?", genai.RoleUser),
		modelTurnWithCall(nil),
	}
	ensureActiveLoopSignatures(contents, "gemini-2.5-flash") // non-modern: no-op
	if firstCallSignature(contents[1]) != nil {
		t.Fatalf("legacy model got a synthetic signature: %q", firstCallSignature(contents[1]))
	}
}

// TestLiveSyntheticSignatureFallback proves the fallback path: an assistant
// turn whose function call carries no signature (as in compacted or pre-fix
// history) still completes on Gemini 3.x because the synthetic sentinel is
// injected. Gated on GEMINI_API_KEY.
func TestLiveSyntheticSignatureFallback(t *testing.T) {
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live synthetic-fallback test")
	}

	p := New(Options{})
	tools := []loop.ToolSpec{{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}}
	events, err := p.Stream(context.Background(), loop.ProviderRequest{
		Model: "gemini-3.1-pro-preview",
		Tools: tools,
		Messages: []loop.Message{
			{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "What's the weather in Toronto? Use get_weather."}}},
			// Assistant tool call with NO signature — what a pre-fix transcript looks like.
			{Role: loop.MessageRoleAssistant, Content: []loop.ContentPart{{Type: loop.ContentTypeToolCall, ToolCall: &loop.ToolCall{ID: "c1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Toronto"}`)}}}},
			{Role: loop.MessageRoleTool, ToolCallID: "c1", ToolName: "get_weather", Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "sunny, 22C"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for ev := range events {
		if ev.Type == loop.ProviderEventError {
			t.Fatalf("provider error (synthetic fallback failed): %s", ev.Error)
		}
	}
}

func TestNewClientConfigAPIVersion(t *testing.T) {
	// Not parallel: mutates a process env var.
	t.Setenv(APIVersionEnvKey, "v1alpha")
	cfg := newClientConfig("k")
	if cfg.HTTPOptions.APIVersion != "v1alpha" {
		t.Fatalf("APIVersion = %q, want v1alpha", cfg.HTTPOptions.APIVersion)
	}
	if cfg.APIKey != "k" || cfg.Backend != genai.BackendGeminiAPI {
		t.Fatalf("config = %#v, want APIKey=k Backend=GeminiAPI", cfg)
	}

	t.Setenv(APIVersionEnvKey, "")
	if got := newClientConfig("k").HTTPOptions.APIVersion; got != "" {
		t.Fatalf("APIVersion = %q, want empty when env unset", got)
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
		// Use the provider default rather than a hardcoded id: gemini-2.5-flash
		// was removed from the v1beta API, which 404'd this smoke test.
		Model: DefaultModel,
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
