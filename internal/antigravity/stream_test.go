package antigravity

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// capturedEvent is one Anthropic SSE frame decoded back into structure.
type capturedEvent struct {
	Name string
	Data map[string]any
}

var (
	eventNamePattern = regexp.MustCompile(`(?m)^event: (.+)$`)
	eventDataPattern = regexp.MustCompile(`(?m)^data: (.+)$`)
)

// runTranslator feeds chunks through the stream translator and captures the
// resulting Anthropic events.
func runTranslator(t *testing.T, chunks []upstreamChunk) []capturedEvent {
	t.Helper()

	var events []capturedEvent
	emit := func(frame string) error {
		nameMatch := eventNamePattern.FindStringSubmatch(frame)
		dataMatch := eventDataPattern.FindStringSubmatch(frame)
		if nameMatch == nil || dataMatch == nil {
			return nil
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(dataMatch[1]), &data); err != nil {
			t.Fatalf("emitted unparseable event data %q: %v", dataMatch[1], err)
		}
		events = append(events, capturedEvent{Name: nameMatch[1], Data: data})
		return nil
	}

	translator := newStreamTranslator("gemini-3.1-pro-high", emit)
	if err := translator.start(); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	for _, chunk := range chunks {
		if err := translator.handleChunk(chunk); err != nil {
			t.Fatalf("handleChunk() error = %v", err)
		}
	}
	if err := translator.finish(nil); err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	return events
}

// chunkOf builds an upstream chunk from raw JSON.
func chunkOf(t *testing.T, raw string) upstreamChunk {
	t.Helper()
	var chunk upstreamChunk
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("bad chunk fixture: %v", err)
	}
	return chunk
}

func indexOf(t *testing.T, data map[string]any) int {
	t.Helper()
	value, ok := data["index"].(float64)
	if !ok {
		t.Fatalf("event has no numeric index: %v", data)
	}
	return int(value)
}

// Thinking can resume after ordinary text. If the thinking block reused the
// still-open text index, the Anthropic stream protocol would be violated.
func TestThinkingAfterTextOpensNewBlockIndex(t *testing.T) {
	events := runTranslator(t, []upstreamChunk{
		chunkOf(t, `{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"planning"}]}}]}}`),
		chunkOf(t, `{"response":{"candidates":[{"content":{"parts":[{"text":"hello "}]}}]}}`),
		chunkOf(t, `{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"more planning"}]}}]}}`),
		chunkOf(t, `{"response":{"candidates":[{"content":{"parts":[{"text":"world"}]}}],`+
			`"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":5,"thoughtsTokenCount":3}}}`),
	})

	// Every start must precede its stop, and must never reuse an index that is
	// still open.
	open := map[int]bool{}
	var startedTypes []string
	var startedIndices []int

	for _, event := range events {
		switch event.Name {
		case "content_block_start":
			index := indexOf(t, event.Data)
			if open[index] {
				t.Fatalf("index %d reopened while still open", index)
			}
			open[index] = true

			block, ok := event.Data["content_block"].(map[string]any)
			if !ok {
				t.Fatalf("content_block_start has no content_block: %v", event.Data)
			}
			blockType, _ := block["type"].(string)
			startedTypes = append(startedTypes, blockType)
			startedIndices = append(startedIndices, index)

		case "content_block_stop":
			index := indexOf(t, event.Data)
			if !open[index] {
				t.Fatalf("index %d stopped without being open", index)
			}
			delete(open, index)
		}
	}

	if len(open) != 0 {
		t.Errorf("blocks left open: %v", open)
	}

	wantTypes := []string{"thinking", "text", "thinking", "text"}
	if strings.Join(startedTypes, ",") != strings.Join(wantTypes, ",") {
		t.Errorf("block types = %v, want %v", startedTypes, wantTypes)
	}
	for i, index := range startedIndices {
		if index != i {
			t.Errorf("block %d opened at index %d, want %d", i, index, i)
		}
	}
}

func TestMessageDeltaReportsInputAndOutputTokens(t *testing.T) {
	events := runTranslator(t, []upstreamChunk{
		chunkOf(t, `{"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],`+
			`"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":4,"thoughtsTokenCount":2}}}`),
	})

	var delta map[string]any
	for _, event := range events {
		if event.Name == "message_delta" {
			delta = event.Data
		}
	}
	if delta == nil {
		t.Fatal("no message_delta event emitted")
	}

	usage, ok := delta["usage"].(map[string]any)
	if !ok {
		t.Fatalf("message_delta has no usage: %v", delta)
	}
	if got := usage["input_tokens"]; got != float64(7) {
		t.Errorf("input_tokens = %v, want 7", got)
	}
	// Output counts answer tokens plus thinking tokens.
	if got := usage["output_tokens"]; got != float64(6) {
		t.Errorf("output_tokens = %v, want 6 (4 + 2)", got)
	}
}

func TestToolCallClosesOpenTextBlockFirst(t *testing.T) {
	events := runTranslator(t, []upstreamChunk{
		chunkOf(t, `{"response":{"candidates":[{"content":{"parts":[{"text":"let me check"}]}}]}}`),
		chunkOf(t, `{"response":{"candidates":[{"content":{"parts":[`+
			`{"functionCall":{"name":"read","id":"c1","args":{"path":"/a"}}}]}}]}}`),
	})

	open := map[int]bool{}
	for _, event := range events {
		switch event.Name {
		case "content_block_start":
			index := indexOf(t, event.Data)
			if open[index] {
				t.Fatalf("index %d reopened while still open", index)
			}
			open[index] = true
		case "content_block_stop":
			delete(open, indexOf(t, event.Data))
		}
	}
	if len(open) != 0 {
		t.Errorf("blocks left open: %v", open)
	}

	var sawToolUse bool
	var stopReason string
	for _, event := range events {
		if event.Name == "content_block_start" {
			if block, ok := event.Data["content_block"].(map[string]any); ok {
				if block["type"] == "tool_use" {
					sawToolUse = true
					if block["name"] != "read" {
						t.Errorf("tool name = %v, want read", block["name"])
					}
				}
			}
		}
		if event.Name == "message_delta" {
			if delta, ok := event.Data["delta"].(map[string]any); ok {
				stopReason, _ = delta["stop_reason"].(string)
			}
		}
	}
	if !sawToolUse {
		t.Error("no tool_use block was opened")
	}
	if stopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stopReason)
	}
}

func TestStreamAlwaysStartsAndStops(t *testing.T) {
	events := runTranslator(t, []upstreamChunk{
		chunkOf(t, `{"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}}`),
	})

	if len(events) < 2 {
		t.Fatalf("got %d events, want at least a start and a stop", len(events))
	}
	if events[0].Name != "message_start" {
		t.Errorf("first event = %q, want message_start", events[0].Name)
	}
	if last := events[len(events)-1].Name; last != "message_stop" {
		t.Errorf("last event = %q, want message_stop", last)
	}
}

// An empty response still has to produce a well-formed stream.
func TestEmptyStreamStillTerminates(t *testing.T) {
	events := runTranslator(t, nil)

	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.HasPrefix(joined, "message_start") {
		t.Errorf("events = %v, want to start with message_start", names)
	}
	if !strings.HasSuffix(joined, "message_stop") {
		t.Errorf("events = %v, want to end with message_stop", names)
	}
}
