package antigravity

import (
	"encoding/json"
	"fmt"

	"gravity-go/internal/logx"
)

// streamTranslator converts upstream chunks into the Anthropic SSE event
// sequence, tracking block indices across text, thinking and tool_use blocks.
//
// The index bookkeeping is the fiddly part: Anthropic requires every opened
// content block to be closed before another opens at the next index, and
// thinking can reappear after ordinary text within one response.
type streamTranslator struct {
	model string
	emit  func(string) error

	blockIndex    int
	textOpen      bool
	thinkingOpen  bool
	thinkingIndex int
	hasToolUse    bool
	inputTokens   int64
	outputTokens  int64
}

func newStreamTranslator(model string, emit func(string) error) *streamTranslator {
	return &streamTranslator{model: model, emit: emit, thinkingIndex: -1}
}

// event renders one SSE frame.
func (t *streamTranslator) event(name string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return t.emit("event: " + name + "\ndata: " + string(encoded) + "\n\n")
}

func (t *streamTranslator) start() error {
	return t.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            GenerateMessageID(),
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         t.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (t *streamTranslator) blockStop(index int) error {
	return t.event("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})
}

// closeTextBlock ends an open text block and advances the index.
func (t *streamTranslator) closeTextBlock() error {
	if !t.textOpen {
		return nil
	}
	if err := t.blockStop(t.blockIndex); err != nil {
		return err
	}
	t.textOpen = false
	t.blockIndex++
	return nil
}

// closeThinkingBlock ends an open thinking block and advances the index.
func (t *streamTranslator) closeThinkingBlock() error {
	if !t.thinkingOpen {
		return nil
	}
	if err := t.blockStop(t.thinkingIndex); err != nil {
		return err
	}
	t.thinkingOpen = false
	t.blockIndex++
	return nil
}

func (t *streamTranslator) handleChunk(chunk upstreamChunk) error {
	for _, p := range chunk.partsOf() {
		if p.Thought && p.Text != "" {
			if err := t.handleThinking(p.Text); err != nil {
				return err
			}
			continue
		}

		// The first non-thinking part closes any open thinking block.
		if err := t.closeThinkingBlock(); err != nil {
			return err
		}

		if p.Text != "" {
			if err := t.handleText(p.Text); err != nil {
				return err
			}
		}
		if p.FunctionCall != nil {
			if err := t.handleToolCall(p); err != nil {
				return err
			}
		}
	}

	if u := chunk.usageOf(); u != nil {
		if u.PromptTokenCount > 0 {
			t.inputTokens = u.PromptTokenCount
		}
		t.outputTokens = u.CandidatesTokenCount + u.ThoughtsTokenCount
	}
	return nil
}

func (t *streamTranslator) handleThinking(text string) error {
	if !t.thinkingOpen {
		// Thinking can resume after ordinary text. The open text block must be
		// closed first, or the thinking block would reuse its index and break
		// the Anthropic stream protocol.
		if err := t.closeTextBlock(); err != nil {
			return err
		}
		t.thinkingIndex = t.blockIndex
		if err := t.event("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         t.thinkingIndex,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		}); err != nil {
			return err
		}
		t.thinkingOpen = true
	}
	return t.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": t.thinkingIndex,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

func (t *streamTranslator) handleText(text string) error {
	if !t.textOpen {
		if err := t.event("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         t.blockIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		}); err != nil {
			return err
		}
		t.textOpen = true
	}
	return t.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": t.blockIndex,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (t *streamTranslator) handleToolCall(p part) error {
	if err := t.closeTextBlock(); err != nil {
		return err
	}

	t.hasToolUse = true
	toolCallID := p.FunctionCall.ID
	if toolCallID == "" {
		toolCallID = generateToolUseID()
	}
	RememberToolCallSignature(toolCallID, p.ThoughtSignature)

	if err := t.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": t.blockIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    toolCallID,
			"name":  RestoreToolName(p.FunctionCall.Name),
			"input": map[string]any{},
		},
	}); err != nil {
		return err
	}

	if p.FunctionCall.Args != nil {
		partialJSON := "{}"
		if asString, ok := p.FunctionCall.Args.(string); ok {
			partialJSON = asString
		} else if encoded, err := json.Marshal(p.FunctionCall.Args); err == nil {
			partialJSON = string(encoded)
		}
		if err := t.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": t.blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": partialJSON},
		}); err != nil {
			return err
		}
	}

	if err := t.blockStop(t.blockIndex); err != nil {
		return err
	}
	t.blockIndex++
	return nil
}

// finish closes any open blocks and writes the terminal events.
func (t *streamTranslator) finish(toolChoice *ToolChoice) error {
	if t.textOpen {
		if err := t.blockStop(t.blockIndex); err != nil {
			return err
		}
		t.textOpen = false
	}
	if t.thinkingOpen {
		if err := t.blockStop(t.thinkingIndex); err != nil {
			return err
		}
		t.thinkingOpen = false
	}

	if !t.hasToolUse && toolChoice != nil && toolChoice.Type == "tool" {
		name := toolChoice.Name
		if name == "" {
			name = "unknown"
		}
		logx.Warn("Tool choice %q requested but no tool_use returned", name)
	}

	stopReason := "end_turn"
	if t.hasToolUse {
		stopReason = "tool_use"
	}

	if err := t.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":  t.inputTokens,
			"output_tokens": t.outputTokens,
		},
	}); err != nil {
		return err
	}

	return t.emit(fmt.Sprintf("event: message_stop\ndata: %s\n\n",
		`{"type":"message_stop"}`))
}
