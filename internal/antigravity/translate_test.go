package antigravity

import (
	"encoding/json"
	"strings"
	"testing"
)

// Upstream rejects the whole request when any tool name breaks its charset, so
// an MCP name like "server/tool" would otherwise kill every request in a session.
func TestSanitizeToolName(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"already valid", "read_file", "read_file"},
		{"slash from an MCP bridge", "server/tool", "server_tool"},
		{"leading digit gets a prefix", "1tool", "_1tool"},
		{"dots and dashes survive", "a.b-c:d", "a.b-c:d"},
		{"empty becomes a placeholder", "", "tool"},
		{"spaces and symbols", "my tool!", "my_tool_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeToolName(tc.input); got != tc.want {
				t.Errorf("SanitizeToolName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSanitizeToolNameTruncatesToLimit(t *testing.T) {
	long := strings.Repeat("a", upstreamToolNameMax+50)
	got := SanitizeToolName(long)
	if len(got) != upstreamToolNameMax {
		t.Errorf("length = %d, want %d", len(got), upstreamToolNameMax)
	}
}

// The client must see the name it sent, not our sanitized version.
func TestRestoreToolNameRoundTrip(t *testing.T) {
	original := "server/my tool"
	sanitized := SanitizeToolName(original)

	if sanitized == original {
		t.Fatal("test name should have needed sanitizing")
	}
	if got := RestoreToolName(sanitized); got != original {
		t.Errorf("RestoreToolName(%q) = %q, want %q", sanitized, got, original)
	}
	// An unknown name passes through untouched.
	if got := RestoreToolName("never_seen"); got != "never_seen" {
		t.Errorf("RestoreToolName() = %q, want the input back", got)
	}
}

func userMessage(text string) Message {
	return Message{Role: "user", Content: MessageContent{IsString: true, Text: text}}
}

// The session id must be stable across turns of one conversation, or upstream
// treats every request as a new session.
func TestStableSessionIDIsDeterministic(t *testing.T) {
	messages := []Message{userMessage("hello there")}

	first := generateStableSessionID(messages, "system prompt")
	second := generateStableSessionID(messages, "system prompt")

	if first != second {
		t.Errorf("session id not stable: %q vs %q", first, second)
	}
}

// A conversation whose first message is a content array must still get a stable
// id. Hashing only plain-string first messages gave these a fresh random id on
// every turn.
func TestStableSessionIDHandlesBlockContent(t *testing.T) {
	messages := []Message{{
		Role: "user",
		Content: MessageContent{Blocks: []ContentBlock{
			{Type: "text", Text: "analyse this"},
		}},
	}}

	first := generateStableSessionID(messages, "")
	second := generateStableSessionID(messages, "")

	if first != second {
		t.Errorf("session id not stable for block content: %q vs %q", first, second)
	}
}

func TestStableSessionIDDiffersByConversation(t *testing.T) {
	a := generateStableSessionID([]Message{userMessage("first conversation")}, "sys")
	b := generateStableSessionID([]Message{userMessage("second conversation")}, "sys")

	if a == b {
		t.Error("different conversations produced the same session id")
	}
}

// Two conversations opening with the same text but different system prompts are
// different conversations.
func TestStableSessionIDIncludesSystemPrompt(t *testing.T) {
	a := generateStableSessionID([]Message{userMessage("same opener")}, "system A")
	b := generateStableSessionID([]Message{userMessage("same opener")}, "system B")

	if a == b {
		t.Error("system prompt is not part of the session id seed")
	}
}

// buildRequest is a helper for the envelope tests.
func buildRequest(t *testing.T, req ChatRequest, originalModel string) map[string]any {
	t.Helper()
	return buildUpstreamRequest(UpstreamModelName(req.Model), req, originalModel, "test-project")
}

func innerOf(t *testing.T, request map[string]any) map[string]any {
	t.Helper()
	inner, ok := request["request"].(map[string]any)
	if !ok {
		t.Fatalf("request envelope has no inner request: %v", request)
	}
	return inner
}

// The client's own system prompt must win: agent clients put their whole tool
// contract in there.
func TestClientSystemPromptWins(t *testing.T) {
	request := buildRequest(t, ChatRequest{
		Model:    "claude-opus-4-6",
		Messages: []Message{userMessage("hi")},
		System:   "YOU ARE A TEST HARNESS",
	}, "claude-opus-4-6")

	encoded, _ := json.Marshal(innerOf(t, request)["systemInstruction"])
	if !strings.Contains(string(encoded), "YOU ARE A TEST HARNESS") {
		t.Errorf("systemInstruction = %s, want the client prompt", encoded)
	}
	if strings.Contains(string(encoded), "powerful agentic AI coding assistant") {
		t.Error("the default persona overrode the client's system prompt")
	}
}

func TestDefaultIdentityUsedWithNoSystemPrompt(t *testing.T) {
	request := buildRequest(t, ChatRequest{
		Model:    "claude-opus-4-6",
		Messages: []Message{userMessage("hi")},
	}, "claude-opus-4-6")

	encoded, _ := json.Marshal(innerOf(t, request)["systemInstruction"])
	if !strings.Contains(string(encoded), "You are Antigravity") {
		t.Errorf("systemInstruction = %s, want the default identity", encoded)
	}
}

// A system turn belongs in systemInstruction, never in contents.
func TestSystemRoleIsExcludedFromContents(t *testing.T) {
	request := buildRequest(t, ChatRequest{
		Model: "claude-opus-4-6",
		Messages: []Message{
			{Role: "system", Content: MessageContent{IsString: true, Text: "ignore me"}},
			userMessage("real turn"),
		},
	}, "claude-opus-4-6")

	contents, ok := innerOf(t, request)["contents"].([]any)
	if !ok {
		t.Fatal("contents missing")
	}
	if len(contents) != 1 {
		t.Fatalf("got %d contents, want 1 (the system turn should be dropped)", len(contents))
	}
	encoded, _ := json.Marshal(contents)
	if strings.Contains(string(encoded), "ignore me") {
		t.Error("system turn leaked into contents")
	}
}

func TestAssistantRoleMapsToModel(t *testing.T) {
	request := buildRequest(t, ChatRequest{
		Model: "claude-opus-4-6",
		Messages: []Message{
			userMessage("q"),
			{Role: "assistant", Content: MessageContent{IsString: true, Text: "a"}},
		},
	}, "claude-opus-4-6")

	contents := innerOf(t, request)["contents"].([]any)
	second := contents[1].(map[string]any)
	if second["role"] != "model" {
		t.Errorf("assistant role = %v, want model", second["role"])
	}
}

// Gemini's contract is ONE tools entry holding every declaration, not one entry
// per tool.
func TestToolsCollapseIntoOneDeclarationList(t *testing.T) {
	request := buildRequest(t, ChatRequest{
		Model:    "gemini-3.1-pro-high",
		Messages: []Message{userMessage("hi")},
		Tools: []Tool{
			{Name: "read", Description: "read a file"},
			{Name: "write", Description: "write a file"},
		},
	}, "gemini-3.1-pro-high")

	tools, ok := innerOf(t, request)["tools"].([]any)
	if !ok {
		t.Fatal("tools missing")
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools entries, want exactly 1", len(tools))
	}

	entry := tools[0].(map[string]any)
	declarations, ok := entry["functionDeclarations"].([]any)
	if !ok {
		t.Fatal("functionDeclarations missing")
	}
	if len(declarations) != 2 {
		t.Errorf("got %d declarations, want 2", len(declarations))
	}
}

// The upstream model id is rollout-dependent, so the thinking level travels
// separately in thinkingConfig.
func TestThinkingLevelFromModelSuffix(t *testing.T) {
	request := buildRequest(t, ChatRequest{
		Model:    "gemini-3.7-flash-medium",
		Messages: []Message{userMessage("hi")},
	}, "gemini-3.7-flash-medium")

	genConfig := innerOf(t, request)["generationConfig"].(map[string]any)
	thinking, ok := genConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig missing: %v", genConfig)
	}
	if thinking["thinkingLevel"] != "medium" {
		t.Errorf("thinkingLevel = %v, want medium", thinking["thinkingLevel"])
	}
}

// An explicit numeric budget from the client beats the model-id suffix.
func TestExplicitThinkingBudgetWins(t *testing.T) {
	request := buildRequest(t, ChatRequest{
		Model:          "gemini-3.7-flash-high",
		Messages:       []Message{userMessage("hi")},
		ThinkingBudget: 4096,
	}, "gemini-3.7-flash-high")

	genConfig := innerOf(t, request)["generationConfig"].(map[string]any)
	thinking := genConfig["thinkingConfig"].(map[string]any)

	if thinking["thinkingBudget"] != 4096 {
		t.Errorf("thinkingBudget = %v, want 4096", thinking["thinkingBudget"])
	}
	if _, present := thinking["thinkingLevel"]; present {
		t.Error("thinkingLevel should not be set when an explicit budget is given")
	}
}

// maxOutputTokens must stay above the thinking budget, or upstream 400s.
func TestThinkingBudgetClampedBelowMaxTokens(t *testing.T) {
	request := buildRequest(t, ChatRequest{
		Model:          "gemini-3.7-flash-high",
		Messages:       []Message{userMessage("hi")},
		MaxTokens:      100,
		ThinkingBudget: 5000,
	}, "gemini-3.7-flash-high")

	genConfig := innerOf(t, request)["generationConfig"].(map[string]any)
	thinking := genConfig["thinkingConfig"].(map[string]any)

	budget, ok := thinking["thinkingBudget"].(int)
	if !ok {
		t.Fatalf("thinkingBudget = %v, want an int", thinking["thinkingBudget"])
	}
	if budget >= 100 {
		t.Errorf("thinkingBudget = %d, want it clamped below maxOutputTokens (100)", budget)
	}
}

func TestMaxOutputTokensClamping(t *testing.T) {
	cases := []struct {
		name  string
		input int
		want  int
	}{
		{"zero falls back to the default", 0, defaultMaxOutputTokens},
		{"negative falls back to the default", -5, defaultMaxOutputTokens},
		{"in range is preserved", 1000, 1000},
		{"over the ceiling is capped", 999999, defaultMaxOutputTokens},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampMaxOutputTokens(tc.input); got != tc.want {
				t.Errorf("clampMaxOutputTokens(%d) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// A tool_result must reach upstream as a functionResponse naming the call it
// answers, or Gemini rejects the turn.
func TestToolResultBecomesFunctionResponse(t *testing.T) {
	toolResultContent, _ := json.Marshal("the file body")

	request := buildRequest(t, ChatRequest{
		Model: "gemini-3.1-pro-high",
		Messages: []Message{
			userMessage("read it"),
			{Role: "assistant", Content: MessageContent{Blocks: []ContentBlock{{
				Type: "tool_use", ID: "call-1", Name: "read", Input: map[string]any{"p": "/a"},
			}}}},
			{Role: "user", Content: MessageContent{Blocks: []ContentBlock{{
				Type: "tool_result", ToolUseID: "call-1", Content: toolResultContent,
			}}}},
		},
	}, "gemini-3.1-pro-high")

	contents := innerOf(t, request)["contents"].([]any)
	third := contents[2].(map[string]any)
	parts := third["parts"].([]any)
	part := parts[0].(map[string]any)

	response, ok := part["functionResponse"].(map[string]any)
	if !ok {
		t.Fatalf("tool_result did not become a functionResponse: %v", part)
	}
	if response["name"] != "read" {
		t.Errorf("functionResponse name = %v, want read (resolved from the tool id)", response["name"])
	}
	if response["id"] != "call-1" {
		t.Errorf("functionResponse id = %v, want call-1", response["id"])
	}
	payload := response["response"].(map[string]any)
	if payload["result"] != "the file body" {
		t.Errorf("result = %v, want the file body", payload["result"])
	}
}

func TestMergeToolResultContent(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		isError bool
		want    string
	}{
		{"plain string", `"hello"`, false, "hello"},
		{"block array is flattened", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, false, "a\nb"},
		{"empty success", ``, false, "Command executed successfully."},
		{"empty error", ``, true, "Tool execution failed with no output."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			if got := mergeToolResultContent(raw, tc.isError); got != tc.want {
				t.Errorf("mergeToolResultContent() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestImageBlockBecomesInlineData(t *testing.T) {
	request := buildRequest(t, ChatRequest{
		Model: "gemini-3.1-pro-high",
		Messages: []Message{{
			Role: "user",
			Content: MessageContent{Blocks: []ContentBlock{{
				Type: "image",
				Source: &ImageSource{
					Type: "base64", MediaType: "image/png", Data: "AAAA",
				},
			}}},
		}},
	}, "gemini-3.1-pro-high")

	contents := innerOf(t, request)["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	part := parts[0].(map[string]any)

	inline, ok := part["inlineData"].(map[string]any)
	if !ok {
		t.Fatalf("image did not become inlineData: %v", part)
	}
	if inline["mimeType"] != "image/png" || inline["data"] != "AAAA" {
		t.Errorf("inlineData = %v", inline)
	}
}

func TestMessageContentUnmarshalsBothShapes(t *testing.T) {
	var asString MessageContent
	if err := json.Unmarshal([]byte(`"plain text"`), &asString); err != nil {
		t.Fatalf("string form failed: %v", err)
	}
	if !asString.IsString || asString.Text != "plain text" {
		t.Errorf("string form = %+v", asString)
	}

	var asBlocks MessageContent
	if err := json.Unmarshal([]byte(`[{"type":"text","text":"a"}]`), &asBlocks); err != nil {
		t.Fatalf("block form failed: %v", err)
	}
	if asBlocks.IsString || len(asBlocks.Blocks) != 1 {
		t.Errorf("block form = %+v", asBlocks)
	}

	var asNull MessageContent
	if err := json.Unmarshal([]byte(`null`), &asNull); err != nil {
		t.Fatalf("null form failed: %v", err)
	}
	if !asNull.IsString || asNull.Text != "" {
		t.Errorf("null form = %+v, want an empty string", asNull)
	}
}

func TestUpstreamModelNameMapping(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-6":       "claude-opus-4-6-thinking",
		"gemini-3.7-flash-high": "gemini-3.7-flash-tiered",
		"gemini-3.1-pro-high":   "gemini-pro-agent",
		"unknown-model":         "unknown-model",
	}
	for input, want := range cases {
		if got := UpstreamModelName(input); got != want {
			t.Errorf("UpstreamModelName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestFunctionCallingConfigModes(t *testing.T) {
	cases := []struct {
		name   string
		choice *ToolChoice
		want   string
	}{
		{"nil defaults to VALIDATED", nil, "VALIDATED"},
		{"auto", &ToolChoice{Type: "auto"}, "VALIDATED"},
		{"none", &ToolChoice{Type: "none"}, "NONE"},
		{"any", &ToolChoice{Type: "any"}, "ANY"},
		{"named tool", &ToolChoice{Type: "tool", Name: "read"}, "ANY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := buildFunctionCallingConfig(tc.choice)
			if config["mode"] != tc.want {
				t.Errorf("mode = %v, want %v", config["mode"], tc.want)
			}
		})
	}

	// A named tool must also constrain the allowed function list.
	config := buildFunctionCallingConfig(&ToolChoice{Type: "tool", Name: "server/read"})
	allowed, ok := config["allowedFunctionNames"].([]any)
	if !ok || len(allowed) != 1 {
		t.Fatalf("allowedFunctionNames = %v", config["allowedFunctionNames"])
	}
	// The name must be sanitized here too, or upstream rejects the request.
	if allowed[0] != "server_read" {
		t.Errorf("allowedFunctionNames[0] = %v, want server_read", allowed[0])
	}
}

// The upstream model id for the flash models is "gemini-3.7-flash-tiered". The
// "-high"/"-medium"/"-low" suffixes are client-facing only: the thinking level
// travels separately in thinkingConfig.
//
// Regression: sending "gemini-3.7-flash-high" through as the upstream id was
// verified live to fail. Google answers an unknown model with
// 429 RESOURCE_EXHAUSTED — indistinguishable from real quota exhaustion — so the
// break presents as a phantom quota problem on the busiest model.
func TestFlashModelsMapToTieredUpstreamID(t *testing.T) {
	for _, clientID := range []string{
		"gemini-3.7-flash",
		"gemini-3.7-flash-low",
		"gemini-3.7-flash-medium",
		"gemini-3.7-flash-high",
	} {
		if got := UpstreamModelName(clientID); got != "gemini-3.7-flash-tiered" {
			t.Errorf("UpstreamModelName(%q) = %q, want gemini-3.7-flash-tiered", clientID, got)
		}
	}
}

// The thinking level must still reach upstream even though every flash variant
// collapses onto one model id — that is what makes the collapse safe.
func TestFlashThinkingLevelSurvivesTheCollapse(t *testing.T) {
	for _, tc := range []struct{ model, wantLevel string }{
		{"gemini-3.7-flash-low", "low"},
		{"gemini-3.7-flash-medium", "medium"},
		{"gemini-3.7-flash-high", "high"},
	} {
		request := buildRequest(t, ChatRequest{
			Model:    tc.model,
			Messages: []Message{userMessage("hi")},
		}, tc.model)

		genConfig := innerOf(t, request)["generationConfig"].(map[string]any)
		thinking, ok := genConfig["thinkingConfig"].(map[string]any)
		if !ok {
			t.Errorf("%s: thinkingConfig missing", tc.model)
			continue
		}
		if thinking["thinkingLevel"] != tc.wantLevel {
			t.Errorf("%s: thinkingLevel = %v, want %q",
				tc.model, thinking["thinkingLevel"], tc.wantLevel)
		}
	}
}
