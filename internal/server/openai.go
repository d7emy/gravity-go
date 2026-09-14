package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gravity-go/internal/antigravity"
	"gravity-go/internal/apperr"
	"gravity-go/internal/logx"
)

// openAIRequest is the /v1/chat/completions payload.
type openAIRequest struct {
	Model           string          `json:"model"`
	Messages        []openAIMessage `json:"messages"`
	MaxTokens       *int            `json:"max_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Stream          *bool           `json:"stream"`
	Tools           []openAITool    `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	ReasoningEffort string          `json:"reasoning_effort"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
	ToolCallID string           `json:"tool_call_id"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

// dataURLPattern matches a base64 data URL. The scheme and the ";base64" marker
// are matched case-insensitively, and the media type is optional, because
// clients emit all of these for the same file:
//
//	data:image/jpeg;base64,...   data:image/jpg;base64,...
//	data:;base64,...             DATA:IMAGE/JPEG;BASE64,...
//	data:image/jpeg;charset=utf-8;base64,...
//
// Rejecting a spelling meant the image was passed on as text instead, and the
// model then invented an answer rather than reporting a missing picture.
//
// The s flag matters: some encoders wrap base64 at 76 columns, and without it
// the trailing group stops at the first newline and the whole match fails.
var dataURLPattern = regexp.MustCompile(`(?is)^data:([^;,]*)(?:;[^,]*?)??;?base64,(.*)$`)

// imageMagic maps a file signature to its media type, so an image whose data URL
// carried no usable type is still sent with the right one rather than being
// dropped or mislabelled.
var imageMagic = []struct {
	prefix []byte
	mime   string
}{
	{[]byte{0xFF, 0xD8, 0xFF}, "image/jpeg"},
	{[]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, "image/png"},
	{[]byte("GIF87a"), "image/gif"},
	{[]byte("GIF89a"), "image/gif"},
}

// sniffImageMime identifies an image from its leading bytes. RIFF/WEBP needs a
// second check because the format tag sits after the length field.
func sniffImageMime(data []byte) string {
	for _, m := range imageMagic {
		if bytes.HasPrefix(data, m.prefix) {
			return m.mime
		}
	}
	if len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) &&
		bytes.Equal(data[8:12], []byte("WEBP")) {
		return "image/webp"
	}
	return ""
}

// normalizeImageMime settles on the media type to send upstream. The bytes are
// trusted over the declared type: a client that labels a JPEG "image/jpg" (not a
// real media type) or omits the type entirely is common, and the file itself is
// unambiguous.
func normalizeImageMime(declared string, data []byte) string {
	if sniffed := sniffImageMime(data); sniffed != "" {
		return sniffed
	}
	declared = strings.ToLower(strings.TrimSpace(declared))
	if declared == "image/jpg" {
		return "image/jpeg"
	}
	if declared == "" {
		return "image/jpeg"
	}
	return declared
}

// decodeImageDataURL pulls the image out of a data URL, tolerating the wrapping
// and padding variations clients produce. It reports whether this was a data URL
// at all.
func decodeImageDataURL(raw string) (mime, b64 string, ok bool) {
	m := dataURLPattern.FindStringSubmatch(raw)
	if m == nil {
		return "", "", false
	}
	// Some clients wrap long base64 across lines, or send it URL-encoded.
	payload := strings.NewReplacer(
		"\n", "",
		"\r", "",
		" ", "",
		"\t", "",
	).Replace(m[2])
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// Tolerate missing "=" padding and the URL-safe alphabet.
		if d2, err2 := base64.RawStdEncoding.DecodeString(payload); err2 == nil {
			decoded = d2
		} else if d3, err3 := base64.URLEncoding.DecodeString(payload); err3 == nil {
			decoded = d3
		} else {
			logx.Warn("[image] data URL payload is not valid base64: %v", err)
			return "", "", false
		}
	}
	if len(decoded) == 0 {
		logx.Warn("[image] data URL decoded to zero bytes")
		return "", "", false
	}
	return normalizeImageMime(m[1], decoded),
		base64.StdEncoding.EncodeToString(decoded), true
}

// describeUnusableImage explains why an image_url could not be inlined, without
// quoting the value itself.
func describeUnusableImage(raw string) string {
	trimmed := strings.TrimSpace(raw)
	lower := strings.ToLower(trimmed)

	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		// A remote URL is short and safe to name; it is the one case where
		// echoing it back actually helps the reader.
		if len(trimmed) > 200 {
			trimmed = trimmed[:200] + "..."
		}
		return "remote URL " + trimmed + " - this API accepts base64 image data only"
	case strings.HasPrefix(lower, "file://"):
		return "file:// URLs cannot be read by this API - send the image as base64"
	case strings.HasPrefix(lower, "data:"):
		return "the data URL could not be decoded - check it is valid base64"
	default:
		return "unrecognised image reference - this API accepts base64 image data " +
			"only, as data:<media-type>;base64,<data>"
	}
}

// contentParts decodes OpenAI's content field, which is a string or a list of
// typed parts.
func contentParts(raw json.RawMessage) (string, []map[string]any, bool) {
	if len(raw) == 0 {
		return "", nil, true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil, true
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, false
	}
	return "", parts, false
}

// contentToText flattens content to plain text, for system prompt extraction.
func contentToText(raw json.RawMessage) string {
	text, parts, isString := contentParts(raw)
	if isString {
		return text
	}
	var out []string
	for _, part := range parts {
		if part["type"] == "text" {
			if s, ok := part["text"].(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return strings.Join(out, "\n")
}

// translateContent converts OpenAI content into Anthropic content blocks.
func translateContent(raw json.RawMessage) antigravity.MessageContent {
	text, parts, isString := contentParts(raw)
	if isString {
		return antigravity.MessageContent{IsString: true, Text: text}
	}

	var blocks []antigravity.ContentBlock
	for _, part := range parts {
		switch part["type"] {
		case "text":
			if s, ok := part["text"].(string); ok && s != "" {
				blocks = append(blocks, antigravity.ContentBlock{Type: "text", Text: s})
			}
		case "image_url":
			imageURL, ok := part["image_url"].(map[string]any)
			if !ok {
				continue
			}
			raw, ok := imageURL["url"].(string)
			if !ok {
				continue
			}
			if mime, data, ok := decodeImageDataURL(raw); ok {
				blocks = append(blocks, antigravity.ContentBlock{
					Type: "image",
					Source: &antigravity.ImageSource{
						Type:      "base64",
						MediaType: mime,
						Data:      data,
					},
				})
				continue
			}

			// Upstream inlines image data only; it cannot fetch anything. Say so
			// in a short marker.
			//
			// The marker must never contain the original string. Pasting an
			// unparsed data URL back in fed megabytes of base64 to the model as
			// prose, and pasting a file path fed it the file name -- from which it
			// confidently answered questions about a picture it had never seen.
			// A wrong answer is worse than a missing one.
			description := describeUnusableImage(raw)
			logx.Warn("[image] %s", description)
			blocks = append(blocks, antigravity.ContentBlock{
				Type: "text", Text: "[image omitted: " + description + "]",
			})
		}
	}
	if len(blocks) == 0 {
		return antigravity.MessageContent{IsString: true, Text: ""}
	}
	return antigravity.MessageContent{Blocks: blocks}
}

// extractOpenAISystemPrompt pulls system/developer turns out of the message list
// so they can travel as a real system instruction. Demoting them to user turns
// strips their authority, which matters most for agent clients whose whole tool
// contract lives in there.
func extractOpenAISystemPrompt(messages []openAIMessage) string {
	var parts []string
	for _, msg := range messages {
		if msg.Role == "system" || msg.Role == "developer" {
			if text := contentToText(msg.Content); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func translateOpenAIMessages(messages []openAIMessage) []antigravity.Message {
	out := make([]antigravity.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "system" || msg.Role == "developer" {
			continue
		}

		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			blocks := make([]antigravity.ContentBlock, 0, len(msg.ToolCalls))
			for _, call := range msg.ToolCalls {
				var input any
				if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
					input = map[string]any{}
				}
				blocks = append(blocks, antigravity.ContentBlock{
					Type:  "tool_use",
					ID:    call.ID,
					Name:  call.Function.Name,
					Input: input,
				})
			}
			out = append(out, antigravity.Message{
				Role:    "assistant",
				Content: antigravity.MessageContent{Blocks: blocks},
			})
			continue
		}

		if msg.Role == "tool" {
			// tool_result takes a string; multimodal parts are flattened.
			text := contentToText(msg.Content)
			encoded, _ := json.Marshal(text)
			out = append(out, antigravity.Message{
				Role: "user",
				Content: antigravity.MessageContent{Blocks: []antigravity.ContentBlock{{
					Type:      "tool_result",
					ToolUseID: msg.ToolCallID,
					Content:   encoded,
				}}},
			})
			continue
		}

		role := "user"
		if msg.Role == "assistant" {
			role = "assistant"
		}
		out = append(out, antigravity.Message{Role: role, Content: translateContent(msg.Content)})
	}
	return out
}

func translateOpenAITools(tools []openAITool) []antigravity.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]antigravity.Tool, 0, len(tools))
	for _, tool := range tools {
		schema := tool.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, antigravity.Tool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}

// translateToolChoice maps OpenAI's tool_choice onto the internal shape.
func translateToolChoice(raw json.RawMessage) *antigravity.ToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch asString {
		case "auto":
			return &antigravity.ToolChoice{Type: "auto"}
		case "none":
			return &antigravity.ToolChoice{Type: "none"}
		case "required":
			return &antigravity.ToolChoice{Type: "any"}
		}
		return nil
	}
	var object struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	if object.Type == "function" && object.Function.Name != "" {
		return &antigravity.ToolChoice{Type: "tool", Name: object.Function.Name}
	}
	return nil
}

// reasoningBudget maps an effort level onto a concrete token budget.
func reasoningBudget(req *openAIRequest) int {
	effort := req.ReasoningEffort
	if effort == "" && req.Reasoning != nil {
		effort = req.Reasoning.Effort
	}
	switch effort {
	case "low":
		return 2048
	case "medium":
		return 8192
	case "high":
		return 24576
	}
	return 0
}

func handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}

	var req openAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body")
		return
	}
	if req.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Model is required")
		return
	}
	if msg := validateModelName(req.Model); msg != "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Messages array cannot be empty")
		return
	}
	if len(req.Messages) > maxMessagesPerRequest {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Too many messages (max "+strconv.Itoa(maxMessagesPerRequest)+")")
		return
	}
	for i, msg := range req.Messages {
		if msg.Role == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Message at index "+strconv.Itoa(i)+" must have a role")
			return
		}
	}
	if req.MaxTokens != nil && *req.MaxTokens > maxTokensLimit {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens too large (max "+strconv.Itoa(maxTokensLimit)+")")
		return
	}
	if msg := validateTemperature(req.Temperature); msg != "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}
	if msg := validateTopP(req.TopP); msg != "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}
	if len(req.Tools) > maxToolsPerRequest {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Too many tools (max "+strconv.Itoa(maxToolsPerRequest)+")")
		return
	}

	globalLimiter.wait(r.Context())

	maxTokens := 0
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}

	chatReq := antigravity.ChatRequest{
		Model:          antigravity.MapOpenAIModel(req.Model),
		Messages:       translateOpenAIMessages(req.Messages),
		Tools:          translateOpenAITools(req.Tools),
		ToolChoice:     translateToolChoice(req.ToolChoice),
		MaxTokens:      maxTokens,
		System:         extractOpenAISystemPrompt(req.Messages),
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		ThinkingBudget: reasoningBudget(&req),
	}
	opts := antigravity.CallOptions{AllowRotation: true}

	if req.Stream != nil && *req.Stream {
		streamOpenAI(w, r, &req, chatReq, opts)
		return
	}

	result, err := antigravity.CreateChatCompletion(r.Context(), chatReq, opts)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	var textContent strings.Builder
	var toolCalls []map[string]any
	for _, block := range result.ContentBlocks {
		switch block.Type {
		case "text":
			textContent.WriteString(block.Text)
		case "tool_use":
			arguments, _ := json.Marshal(block.Input)
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": string(arguments),
				},
			})
		}
	}

	message := map[string]any{"role": "assistant"}
	if len(toolCalls) > 0 {
		message["content"] = nil
		message["tool_calls"] = toolCalls
	} else {
		message["content"] = textContent.String()
	}
	if result.Reasoning != "" {
		// Clients disagree on the field name: DeepSeek-style reads
		// reasoning_content, OpenRouter-style reads reasoning. Send both.
		message["reasoning_content"] = result.Reasoning
		message["reasoning"] = result.Reasoning
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      generateChatID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": mapStopReason(result.StopReason),
		}},
		"usage": map[string]any{
			"prompt_tokens":     result.Usage.InputTokens,
			"completion_tokens": result.Usage.OutputTokens,
			"total_tokens":      result.Usage.InputTokens + result.Usage.OutputTokens,
		},
	})
}

func mapStopReason(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

func generateChatID() string {
	return "chatcmpl-" + strings.TrimPrefix(antigravity.GenerateMessageID(), "msg_")
}

// openAIStreamState accumulates Anthropic events and re-emits them as OpenAI
// chunks.
type openAIStreamState struct {
	chatID          string
	model           string
	write           func(string) error
	sentRole        bool
	currentToolCall *pendingToolCall
	toolCalls       []pendingToolCall
	inputTokens     int64
	outputTokens    int64
}

type pendingToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func (s *openAIStreamState) chunk(payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.write("data: " + string(encoded) + "\n\n")
}

func (s *openAIStreamState) baseChunk(delta map[string]any, finishReason any) map[string]any {
	return map[string]any{
		"id":      s.chatID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
}

// handleEvent consumes one Anthropic SSE frame.
func (s *openAIStreamState) handleEvent(frame string) error {
	for _, line := range strings.Split(frame, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimSpace(line[6:])
		if data == "" || data == "[DONE]" {
			continue
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			continue
		}
		if err := s.dispatch(parsed); err != nil {
			return err
		}
	}
	return nil
}

func (s *openAIStreamState) dispatch(parsed map[string]any) error {
	switch parsed["type"] {
	case "message_start":
		if !s.sentRole {
			s.sentRole = true
			if err := s.chunk(s.baseChunk(map[string]any{"role": "assistant"}, nil)); err != nil {
				return err
			}
		}
		if message, ok := parsed["message"].(map[string]any); ok {
			if usage, ok := message["usage"].(map[string]any); ok {
				if v, ok := usage["input_tokens"].(float64); ok && v > 0 {
					s.inputTokens = int64(v)
				}
			}
		}

	case "content_block_start":
		block, ok := parsed["content_block"].(map[string]any)
		if ok && block["type"] == "tool_use" {
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			s.currentToolCall = &pendingToolCall{ID: id, Name: name}
		}

	case "content_block_delta":
		delta, ok := parsed["delta"].(map[string]any)
		if !ok {
			return nil
		}
		switch delta["type"] {
		case "thinking_delta":
			if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
				return s.chunk(s.baseChunk(map[string]any{
					"reasoning_content": thinking,
					"reasoning":         thinking,
				}, nil))
			}
		case "text_delta":
			if text, ok := delta["text"].(string); ok && text != "" {
				return s.chunk(s.baseChunk(map[string]any{"content": text}, nil))
			}
		case "input_json_delta":
			if s.currentToolCall != nil {
				if partial, ok := delta["partial_json"].(string); ok {
					s.currentToolCall.Arguments.WriteString(partial)
				}
			}
		}

	case "content_block_stop":
		if s.currentToolCall != nil {
			s.toolCalls = append(s.toolCalls, *s.currentToolCall)
			s.currentToolCall = nil
		}

	case "message_delta":
		stopReason := "end_turn"
		if delta, ok := parsed["delta"].(map[string]any); ok {
			if reason, ok := delta["stop_reason"].(string); ok && reason != "" {
				stopReason = reason
			}
		}
		if usage, ok := parsed["usage"].(map[string]any); ok {
			if v, ok := usage["output_tokens"].(float64); ok && v > 0 {
				s.outputTokens = int64(v)
			}
			if v, ok := usage["input_tokens"].(float64); ok && v > 0 {
				s.inputTokens = int64(v)
			}
		}

		if len(s.toolCalls) > 0 {
			calls := make([]any, 0, len(s.toolCalls))
			for i, call := range s.toolCalls {
				arguments := call.Arguments.String()
				if arguments == "" {
					arguments = "{}"
				}
				calls = append(calls, map[string]any{
					"index": i,
					"id":    call.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      call.Name,
						"arguments": arguments,
					},
				})
			}
			if err := s.chunk(s.baseChunk(map[string]any{"tool_calls": calls},
				mapStopReason("tool_use"))); err != nil {
				return err
			}
		}
		return s.chunk(s.baseChunk(map[string]any{}, mapStopReason(stopReason)))
	}
	return nil
}

func streamOpenAI(
	w http.ResponseWriter, r *http.Request, req *openAIRequest,
	chatReq antigravity.ChatRequest, opts antigravity.CallOptions,
) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	stream := newSSEWriter(w)
	defer startKeepAlive(r.Context(), stream)()

	state := &openAIStreamState{
		chatID: generateChatID(),
		model:  req.Model,
		write:  stream.write,
	}

	err := antigravity.CreateChatCompletionStream(r.Context(), chatReq, opts, state.handleEvent)
	if err != nil {
		var upstream *apperr.UpstreamError
		if errors.As(err, &upstream) {
			message, reason := apperr.SummarizeUpstream(upstream)
			logx.Error("OpenAI stream error: %s", message)
			payload := map[string]any{
				"type":     "upstream_error",
				"message":  message,
				"provider": upstream.Provider,
			}
			if reason != "" {
				payload["reason"] = reason
			}
			encoded, _ := json.Marshal(map[string]any{"error": payload})
			_ = stream.write("data: " + string(encoded) + "\n\n")
		} else {
			if r.Context().Err() == nil {
				logx.Error("OpenAI stream error: %v", err)
			}
			encoded, _ := json.Marshal(map[string]any{
				"error": map[string]any{"message": err.Error(), "type": "api_error"},
			})
			_ = stream.write("data: " + string(encoded) + "\n\n")
		}
		_ = stream.write("data: [DONE]\n\n")
		return
	}

	// Clients that opt in via stream_options.include_usage expect a final
	// usage-only chunk before [DONE].
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		encoded, _ := json.Marshal(map[string]any{
			"id":      state.chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     state.inputTokens,
				"completion_tokens": state.outputTokens,
				"total_tokens":      state.inputTokens + state.outputTokens,
			},
		})
		_ = stream.write("data: " + string(encoded) + "\n\n")
	}

	_ = stream.write("data: [DONE]\n\n")
}

func writeOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("X-Log-Reason", message)
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"type": errType, "message": message},
	})
}
