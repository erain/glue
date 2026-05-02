package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"glue/loop"

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
			if part.ToolCall == nil {
				return nil, fmt.Errorf("gemini: tool call content missing payload")
			}
			args, err := rawObject(part.ToolCall.Arguments)
			if err != nil {
				return nil, fmt.Errorf("gemini: tool call %q arguments: %w", part.ToolCall.Name, err)
			}
			parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
				ID:   part.ToolCall.ID,
				Name: part.ToolCall.Name,
				Args: args,
			}})
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

func buildGenerateConfig(req loop.ProviderRequest) (*genai.GenerateContentConfig, error) {
	config := &genai.GenerateContentConfig{}
	if req.SystemPrompt != "" {
		config.SystemInstruction = genai.NewContentFromText(req.SystemPrompt, genai.RoleUser)
	}
	tools, err := ConvertTools(req.Tools)
	if err != nil {
		return nil, err
	}
	config.Tools = tools

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
	// Structured-output options (response_mime_type, response_json_schema)
	// are intentionally unhandled here; they belong to the structured-JSON
	// result issue.
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
