package antigravity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"gravity-go/internal/logx"
	"math"
	"regexp"
	"strings"
	"sync"
)

const defaultMaxOutputTokens = 64000

// antigravityIdentity is the fallback persona, used only when the client sends
// no system prompt of its own.
const antigravityIdentity = `You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.
You are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question.
**Absolute paths only**
**Proactiveness**`

// Upstream rejects the entire request — not just the offending tool — when a
// tool name breaks its charset: "Invalid function name. Must start with a letter
// or an underscore. Must be alphameric (a-z, A-Z, 0-9), underscores (_), dots
// (.), colons (:), or dashes (-)". MCP bridges routinely produce "server/tool",
// so one such tool would kill every request in the session. Rename on the way
// out, restore on the way back.
const upstreamToolNameMax = 64

var (
	toolNameMu       sync.Mutex
	sanitizedToolMap = map[string]string{} // sanitized -> original
	invalidToolChars = regexp.MustCompile(`[^A-Za-z0-9_.:-]`)
	leadingOK        = regexp.MustCompile(`^[A-Za-z_]`)
)

// SanitizeToolName rewrites a tool name into upstream's accepted charset.
func SanitizeToolName(name string) string {
	if name == "" {
		return "tool"
	}
	sanitized := invalidToolChars.ReplaceAllString(name, "_")
	if !leadingOK.MatchString(sanitized) {
		sanitized = "_" + sanitized
	}
	if len(sanitized) > upstreamToolNameMax {
		sanitized = sanitized[:upstreamToolNameMax]
	}
	if sanitized != name {
		toolNameMu.Lock()
		if len(sanitizedToolMap) > 1000 {
			sanitizedToolMap = map[string]string{}
		}
		sanitizedToolMap[sanitized] = name
		toolNameMu.Unlock()
	}
	return sanitized
}

// RestoreToolName maps a name coming back from upstream to the client's original.
func RestoreToolName(name string) string {
	if name == "" {
		return name
	}
	toolNameMu.Lock()
	defer toolNameMu.Unlock()
	if original, ok := sanitizedToolMap[name]; ok {
		return original
	}
	return name
}

func generateToolUseID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "toolu_fallback"
	}
	return "toolu_" + hex.EncodeToString(buf)
}

// GenerateMessageID builds an Anthropic-style message id.
func GenerateMessageID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "msg_fallback"
	}
	return "msg_" + hex.EncodeToString(buf)
}

func generateRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "agent-fallback"
	}
	return "agent-" + hex.EncodeToString(buf)
}

// generateStableSessionID derives a per-conversation id.
//
// It seeds on the system prompt plus the first user turn, both fixed for the
// life of a conversation, stringifying whatever shape the content is. Hashing
// only a plain-string first message (as an earlier version did) gave conversations
// that opened with a content array a fresh random id every turn — the opposite of
// stable.
func generateStableSessionID(messages []Message, system string) string {
	first := ""
	for _, msg := range messages {
		if msg.Role == "user" {
			if msg.Content.IsString {
				first = msg.Content.Text
			} else if encoded, err := json.Marshal(msg.Content.Blocks); err == nil {
				first = string(encoded)
			}
			break
		}
	}

	seed := system + "\n" + first
	if strings.TrimSpace(seed) == "" {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err == nil {
			return "-" + fmt.Sprintf("%d", int64(binaryUint64(buf)&math.MaxInt64))
		}
		return "-0"
	}

	// FNV-1a across two 32-bit lanes, combined into 64 bits: far fewer
	// collisions than a single 32-bit hash.
	var h1 uint32 = 0x811c9dc5
	var h2 uint32 = 0x01000193
	for i := 0; i < len(seed); i++ {
		c := uint32(seed[i])
		h1 = (h1 ^ c) * 0x01000193
		h2 = (h2 ^ (c + uint32(i))) * 0x85ebca6b
	}
	combined := uint64(h1)*4294967296 + uint64(h2)
	return "-" + fmt.Sprintf("%d", combined)
}

func binaryUint64(b []byte) uint64 {
	var out uint64
	for i := 0; i < len(b) && i < 8; i++ {
		out = out<<8 | uint64(b[i])
	}
	return out
}

// mergeToolResultContent flattens a tool_result payload to the single string
// upstream's functionResponse expects.
func mergeToolResultContent(raw json.RawMessage, isError bool) string {
	if text, ok := rawAsString(raw); ok {
		return text
	}

	if len(raw) > 0 {
		var blocks []map[string]any
		if err := json.Unmarshal(raw, &blocks); err == nil {
			var parts []string
			for _, block := range blocks {
				if text, ok := block["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
			if merged := strings.Join(parts, "\n"); strings.TrimSpace(merged) != "" {
				return merged
			}
		}
		// Anything else: hand the model the raw JSON rather than nothing.
		var probe any
		if err := json.Unmarshal(raw, &probe); err == nil && probe != nil {
			return string(raw)
		}
	}

	if isError {
		return "Tool execution failed with no output."
	}
	return "Command executed successfully."
}

func parseFunctionCallArgs(args any) any {
	if args == nil {
		return map[string]any{}
	}
	if asString, ok := args.(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(asString), &parsed); err == nil {
			return parsed
		}
		return map[string]any{"value": asString}
	}
	return args
}

// buildParts converts one Anthropic message's content into upstream parts.
// toolIDToName carries tool ids forward so a later tool_result can name its call.
func buildParts(content MessageContent, toolIDToName map[string]string) []any {
	if content.IsString {
		return []any{map[string]any{"text": content.Text}}
	}

	var parts []any
	for _, block := range content.Blocks {
		switch block.Type {
		case "text":
			parts = append(parts, map[string]any{"text": block.Text})

		case "image":
			// Upstream accepts inline data only. Anything else has to be reported
			// rather than dropped: an image that silently disappears reaches the
			// model as a bare question about a picture it was never sent, and the
			// client just sees "I can't see an image" with nothing to debug.
			switch {
			case block.Source == nil:
				logx.Warn("[image] block has no source; dropping")
				parts = append(parts, map[string]any{
					"text": "[image omitted: the request contained an image block with no source]",
				})

			case block.Source.Type == "base64":
				if block.Source.Data == "" {
					logx.Warn("[image] base64 source carried no data; dropping")
					parts = append(parts, map[string]any{
						"text": "[image omitted: base64 source carried no data]",
					})
					break
				}
				parts = append(parts, map[string]any{
					"inlineData": map[string]any{
						"mimeType": block.Source.MediaType,
						"data":     block.Source.Data,
					},
				})

			case block.Source.Type == "url":
				// Matches how the OpenAI path handles a remote image: describe it,
				// so the omission is visible in the answer instead of invisible.
				logx.Warn("[image] url sources are not supported upstream (%s); "+
					"send the image as base64", block.Source.URL)
				parts = append(parts, map[string]any{
					"text": "[image omitted: " + block.Source.URL +
						" - this API accepts base64 image data only, not URLs]",
				})

			default:
				logx.Warn("[image] unsupported source type %q; send the image as base64",
					block.Source.Type)
				parts = append(parts, map[string]any{
					"text": "[image omitted: unsupported source type \"" +
						block.Source.Type + "\" - this API accepts base64 image data only]",
				})
			}

		case "tool_use":
			toolID := block.ID
			if toolID == "" {
				toolID = generateToolUseID()
			}
			toolName := block.Name
			if toolName == "" {
				toolName = toolID
			}
			upstreamName := SanitizeToolName(toolName)
			toolIDToName[toolID] = upstreamName

			input := block.Input
			if input == nil {
				input = map[string]any{}
			}
			functionCall := map[string]any{
				"name": upstreamName,
				"args": input,
				"id":   toolID,
			}
			// Gemini requires the thoughtSignature from the model's own
			// functionCall part to be echoed on the replay.
			if signature := GetToolCallSignature(toolID); signature != "" {
				parts = append(parts, map[string]any{
					"thoughtSignature": signature,
					"functionCall":     functionCall,
				})
			} else {
				parts = append(parts, map[string]any{"functionCall": functionCall})
			}

		case "tool_result":
			toolUseID := block.ToolUseID
			toolName := toolIDToName[toolUseID]
			if toolName == "" {
				toolName = toolUseID
			}
			if toolName == "" {
				toolName = "tool"
			}
			functionResponse := map[string]any{
				"name":     toolName,
				"response": map[string]any{"result": mergeToolResultContent(block.Content, block.IsError)},
			}
			if toolUseID != "" {
				functionResponse["id"] = toolUseID
			}
			parts = append(parts, map[string]any{"functionResponse": functionResponse})
		}
	}

	if len(parts) == 0 {
		return []any{map[string]any{"text": "[No text]"}}
	}
	return parts
}

func buildSafetySettings() []any {
	categories := []string{
		"HARM_CATEGORY_HARASSMENT",
		"HARM_CATEGORY_HATE_SPEECH",
		"HARM_CATEGORY_SEXUALLY_EXPLICIT",
		"HARM_CATEGORY_DANGEROUS_CONTENT",
		"HARM_CATEGORY_CIVIC_INTEGRITY",
	}
	out := make([]any, 0, len(categories))
	for _, category := range categories {
		out = append(out, map[string]any{"category": category, "threshold": "OFF"})
	}
	return out
}

// buildSystemInstruction prefers the client's own system prompt. Agent clients
// put their entire tool/verification contract in there, so overriding it with a
// generic persona throws that contract away.
func buildSystemInstruction(system string) map[string]any {
	text := strings.TrimSpace(system)
	if text == "" {
		text = antigravityIdentity
	}
	return map[string]any{
		"role": "user",
		"parts": []any{
			map[string]any{"text": text},
			map[string]any{"text": "\n--- [SYSTEM_PROMPT_END] ---"},
		},
	}
}

func buildFunctionCallingConfig(choice *ToolChoice) map[string]any {
	if choice == nil {
		return map[string]any{"mode": "VALIDATED"}
	}
	switch choice.Type {
	case "none":
		return map[string]any{"mode": "NONE"}
	case "any":
		return map[string]any{"mode": "ANY"}
	case "tool":
		out := map[string]any{"mode": "ANY"}
		if choice.Name != "" {
			out["allowedFunctionNames"] = []any{SanitizeToolName(choice.Name)}
		}
		return out
	default:
		return map[string]any{"mode": "VALIDATED"}
	}
}

func clampMaxOutputTokens(maxTokens int) int {
	if maxTokens <= 0 {
		return defaultMaxOutputTokens
	}
	if maxTokens > defaultMaxOutputTokens {
		return defaultMaxOutputTokens
	}
	return maxTokens
}

var thinkingSuffix = regexp.MustCompile(`(?i)-(low|medium|high)$`)

// buildUpstreamRequest converts a ChatRequest into the upstream JSON envelope.
// upstreamModel is the mapped id; originalModel is what the client asked for,
// whose -low/-medium/-high suffix selects the thinking level.
func buildUpstreamRequest(
	upstreamModel string,
	req ChatRequest,
	originalModel string,
	projectID string,
) map[string]any {
	toolIDToName := map[string]string{}

	contents := make([]any, 0, len(req.Messages))
	for _, msg := range req.Messages {
		// A system turn travels in systemInstruction, never in contents.
		if msg.Role == "system" {
			continue
		}
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{
			"role":  role,
			"parts": buildParts(msg.Content, toolIDToName),
		})
	}

	if projectID == "" {
		projectID = "unknown"
	}

	generationConfig := map[string]any{
		"maxOutputTokens": clampMaxOutputTokens(req.MaxTokens),
		"stopSequences":   []any{"\n\nHuman:", "[DONE]"},
	}
	if req.Temperature != nil && !math.IsNaN(*req.Temperature) && !math.IsInf(*req.Temperature, 0) {
		generationConfig["temperature"] = *req.Temperature
	}
	if req.TopP != nil && !math.IsNaN(*req.TopP) && !math.IsInf(*req.TopP, 0) {
		generationConfig["topP"] = *req.TopP
	}
	if req.TopK != nil && !math.IsNaN(*req.TopK) && !math.IsInf(*req.TopK, 0) {
		generationConfig["topK"] = *req.TopK
	}

	inner := map[string]any{
		"contents":          contents,
		"sessionId":         generateStableSessionID(req.Messages, req.System),
		"safetySettings":    buildSafetySettings(),
		"systemInstruction": buildSystemInstruction(req.System),
		"generationConfig":  generationConfig,
	}

	if req.ToolChoice != nil {
		inner["toolConfig"] = map[string]any{
			"functionCallingConfig": buildFunctionCallingConfig(req.ToolChoice),
		}
	}

	// Thinking configuration. An explicit numeric budget from the client wins
	// over the level implied by the model-id suffix. maxOutputTokens must stay
	// above the budget, so it is clamped one below.
	maxOut := clampMaxOutputTokens(req.MaxTokens)
	if req.ThinkingBudget > 0 {
		budget := req.ThinkingBudget
		if budget > maxOut-1 {
			budget = maxOut - 1
		}
		if budget > 0 {
			generationConfig["thinkingConfig"] = map[string]any{
				"includeThoughts": true,
				"thinkingBudget":  budget,
			}
		}
	}
	if _, hasThinking := generationConfig["thinkingConfig"]; !hasThinking &&
		strings.Contains(upstreamModel, "gemini") {
		match := thinkingSuffix.FindStringSubmatch(strings.ToLower(originalModel))
		agentDefaultHigh := strings.Contains(upstreamModel, "agent") && match == nil
		if match != nil || agentDefaultHigh {
			level := "high"
			if match != nil {
				level = match[1]
			}
			generationConfig["thinkingConfig"] = map[string]any{
				"includeThoughts": true,
				"thinkingLevel":   level,
			}
		}
	}

	if len(req.Tools) > 0 {
		// Gemini's contract is ONE tools entry holding every declaration, not
		// one entry per tool.
		declarations := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			declaration := map[string]any{
				"name":       SanitizeToolName(tool.Name),
				"parameters": NormalizeToolParameters(tool.InputSchema),
			}
			if tool.Description != "" {
				declaration["description"] = tool.Description
			}
			declarations = append(declarations, declaration)
		}
		inner["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}

	return map[string]any{
		"model":       upstreamModel,
		"userAgent":   UserAgent(),
		"requestType": "agent",
		"project":     projectID,
		"requestId":   generateRequestID(),
		"request":     inner,
	}
}
