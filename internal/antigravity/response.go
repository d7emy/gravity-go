package antigravity

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// upstreamChunk is one decoded SSE payload from streamGenerateContent.
type upstreamChunk struct {
	Response *candidateEnvelope `json:"response"`
	// Some chunks arrive without the response wrapper.
	Candidates    []candidate    `json:"candidates"`
	UsageMetadata *usageMetadata `json:"usageMetadata"`
}

type candidateEnvelope struct {
	Candidates    []candidate    `json:"candidates"`
	UsageMetadata *usageMetadata `json:"usageMetadata"`
}

type candidate struct {
	Content struct {
		Parts []part `json:"parts"`
	} `json:"content"`
}

type part struct {
	Text             string        `json:"text"`
	Thought          bool          `json:"thought"`
	ThoughtSignature string        `json:"thoughtSignature"`
	FunctionCall     *functionCall `json:"functionCall"`
}

type functionCall struct {
	Name string `json:"name"`
	Args any    `json:"args"`
	ID   string `json:"id"`
}

type usageMetadata struct {
	PromptTokenCount     int64 `json:"promptTokenCount"`
	CandidatesTokenCount int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount   int64 `json:"thoughtsTokenCount"`
}

// partsOf returns the parts of a chunk regardless of which envelope it used.
func (c upstreamChunk) partsOf() []part {
	if c.Response != nil && len(c.Response.Candidates) > 0 {
		return c.Response.Candidates[0].Content.Parts
	}
	if len(c.Candidates) > 0 {
		return c.Candidates[0].Content.Parts
	}
	return nil
}

// usageOf returns the usage block of a chunk regardless of envelope.
func (c upstreamChunk) usageOf() *usageMetadata {
	if c.Response != nil && c.Response.UsageMetadata != nil {
		return c.Response.UsageMetadata
	}
	return c.UsageMetadata
}

// parseAPIResponse folds the collected chunks into one completed response.
func parseAPIResponse(chunks []upstreamChunk) (ChatResponse, error) {
	if len(chunks) == 0 {
		return ChatResponse{}, errors.New("empty response")
	}

	var blocks []ResponseBlock
	var reasoning strings.Builder
	hasToolUse := false

	for _, chunk := range chunks {
		for _, p := range chunk.partsOf() {
			if p.Thought {
				// A thought part carries reasoning text, not answer text.
				reasoning.WriteString(p.Text)
				continue
			}
			if p.Text != "" {
				// Coalesce consecutive text parts into one block.
				if n := len(blocks); n > 0 && blocks[n-1].Type == "text" {
					blocks[n-1].Text += p.Text
				} else {
					blocks = append(blocks, ResponseBlock{Type: "text", Text: p.Text})
				}
			}
			if p.FunctionCall != nil {
				hasToolUse = true
				toolCallID := p.FunctionCall.ID
				if toolCallID == "" {
					toolCallID = generateToolUseID()
				}
				RememberToolCallSignature(toolCallID, p.ThoughtSignature)
				blocks = append(blocks, ResponseBlock{
					Type:  "tool_use",
					ID:    toolCallID,
					Name:  RestoreToolName(p.FunctionCall.Name),
					Input: parseFunctionCallArgs(p.FunctionCall.Args),
				})
			}
		}
	}

	if len(blocks) == 0 {
		blocks = append(blocks, ResponseBlock{Type: "text", Text: ""})
	}

	stopReason := "end_turn"
	if hasToolUse {
		stopReason = "tool_use"
	}

	// Usage arrives cumulatively; the last chunk carrying it wins.
	var usage Usage
	for i := len(chunks) - 1; i >= 0; i-- {
		if u := chunks[i].usageOf(); u != nil {
			usage = Usage{
				InputTokens:  u.PromptTokenCount,
				OutputTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount,
			}
			break
		}
	}

	return ChatResponse{
		ContentBlocks: blocks,
		StopReason:    stopReason,
		Usage:         usage,
		Reasoning:     reasoning.String(),
	}, nil
}

var traceIDPattern = regexp.MustCompile(`"traceId"\s*:\s*"([^"]+)"`)

// ExtractTraceID pulls Google's trace id out of a response body. Surfacing it on
// failures is the difference between "Google returned 500" and something Google
// can actually look up.
func ExtractTraceID(body string) string {
	text := strings.TrimSpace(body)
	if text == "" {
		return ""
	}
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		var root map[string]any
		if err := json.Unmarshal([]byte(text), &root); err == nil {
			if id, ok := root["traceId"].(string); ok && id != "" {
				return id
			}
			if errNode, ok := root["error"].(map[string]any); ok {
				if id, ok := errNode["traceId"].(string); ok && id != "" {
					return id
				}
			}
		}
	}
	if m := traceIDPattern.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

var thoughtSignaturePattern = regexp.MustCompile(`(?i)thought_signature|thoughtsignature`)

// IsMissingThoughtSignatureError recognises the 400 Gemini returns when a
// replayed functionCall arrives without its signature.
func IsMissingThoughtSignatureError(status int, errorText string) bool {
	return status == 400 && thoughtSignaturePattern.MatchString(errorText)
}

// degradeUnsignedToolCalls rewrites replayed tool calls we have no signature for
// into plain text.
//
// Gemini 3 rejects any replayed functionCall with no thoughtSignature. That
// happens whenever a client replays history from before this proxy stored
// signatures, or from an entry that has since been evicted. Rather than failing
// the turn, degrade those calls to text so the model keeps the context and the
// agent loop survives.
func degradeUnsignedToolCalls(request map[string]any) bool {
	inner, ok := request["request"].(map[string]any)
	if !ok {
		return false
	}
	contents, ok := inner["contents"].([]any)
	if !ok {
		return false
	}

	degradedIDs := map[string]bool{}
	degradedNames := map[string]bool{}
	changed := false

	for _, message := range contents {
		node, ok := message.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := node["parts"].([]any)
		if !ok {
			continue
		}
		for i, rawPart := range parts {
			partNode, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			call, ok := partNode["functionCall"].(map[string]any)
			if !ok {
				continue
			}
			if _, signed := partNode["thoughtSignature"]; signed {
				continue
			}

			name, _ := call["name"].(string)
			if id, ok := call["id"].(string); ok && id != "" {
				degradedIDs[id] = true
			}
			if name != "" {
				degradedNames[name] = true
			}
			changed = true

			args := "{}"
			if call["args"] != nil {
				if encoded, err := json.Marshal(call["args"]); err == nil {
					args = string(encoded)
				}
			}
			if name == "" {
				name = "tool"
			}
			parts[i] = map[string]any{
				"text": "[Previous tool call] " + name + "(" + args + ")",
			}
		}
	}

	if !changed {
		return false
	}

	// A functionResponse without its functionCall is invalid, so degrade the
	// matching replies too.
	for _, message := range contents {
		node, ok := message.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := node["parts"].([]any)
		if !ok {
			continue
		}
		for i, rawPart := range parts {
			partNode, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			response, ok := partNode["functionResponse"].(map[string]any)
			if !ok {
				continue
			}
			id, _ := response["id"].(string)
			name, _ := response["name"].(string)
			if id != "" {
				if !degradedIDs[id] {
					continue
				}
			} else if !degradedNames[name] {
				continue
			}

			result := ""
			if payload, ok := response["response"].(map[string]any); ok {
				if text, ok := payload["result"].(string); ok {
					result = text
				} else if encoded, err := json.Marshal(payload); err == nil {
					result = string(encoded)
				}
			}
			if name == "" {
				name = "tool"
			}
			parts[i] = map[string]any{
				"text": "[Previous tool result] " + name + " -> " + result,
			}
		}
	}

	return true
}

var (
	invalidFunctionNamePattern = regexp.MustCompile(
		`(?i)Invalid function name|function_declarations\[\d+\]\.name`)
	thinkingPattern         = regexp.MustCompile(`(?i)thinking|thought`)
	missingProjectIDPattern = regexp.MustCompile(`^projects/([^/]+)$`)
)

// RepairBadRequest fixes a 400 in place when the cause is something we can
// correct, returning a short description of what changed.
//
// Upstream 400s are otherwise terminal, so without this one repairable mistake
// would end the whole agent turn.
func RepairBadRequest(request map[string]any, status int, errorText string) string {
	if status != 400 {
		return ""
	}

	// 1. Replayed tool calls with no stored signature.
	if IsMissingThoughtSignatureError(status, errorText) {
		if degradeUnsignedToolCalls(request) {
			return "missing thought_signature -> unsigned tool calls degraded to text"
		}
	}

	// 2. A tool name upstream will not accept. Names are sanitized on the way
	//    out, so this is a backstop for anything that slipped through.
	if invalidFunctionNamePattern.MatchString(errorText) {
		renamed := 0
		if inner, ok := request["request"].(map[string]any); ok {
			if tools, ok := inner["tools"].([]any); ok {
				for _, entry := range tools {
					node, ok := entry.(map[string]any)
					if !ok {
						continue
					}
					declarations, ok := node["functionDeclarations"].([]any)
					if !ok {
						continue
					}
					for _, rawDecl := range declarations {
						decl, ok := rawDecl.(map[string]any)
						if !ok {
							continue
						}
						name, _ := decl["name"].(string)
						safe := SanitizeToolName(name)
						if safe != name {
							decl["name"] = safe
							renamed++
						}
					}
				}
			}
		}
		if renamed > 0 {
			return "invalid tool name -> renamed tool(s)"
		}
	}

	// 3. Thinking budget must stay below maxOutputTokens; drop it rather than fail.
	if thinkingPattern.MatchString(errorText) {
		if inner, ok := request["request"].(map[string]any); ok {
			if genConfig, ok := inner["generationConfig"].(map[string]any); ok {
				if _, has := genConfig["thinkingConfig"]; has {
					delete(genConfig, "thinkingConfig")
					return "thinking config rejected -> removed"
				}
			}
		}
	}

	return ""
}

// isQuotaExhaustedErrorText reports whether a 429 body means a spent quota (so
// rotate accounts) rather than transient throttling (so wait).
func isQuotaExhaustedErrorText(errorText string) bool {
	body := strings.TrimSpace(errorText)
	if body == "" {
		return false
	}

	if strings.HasPrefix(body, "{") || strings.HasPrefix(body, "[") {
		var root map[string]any
		if err := json.Unmarshal([]byte(body), &root); err == nil {
			if errNode, ok := root["error"].(map[string]any); ok {
				if details, ok := errNode["details"].([]any); ok {
					for _, detail := range details {
						node, ok := detail.(map[string]any)
						if !ok {
							continue
						}
						if node["reason"] == "QUOTA_EXHAUSTED" {
							return true
						}
					}
				}
				if message, ok := errNode["message"].(string); ok {
					lower := strings.ToLower(message)
					if strings.Contains(lower, "quota") && strings.Contains(lower, "reset") {
						return true
					}
				}
			}
		}
	}

	lower := strings.ToLower(body)
	if strings.Contains(lower, "quota_exhausted") {
		return true
	}
	return strings.Contains(lower, "quota") && strings.Contains(lower, "reset")
}

// extractMissingProjectID reads the project id from a NOT_FOUND body, so the
// caller can re-resolve a stale project rather than failing.
func extractMissingProjectID(errorText string) string {
	body := strings.TrimSpace(errorText)
	if !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, "[") {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return ""
	}
	errNode, ok := root["error"].(map[string]any)
	if !ok {
		return ""
	}
	details, ok := errNode["details"].([]any)
	if !ok {
		return ""
	}
	for _, detail := range details {
		node, ok := detail.(map[string]any)
		if !ok {
			continue
		}
		resourceName, _ := node["resourceName"].(string)
		if m := missingProjectIDPattern.FindStringSubmatch(resourceName); m != nil {
			return m[1]
		}
	}
	return ""
}

// extractSSEEventData joins the data: lines of one SSE event block.
func extractSSEEventData(event string) string {
	var dataLines []string
	for _, line := range strings.Split(event, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		value := line[5:]
		value = strings.TrimPrefix(value, " ")
		dataLines = append(dataLines, value)
	}
	if len(dataLines) == 0 {
		return ""
	}
	return strings.Join(dataLines, "\n")
}

var sseEventSeparator = regexp.MustCompile(`\r?\n\r?\n`)

// collectSSEChunks decodes a whole buffered SSE body.
func collectSSEChunks(rawSSE string) []upstreamChunk {
	var chunks []upstreamChunk
	for _, event := range sseEventSeparator.Split(rawSSE, -1) {
		data := strings.TrimSpace(extractSSEEventData(event))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk upstreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}
