package antigravity

import (
	"encoding/json"
	"strings"
	"testing"
)

const upstream400 = `{"error":{"code":400,` +
	`"message":"Function call is missing a thought_signature in functionCall parts. ` +
	`This is required for tools to work correctly, and missing thought_signature ` +
	`may lead to degraded model performance.","status":"INVALID_ARGUMENT"}}`

func TestDetectsMissingThoughtSignature400(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"the real 400", 400, upstream400, true},
		{"a different 400", 400, "some other bad request", false},
		{"right body, wrong status", 429, upstream400, false},
		{"empty body", 400, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMissingThoughtSignatureError(tc.status, tc.body); got != tc.want {
				t.Errorf("IsMissingThoughtSignatureError() = %v, want %v", got, tc.want)
			}
		})
	}
}

// buildSignatureRequest mirrors a replayed conversation where the first tool
// call is signed and the second is not.
func buildSignatureRequest(t *testing.T) map[string]any {
	t.Helper()
	raw := `{
		"request": {
			"contents": [
				{"role": "user", "parts": [{"text": "fix the checker"}]},
				{"role": "model", "parts": [
					{"thoughtSignature": "sig-1",
					 "functionCall": {"name": "read", "id": "c1", "args": {"filePath": "/a.js"}}}
				]},
				{"role": "user", "parts": [
					{"functionResponse": {"name": "read", "id": "c1", "response": {"result": "file body"}}}
				]},
				{"role": "model", "parts": [
					{"functionCall": {"name": "edit", "id": "c2", "args": {"filePath": "/a.js"}}}
				]},
				{"role": "user", "parts": [
					{"functionResponse": {"name": "edit", "id": "c2", "response": {"result": "edited"}}}
				]}
			]
		}
	}`
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return out
}

// partAt reaches into contents[i].parts[j].
func partAt(t *testing.T, request map[string]any, message, index int) map[string]any {
	t.Helper()
	inner := request["request"].(map[string]any)
	contents := inner["contents"].([]any)
	node := contents[message].(map[string]any)
	parts := node["parts"].([]any)
	part, ok := parts[index].(map[string]any)
	if !ok {
		t.Fatalf("contents[%d].parts[%d] is not an object", message, index)
	}
	return part
}

func TestDegradesOnlyUnsignedCallAndItsResult(t *testing.T) {
	request := buildSignatureRequest(t)

	if !degradeUnsignedToolCalls(request) {
		t.Fatal("degradeUnsignedToolCalls() = false, want true")
	}

	// The signed call keeps its valid history.
	signed := partAt(t, request, 1, 0)
	if _, ok := signed["functionCall"]; !ok {
		t.Error("signed functionCall was degraded but should have been left alone")
	}
	if _, ok := signed["thoughtSignature"]; !ok {
		t.Error("thoughtSignature was dropped from the signed call")
	}
	if _, ok := partAt(t, request, 2, 0)["functionResponse"]; !ok {
		t.Error("the signed call's response should have been left alone")
	}

	// The unsigned call and its reply become plain text.
	unsigned := partAt(t, request, 3, 0)
	if _, ok := unsigned["functionCall"]; ok {
		t.Error("unsigned functionCall should have been degraded")
	}
	text, _ := unsigned["text"].(string)
	if !strings.Contains(text, "[Previous tool call] edit(") {
		t.Errorf("degraded call text = %q", text)
	}

	orphan := partAt(t, request, 4, 0)
	if _, ok := orphan["functionResponse"]; ok {
		t.Error("the orphaned functionResponse should have been degraded too")
	}
	resultText, _ := orphan["text"].(string)
	if !strings.Contains(resultText, "[Previous tool result] edit -> edited") {
		t.Errorf("degraded result text = %q", resultText)
	}
}

func TestDegradeIsNoOpWhenEveryCallIsSigned(t *testing.T) {
	raw := `{
		"request": {
			"contents": [
				{"role": "user", "parts": [{"text": "hi"}]},
				{"role": "model", "parts": [
					{"thoughtSignature": "sig",
					 "functionCall": {"name": "read", "id": "c1", "args": {}}}
				]},
				{"role": "user", "parts": [
					{"functionResponse": {"name": "read", "id": "c1", "response": {"result": "ok"}}}
				]}
			]
		}
	}`
	var request map[string]any
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}

	if degradeUnsignedToolCalls(request) {
		t.Fatal("degradeUnsignedToolCalls() = true, want false when everything is signed")
	}
	if _, ok := partAt(t, request, 1, 0)["functionCall"]; !ok {
		t.Error("functionCall was modified on a no-op pass")
	}
	if _, ok := partAt(t, request, 2, 0)["functionResponse"]; !ok {
		t.Error("functionResponse was modified on a no-op pass")
	}
}

func TestDegradeToleratesMalformedRequests(t *testing.T) {
	cases := []struct {
		name    string
		request map[string]any
	}{
		{"empty", map[string]any{}},
		{"no contents", map[string]any{"request": map[string]any{}}},
		{"contents is a string", map[string]any{
			"request": map[string]any{"contents": "nope"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if degradeUnsignedToolCalls(tc.request) {
				t.Error("degradeUnsignedToolCalls() = true, want false on malformed input")
			}
		})
	}
}

func TestRepairBadRequestDropsThinkingConfig(t *testing.T) {
	request := map[string]any{
		"request": map[string]any{
			"generationConfig": map[string]any{
				"maxOutputTokens": 100,
				"thinkingConfig":  map[string]any{"thinkingBudget": 500},
			},
		},
	}

	repaired := RepairBadRequest(request, 400, "thinking budget exceeds max output tokens")
	if repaired == "" {
		t.Fatal("RepairBadRequest() returned no repair")
	}

	inner := request["request"].(map[string]any)
	genConfig := inner["generationConfig"].(map[string]any)
	if _, present := genConfig["thinkingConfig"]; present {
		t.Error("thinkingConfig should have been removed")
	}
}

func TestRepairBadRequestIgnoresNon400(t *testing.T) {
	if got := RepairBadRequest(map[string]any{}, 500, upstream400); got != "" {
		t.Errorf("RepairBadRequest() = %q, want no repair for a 500", got)
	}
}

func TestExtractTraceID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"top level", `{"traceId":"abc123"}`, "abc123"},
		{"nested under error", `{"error":{"traceId":"xyz789"}}`, "xyz789"},
		{"regex fallback on invalid JSON", `garbage "traceId": "fallback" more`, "fallback"},
		{"absent", `{"error":{"code":500}}`, ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractTraceID(tc.body); got != tc.want {
				t.Errorf("ExtractTraceID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// quota_exhausted means rotate accounts; anything else means wait. Getting this
// wrong either burns every account or stalls on a spent one.
func TestIsQuotaExhaustedErrorText(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"explicit reason", `{"error":{"details":[{"reason":"QUOTA_EXHAUSTED"}]}}`, true},
		{"quota and reset in message", `{"error":{"message":"quota will reset at midnight"}}`, true},
		{"rate limit is not quota", `{"error":{"details":[{"reason":"RATE_LIMIT_EXCEEDED"}]}}`, false},
		{"capacity is not quota", `{"error":{"message":"no capacity available"}}`, false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isQuotaExhaustedErrorText(tc.body); got != tc.want {
				t.Errorf("isQuotaExhaustedErrorText() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtractMissingProjectID(t *testing.T) {
	body := `{"error":{"details":[{"resourceName":"projects/stale-project-42"}]}}`
	if got := extractMissingProjectID(body); got != "stale-project-42" {
		t.Errorf("extractMissingProjectID() = %q, want stale-project-42", got)
	}
	if got := extractMissingProjectID(`{"error":{}}`); got != "" {
		t.Errorf("extractMissingProjectID() = %q, want empty", got)
	}
}

// SSE payloads can span multiple data: lines, which must be rejoined with
// newlines before parsing.
func TestExtractSSEEventDataJoinsMultilinePayloads(t *testing.T) {
	event := "event: message\ndata: {\"a\":\ndata: 1}\n"
	if got := extractSSEEventData(event); got != "{\"a\":\n1}" {
		t.Errorf("extractSSEEventData() = %q", got)
	}
	if got := extractSSEEventData("event: ping\n"); got != "" {
		t.Errorf("extractSSEEventData() = %q, want empty when there is no data line", got)
	}
}

func TestCollectSSEChunksSkipsDoneAndGarbage(t *testing.T) {
	body := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}}\n\n" +
		"data: [DONE]\n\n" +
		"data: not json\n\n"

	chunks := collectSSEChunks(body)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	parts := chunks[0].partsOf()
	if len(parts) != 1 || parts[0].Text != "hi" {
		t.Errorf("parts = %+v, want a single text part 'hi'", parts)
	}
}

func TestParseAPIResponseCoalescesTextAndCountsUsage(t *testing.T) {
	body := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Hello "}]}}]}}

data: {"response":{"candidates":[{"content":{"parts":[{"text":"world"}]}}],` +
		`"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":5,"thoughtsTokenCount":3}}}

`
	result, err := parseAPIResponse(collectSSEChunks(body))
	if err != nil {
		t.Fatalf("parseAPIResponse() error = %v", err)
	}

	if len(result.ContentBlocks) != 1 {
		t.Fatalf("got %d blocks, want 1 coalesced text block", len(result.ContentBlocks))
	}
	if result.ContentBlocks[0].Text != "Hello world" {
		t.Errorf("text = %q, want %q", result.ContentBlocks[0].Text, "Hello world")
	}
	if result.StopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", result.StopReason)
	}
	if result.Usage.InputTokens != 11 {
		t.Errorf("inputTokens = %d, want 11", result.Usage.InputTokens)
	}
	// Output counts answer tokens plus thinking tokens.
	if result.Usage.OutputTokens != 8 {
		t.Errorf("outputTokens = %d, want 8 (5 + 3)", result.Usage.OutputTokens)
	}
}

func TestParseAPIResponseSeparatesReasoningFromText(t *testing.T) {
	body := `data: {"response":{"candidates":[{"content":{"parts":[` +
		`{"thought":true,"text":"thinking hard"},{"text":"answer"}]}}]}}

`
	result, err := parseAPIResponse(collectSSEChunks(body))
	if err != nil {
		t.Fatalf("parseAPIResponse() error = %v", err)
	}
	if result.Reasoning != "thinking hard" {
		t.Errorf("reasoning = %q", result.Reasoning)
	}
	if len(result.ContentBlocks) != 1 || result.ContentBlocks[0].Text != "answer" {
		t.Errorf("blocks = %+v, want just the answer text", result.ContentBlocks)
	}
}

func TestParseAPIResponseMarksToolUse(t *testing.T) {
	body := `data: {"response":{"candidates":[{"content":{"parts":[` +
		`{"functionCall":{"name":"read","id":"c1","args":{"path":"/a"}}}]}}]}}

`
	result, err := parseAPIResponse(collectSSEChunks(body))
	if err != nil {
		t.Fatalf("parseAPIResponse() error = %v", err)
	}
	if result.StopReason != "tool_use" {
		t.Errorf("stopReason = %q, want tool_use", result.StopReason)
	}
	if len(result.ContentBlocks) != 1 || result.ContentBlocks[0].Type != "tool_use" {
		t.Fatalf("blocks = %+v, want one tool_use block", result.ContentBlocks)
	}
	if result.ContentBlocks[0].ID != "c1" {
		t.Errorf("tool id = %q, want c1", result.ContentBlocks[0].ID)
	}
}

func TestParseAPIResponseRejectsEmpty(t *testing.T) {
	if _, err := parseAPIResponse(nil); err == nil {
		t.Error("parseAPIResponse(nil) should error")
	}
}
