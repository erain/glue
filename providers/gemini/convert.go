package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/erain/glue/loop"

	"google.golang.org/genai"
)

// ConvertMessages converts Glue messages to Gemini contents.
//
// Consecutive tool-role messages are grouped into a single Gemini content
// with role "user" and one FunctionResponse part per message, matching how
// the genai SDK expects multi-tool turns to be expressed.
func ConvertMessages(messages []loop.Message) ([]*genai.Content, error) {
	contents := make([]*genai.Content, 0, len(messages))
	for i := 0; i < len(messages); {
		if messages[i].Role == loop.MessageRoleTool {
			parts := make([]*genai.Part, 0)
			for i < len(messages) && messages[i].Role == loop.MessageRoleTool {
				part, err := convertToolMessage(messages[i])
				if err != nil {
					return nil, err
				}
				parts = append(parts, part)
				i++
			}
			if len(parts) > 0 {
				contents = append(contents, genai.NewContentFromParts(parts, genai.RoleUser))
			}
			continue
		}

		content, err := convertMessage(messages[i])
		if err != nil {
			return nil, err
		}
		if content != nil {
			contents = append(contents, content)
		}
		i++
	}
	return contents, nil
}

func convertMessage(message loop.Message) (*genai.Content, error) {
	var role genai.Role
	switch message.Role {
	case loop.MessageRoleUser:
		role = genai.RoleUser
	case loop.MessageRoleAssistant:
		role = genai.RoleModel
	case loop.MessageRoleTool:
		return nil, fmt.Errorf("gemini: tool messages should be converted via ConvertMessages, not convertMessage")
	default:
		return nil, fmt.Errorf("gemini: unsupported message role %q", message.Role)
	}

	parts := make([]*genai.Part, 0, len(message.Content))
	for _, part := range message.Content {
		switch part.Type {
		case loop.ContentTypeText:
			if part.Text != "" {
				parts = append(parts, genai.NewPartFromText(part.Text))
			}
		case loop.ContentTypeThinking:
			if part.Thinking != "" {
				thought := &genai.Part{Text: part.Thinking, Thought: true}
				if sig := decodeSignature(part.Signature); len(sig) > 0 {
					thought.ThoughtSignature = sig
				}
				parts = append(parts, thought)
			}
		case loop.ContentTypeImage:
			if part.Image == nil {
				return nil, fmt.Errorf("gemini: image content missing payload")
			}
			data, err := base64.StdEncoding.DecodeString(part.Image.Data)
			if err != nil {
				return nil, fmt.Errorf("gemini: decode image data: %w", err)
			}
			parts = append(parts, genai.NewPartFromBytes(data, part.Image.MIMEType))
		case loop.ContentTypeToolCall:
			if part.ToolCall == nil {
				return nil, fmt.Errorf("gemini: tool call content missing payload")
			}
			args, err := rawObject(part.ToolCall.Arguments)
			if err != nil {
				return nil, fmt.Errorf("gemini: tool call %q arguments: %w", part.ToolCall.Name, err)
			}
			call := &genai.Part{FunctionCall: &genai.FunctionCall{
				ID:   part.ToolCall.ID,
				Name: part.ToolCall.Name,
				Args: args,
			}}
			// Echo the thought signature the model produced for this call.
			// Gemini 3.x rejects a replayed function call that has lost it.
			if sig := decodeSignature(part.Signature); len(sig) > 0 {
				call.ThoughtSignature = sig
			}
			parts = append(parts, call)
		default:
			return nil, fmt.Errorf("gemini: unsupported content type %q", part.Type)
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return genai.NewContentFromParts(parts, role), nil
}

func convertToolMessage(message loop.Message) (*genai.Part, error) {
	if message.ToolName == "" {
		return nil, fmt.Errorf("gemini: tool message missing tool name")
	}
	key := "output"
	if message.IsError {
		key = "error"
	}
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		ID:       message.ToolCallID,
		Name:     message.ToolName,
		Response: map[string]any{key: toolMessageText(message)},
	}}, nil
}

func toolMessageText(message loop.Message) string {
	var text string
	for _, part := range message.Content {
		if part.Type == loop.ContentTypeText {
			if text != "" {
				text += "\n"
			}
			text += part.Text
		}
	}
	return text
}

// ConvertTools converts Glue tool specs to Gemini function declarations.
// All declarations are bundled into a single Gemini Tool, which is how
// genai expects multiple tools to be exposed to the model.
func ConvertTools(tools []loop.ToolSpec) ([]*genai.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	declarations := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, tool := range tools {
		declaration := &genai.FunctionDeclaration{
			Name:        tool.Name,
			Description: tool.Description,
		}
		if len(tool.Parameters) > 0 {
			schema, err := rawSchema(tool.Parameters)
			if err != nil {
				return nil, fmt.Errorf("gemini: tool %q parameters: %w", tool.Name, err)
			}
			declaration.ParametersJsonSchema = schema
		}
		declarations = append(declarations, declaration)
	}
	return []*genai.Tool{{FunctionDeclarations: declarations}}, nil
}

// syntheticThoughtSignature is the sentinel Gemini's backend recognizes as
// "skip thought-signature validation for this turn" — the same literal
// Google's gemini-cli uses. We fall back to it only when a real signature is
// absent (compacted history, transcripts written before signature
// round-tripping landed, or a turn that genuinely arrived unsigned), so a
// replayed Gemini 3.x function call does not 400 with "Function call is
// missing a thought_signature". The SDK base64-encodes these bytes on the
// wire and the backend recognizes the decoded value; verified accepted live.
var syntheticThoughtSignature = []byte("skip_thought_signature_validator")

// ensureActiveLoopSignatures guarantees that, for Gemini 3.x, every model turn
// in the active loop has a thought signature on its first function call — the
// invariant the API enforces. The active loop begins at the most recent
// genuine user turn (one carrying text or images, not just function
// responses); everything after it is the current tool-calling loop, and turns
// before it are exempt. Real signatures (restored by convertMessage) are left
// untouched; only a missing one gets the synthetic sentinel. Older models
// neither emit nor require signatures, so they are skipped entirely.
func ensureActiveLoopSignatures(contents []*genai.Content, model string) {
	if !isModernGeminiModel(model) {
		return
	}
	start := -1
	for i := len(contents) - 1; i >= 0; i-- {
		if isGenuineUserTurn(contents[i]) {
			start = i
			break
		}
	}
	if start == -1 {
		return
	}
	for i := start; i < len(contents); i++ {
		content := contents[i]
		if content.Role != string(genai.RoleModel) {
			continue
		}
		for _, part := range content.Parts {
			if part == nil || part.FunctionCall == nil {
				continue
			}
			if len(part.ThoughtSignature) == 0 {
				part.ThoughtSignature = syntheticThoughtSignature
			}
			break // only the first function call in a turn needs a signature
		}
	}
}

// isGenuineUserTurn reports whether a content is a real user message (text or
// image input) rather than a grouped batch of function responses, which also
// carry the user role. It marks the active-loop boundary.
func isGenuineUserTurn(content *genai.Content) bool {
	if content == nil || content.Role != string(genai.RoleUser) {
		return false
	}
	for _, part := range content.Parts {
		if part != nil && part.FunctionResponse == nil {
			return true
		}
	}
	return false
}

// encodeSignature base64-encodes an opaque thought signature for storage on a
// loop.ContentPart. Empty signatures stay empty so transcripts don't carry
// noise.
func encodeSignature(sig []byte) string {
	if len(sig) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(sig)
}

// decodeSignature reverses encodeSignature. A corrupt or non-base64 value
// (e.g. a hand-edited transcript) decodes to nil rather than failing the whole
// request — a missing signature is recoverable, a hard error is not.
func decodeSignature(s string) []byte {
	if s == "" {
		return nil
	}
	sig, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return sig
}

// isModernGeminiModel reports whether the model belongs to the Gemini 3.x
// family. Those ids emit opaque thought signatures that the API requires
// echoed back on replay and benefit from includeThoughts; 2.5 and earlier do
// neither. The id may carry suffixes (e.g. "gemini-3.1-pro-preview"), so match
// on the family prefix.
func isModernGeminiModel(model string) bool {
	return strings.Contains(model, "gemini-3")
}

func rawObject(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("must be a JSON object")
	}
	return object, nil
}

func rawSchema(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	return schema, nil
}

func buildGenerateConfig(req loop.ProviderRequest, model string) (*genai.GenerateContentConfig, error) {
	config := &genai.GenerateContentConfig{}
	if req.SystemPrompt != "" {
		config.SystemInstruction = genai.NewContentFromText(req.SystemPrompt, genai.RoleUser)
	}
	tools, err := ConvertTools(req.Tools)
	if err != nil {
		return nil, err
	}
	config.Tools = tools

	// Surface the model's reasoning on Gemini 3.x: includeThoughts streams
	// thought parts (and the signatures attached to them) so callers can show
	// thinking and so signed thoughts round-trip. The function-call signature
	// the API requires on replay is returned regardless of this flag, so it is
	// complementary, not load-bearing. Older ids neither emit thoughts nor
	// need the flag, so leave them untouched.
	if isModernGeminiModel(model) {
		config.ThinkingConfig = &genai.ThinkingConfig{IncludeThoughts: true}
	}

	for key, value := range req.Options {
		switch key {
		case "temperature":
			temperature, ok := numberAsFloat32(value)
			if !ok {
				return nil, fmt.Errorf("gemini: temperature must be numeric")
			}
			config.Temperature = genai.Ptr(temperature)
		case "max_tokens", "max_output_tokens":
			maxTokens, ok := numberAsInt32(value)
			if !ok {
				return nil, fmt.Errorf("gemini: %s must be numeric", key)
			}
			config.MaxOutputTokens = maxTokens
		case "response_mime_type":
			mimeType, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("gemini: response_mime_type must be a string")
			}
			config.ResponseMIMEType = mimeType
		case "response_json_schema":
			config.ResponseJsonSchema = value
		}
	}
	return config, nil
}

func numberAsFloat32(value any) (float32, bool) {
	switch v := value.(type) {
	case float32:
		return v, true
	case float64:
		return float32(v), true
	case int:
		return float32(v), true
	case int32:
		return float32(v), true
	case int64:
		return float32(v), true
	default:
		return 0, false
	}
}

func numberAsInt32(value any) (int32, bool) {
	switch v := value.(type) {
	case int:
		return int32(v), true
	case int32:
		return v, true
	case int64:
		return int32(v), true
	case float32:
		return int32(v), true
	case float64:
		return int32(v), true
	default:
		return 0, false
	}
}
