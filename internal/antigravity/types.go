package antigravity

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ImageSource is a base64 inline image on a content block.
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	// URL is carried so a "url" source can be named in the warning and the
	// placeholder rather than vanishing without trace. Upstream cannot fetch it.
	URL string `json:"url,omitempty"`
}

// ContentBlock is one Anthropic content block. Content is left raw because
// tool_result carries either a string or a nested block array.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     any             `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *ImageSource    `json:"source,omitempty"`
}

// MessageContent is Anthropic's `content`, which is either a bare string or an
// array of blocks. Both shapes round-trip through this type.
type MessageContent struct {
	IsString bool
	Text     string
	Blocks   []ContentBlock
}

// UnmarshalJSON accepts either encoding.
func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		mc.IsString = true
		mc.Text = ""
		return nil
	}
	if trimmed[0] == '"' {
		mc.IsString = true
		return json.Unmarshal(trimmed, &mc.Text)
	}
	mc.IsString = false
	return json.Unmarshal(trimmed, &mc.Blocks)
}

// MarshalJSON re-emits whichever shape was parsed.
func (mc MessageContent) MarshalJSON() ([]byte, error) {
	if mc.IsString {
		return json.Marshal(mc.Text)
	}
	if mc.Blocks == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(mc.Blocks)
}

// AsBlocks normalizes to a block list.
func (mc MessageContent) AsBlocks() []ContentBlock {
	if mc.IsString {
		if mc.Text == "" {
			return nil
		}
		return []ContentBlock{{Type: "text", Text: mc.Text}}
	}
	return mc.Blocks
}

// PlainText flattens the content to text, the way the session-id seed needs it.
func (mc MessageContent) PlainText() string {
	if mc.IsString {
		return mc.Text
	}
	var parts []string
	for _, block := range mc.Blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		case "tool_use":
			parts = append(parts, "[Tool: "+block.Name+"]")
		case "tool_result":
			if text, ok := rawAsString(block.Content); ok {
				parts = append(parts, text)
			}
		}
	}
	if len(parts) == 0 {
		return "[No text]"
	}
	return strings.Join(parts, "\n")
}

// Message is one Anthropic conversation turn.
type Message struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

// Tool is an Anthropic tool declaration.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

// ToolChoice constrains which tool the model may call.
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// ChatRequest is the provider-neutral request the handlers build.
type ChatRequest struct {
	Model      string
	Messages   []Message
	Tools      []Tool
	ToolChoice *ToolChoice
	MaxTokens  int
	// System is the client's system prompt; it becomes systemInstruction.
	System      string
	Temperature *float64
	TopP        *float64
	TopK        *float64
	// ThinkingBudget is an explicit reasoning budget in tokens. When set it
	// overrides the level implied by the model id suffix.
	ThinkingBudget int
}

// ResponseBlock is one block of a completed (non-streaming) response.
type ResponseBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`
}

// Usage is the token accounting for one response.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// ChatResponse is a completed non-streaming response.
type ChatResponse struct {
	ContentBlocks []ResponseBlock
	StopReason    string
	Usage         Usage
	Reasoning     string
}

// CallOptions selects which account handles a request.
type CallOptions struct {
	AccountID     string
	AllowRotation bool
	RouteTag      string
}

// rawAsString reads a JSON value that is expected to be a plain string.
func rawAsString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}
