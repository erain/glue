package gemini

import (
	"encoding/base64"
	"fmt"

	"glue/loop"

	"google.golang.org/genai"
)

// ConvertMessages converts Glue text messages to Gemini contents.
//
// Tool-call assistant content and tool-role messages are not supported in
// the text-only provider; they will be wired up alongside Gemini function
// calling in a follow-up issue. Encountering them here yields a clear error
// rather than silently dropping the message.
func ConvertMessages(messages []loop.Message) ([]*genai.Content, error) {
	contents := make([]*genai.Content, 0, len(messages))
	for _, message := range messages {
		content, err := convertMessage(message)
		if err != nil {
			return nil, err
		}
		if content != nil {
			contents = append(contents, content)
		}
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
		return nil, fmt.Errorf("gemini: tool messages require function-calling support (not yet implemented)")
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
				parts = append(parts, genai.NewPartFromText(part.Thinking))
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
			return nil, fmt.Errorf("gemini: tool-call content requires function-calling support (not yet implemented)")
		default:
			return nil, fmt.Errorf("gemini: unsupported content type %q", part.Type)
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return genai.NewContentFromParts(parts, role), nil
}

func buildGenerateConfig(req loop.ProviderRequest) (*genai.GenerateContentConfig, error) {
	config := &genai.GenerateContentConfig{}
	if req.SystemPrompt != "" {
		config.SystemInstruction = genai.NewContentFromText(req.SystemPrompt, genai.RoleUser)
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
		}
	}
	// Tools and structured-output options (response_mime_type,
	// response_json_schema) are intentionally unhandled here; they belong
	// to the function-calling and structured-JSON issues respectively.
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
