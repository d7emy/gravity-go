package server

import (
	"encoding/json"
	"strings"
	"testing"

	"gravity-go/internal/antigravity"
)

func TestTranslateContentHandlesStringAndParts(t *testing.T) {
	asString := translateContent(json.RawMessage(`"plain"`))
	if !asString.IsString || asString.Text != "plain" {
		t.Errorf("string content = %+v", asString)
	}

	asParts := translateContent(json.RawMessage(
		`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`))
	if asParts.IsString || len(asParts.Blocks) != 2 {
		t.Errorf("part content = %+v, want 2 blocks", asParts)
	}
}

// A data: URL must become inline base64; upstream only accepts inlined images.
func TestTranslateContentInlinesDataURLImage(t *testing.T) {
	content := translateContent(json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]`))

	if len(content.Blocks) != 1 {
		t.Fatalf("blocks = %+v, want 1", content.Blocks)
	}
	block := content.Blocks[0]
	if block.Type != "image" || block.Source == nil {
		t.Fatalf("block = %+v, want an image with a source", block)
	}
	if block.Source.MediaType != "image/png" || block.Source.Data != "AAAA" {
		t.Errorf("source = %+v", block.Source)
	}
}

// A remote image URL cannot be fetched upstream, so it degrades to a description
// rather than being dropped silently. The marker names the URL that was skipped
// and says what to send instead, so the omission is actionable.
func TestTranslateContentDescribesRemoteImage(t *testing.T) {
	content := translateContent(json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`))

	if len(content.Blocks) != 1 || content.Blocks[0].Type != "text" {
		t.Fatalf("blocks = %+v, want a single text block", content.Blocks)
	}
	text := content.Blocks[0].Text
	if !strings.Contains(text, "image omitted") {
		t.Errorf("text = %q, want it to say the image was omitted", text)
	}
	if !strings.Contains(text, "https://example.com/a.png") {
		t.Errorf("text = %q, want it to name the skipped URL", text)
	}
	if !strings.Contains(text, "base64") {
		t.Errorf("text = %q, want it to say what to send instead", text)
	}
}

// system/developer turns must travel as a real system instruction, not be
// demoted to user turns, which would strip their authority.
func TestExtractOpenAISystemPromptJoinsSystemAndDeveloper(t *testing.T) {
	messages := []openAIMessage{
		{Role: "system", Content: json.RawMessage(`"be terse"`)},
		{Role: "developer", Content: json.RawMessage(`"use tools"`)},
		{Role: "user", Content: json.RawMessage(`"hi"`)},
	}

	got := extractOpenAISystemPrompt(messages)
	if got != "be terse\n\nuse tools" {
		t.Errorf("system prompt = %q", got)
	}
}

func TestTranslateOpenAIMessagesDropsSystemTurns(t *testing.T) {
	messages := []openAIMessage{
		{Role: "system", Content: json.RawMessage(`"ignore"`)},
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", Content: json.RawMessage(`"hello"`)},
	}

	got := translateOpenAIMessages(messages)
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2 (the system turn is extracted separately)", len(got))
	}
	if got[0].Role != "user" || got[1].Role != "assistant" {
		t.Errorf("roles = %q/%q", got[0].Role, got[1].Role)
	}
}

func TestTranslateOpenAIToolCallsBecomeToolUse(t *testing.T) {
	var call openAIToolCall
	call.ID = "call-1"
	call.Type = "function"
	call.Function.Name = "read"
	call.Function.Arguments = `{"path":"/a"}`

	got := translateOpenAIMessages([]openAIMessage{{Role: "assistant", ToolCalls: []openAIToolCall{call}}})
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}

	blocks := got[0].Content.Blocks
	if len(blocks) != 1 || blocks[0].Type != "tool_use" {
		t.Fatalf("blocks = %+v, want a tool_use block", blocks)
	}
	if blocks[0].ID != "call-1" || blocks[0].Name != "read" {
		t.Errorf("block = %+v", blocks[0])
	}

	input, ok := blocks[0].Input.(map[string]any)
	if !ok || input["path"] != "/a" {
		t.Errorf("input = %v, want the decoded arguments", blocks[0].Input)
	}
}

// An OpenAI tool turn maps to an Anthropic tool_result carried on a user turn.
func TestTranslateOpenAIToolResultBecomesUserToolResult(t *testing.T) {
	got := translateOpenAIMessages([]openAIMessage{{
		Role:       "tool",
		ToolCallID: "call-1",
		Content:    json.RawMessage(`"the output"`),
	}})

	if len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user turn", got)
	}
	blocks := got[0].Content.Blocks
	if len(blocks) != 1 || blocks[0].Type != "tool_result" {
		t.Fatalf("blocks = %+v, want a tool_result", blocks)
	}
	if blocks[0].ToolUseID != "call-1" {
		t.Errorf("tool_use_id = %q, want call-1", blocks[0].ToolUseID)
	}

	var text string
	if err := json.Unmarshal(blocks[0].Content, &text); err != nil {
		t.Fatalf("tool_result content is not a JSON string: %v", err)
	}
	if text != "the output" {
		t.Errorf("content = %q", text)
	}
}

func TestTranslateToolChoiceForms(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantType string
		wantName string
	}{
		{"auto", `"auto"`, "auto", ""},
		{"none", `"none"`, "none", ""},
		{"required maps to any", `"required"`, "any", ""},
		{"named function", `{"type":"function","function":{"name":"read"}}`, "tool", "read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateToolChoice(json.RawMessage(tc.raw))
			if got == nil {
				t.Fatal("translateToolChoice() = nil")
			}
			if got.Type != tc.wantType {
				t.Errorf("type = %q, want %q", got.Type, tc.wantType)
			}
			if got.Name != tc.wantName {
				t.Errorf("name = %q, want %q", got.Name, tc.wantName)
			}
		})
	}

	if got := translateToolChoice(nil); got != nil {
		t.Errorf("translateToolChoice(nil) = %+v, want nil", got)
	}
}

func TestTranslateOpenAIToolsDefaultsEmptySchema(t *testing.T) {
	var tool openAITool
	tool.Function.Name = "ping"

	got := translateOpenAITools([]openAITool{tool})
	if len(got) != 1 {
		t.Fatalf("got %d tools, want 1", len(got))
	}

	schema, ok := got[0].InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("schema = %v, want an object", got[0].InputSchema)
	}
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"tool_use":   "tool_calls",
		"max_tokens": "length",
		"end_turn":   "stop",
		"":           "stop",
	}
	for input, want := range cases {
		if got := mapStopReason(input); got != want {
			t.Errorf("mapStopReason(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestReasoningBudgetFromEffort(t *testing.T) {
	cases := []struct {
		name string
		req  openAIRequest
		want int
	}{
		{"none", openAIRequest{}, 0},
		{"low", openAIRequest{ReasoningEffort: "low"}, 2048},
		{"medium", openAIRequest{ReasoningEffort: "medium"}, 8192},
		{"high", openAIRequest{ReasoningEffort: "high"}, 24576},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasoningBudget(&tc.req); got != tc.want {
				t.Errorf("reasoningBudget() = %d, want %d", got, tc.want)
			}
		})
	}

	// The nested reasoning.effort form must work too.
	nested := openAIRequest{}
	nested.Reasoning = &struct {
		Effort string `json:"effort"`
	}{Effort: "high"}
	if got := reasoningBudget(&nested); got != 24576 {
		t.Errorf("nested reasoning.effort budget = %d, want 24576", got)
	}
}

// The OpenAI stream state machine must fold Anthropic events back into OpenAI
// chunks, including accumulating tool-call arguments across deltas.
func TestOpenAIStreamAccumulatesToolCalls(t *testing.T) {
	var chunks []map[string]any
	state := &openAIStreamState{
		chatID: "chatcmpl-test",
		model:  "gpt-4",
		write: func(payload string) error {
			data, ok := stripDataPrefix(payload)
			if !ok {
				return nil
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				return nil
			}
			chunks = append(chunks, parsed)
			return nil
		},
	}

	frames := []string{
		`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}

`,
		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c1","name":"read"}}

`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"p\":"}}

`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"/a\"}"}}

`,
		`event: content_block_stop
data: {"type":"content_block_stop","index":0}

`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

`,
	}
	for _, frame := range frames {
		if err := state.handleEvent(frame); err != nil {
			t.Fatalf("handleEvent() error = %v", err)
		}
	}

	if state.inputTokens != 5 || state.outputTokens != 9 {
		t.Errorf("usage = %d in / %d out, want 5/9", state.inputTokens, state.outputTokens)
	}

	// Find the chunk carrying the assembled tool call.
	var arguments string
	var finishReason string
	for _, chunk := range chunks {
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			finishReason = reason
		}
		delta, _ := choice["delta"].(map[string]any)
		calls, ok := delta["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			continue
		}
		fn := calls[0].(map[string]any)["function"].(map[string]any)
		arguments, _ = fn["arguments"].(string)
	}

	if arguments != `{"p":"/a"}` {
		t.Errorf("tool call arguments = %q, want the two deltas concatenated", arguments)
	}
	if finishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", finishReason)
	}
}

func TestOpenAIStreamEmitsRoleOnce(t *testing.T) {
	var roleChunks int
	state := &openAIStreamState{
		chatID: "chatcmpl-test",
		model:  "gpt-4",
		write: func(payload string) error {
			data, ok := stripDataPrefix(payload)
			if !ok {
				return nil
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				return nil
			}
			choices, _ := parsed["choices"].([]any)
			if len(choices) == 0 {
				return nil
			}
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			if _, has := delta["role"]; has {
				roleChunks++
			}
			return nil
		},
	}

	frame := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	for i := 0; i < 3; i++ {
		if err := state.handleEvent(frame); err != nil {
			t.Fatalf("handleEvent() error = %v", err)
		}
	}

	if roleChunks != 1 {
		t.Errorf("role chunks = %d, want exactly 1", roleChunks)
	}
}

// stripDataPrefix pulls the payload out of an SSE data frame.
func stripDataPrefix(payload string) (string, bool) {
	const prefix = "data: "
	if len(payload) <= len(prefix) || payload[:len(prefix)] != prefix {
		return "", false
	}
	trimmed := payload[len(prefix):]
	for len(trimmed) > 0 && (trimmed[len(trimmed)-1] == '\n' || trimmed[len(trimmed)-1] == '\r') {
		trimmed = trimmed[:len(trimmed)-1]
	}
	if trimmed == "[DONE]" {
		return "", false
	}
	return trimmed, true
}

func TestMapOpenAIModelAliases(t *testing.T) {
	cases := map[string]string{
		"gpt-4":                 "claude-sonnet-4-5",
		"gpt-4o":                "claude-sonnet-4-5",
		"o1":                    "claude-sonnet-4-5-thinking",
		"gemini-3.7-flash-high": "gemini-3.7-flash-high",
	}
	for input, want := range cases {
		if got := antigravity.MapOpenAIModel(input); got != want {
			t.Errorf("MapOpenAIModel(%q) = %q, want %q", input, got, want)
		}
	}
}
