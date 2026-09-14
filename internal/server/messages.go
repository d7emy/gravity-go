package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gravity-go/internal/antigravity"
	"gravity-go/internal/apperr"
	"gravity-go/internal/logx"
)

// anthropicRequest is the /v1/messages payload.
type anthropicRequest struct {
	Model       string                    `json:"model"`
	Messages    []antigravity.Message     `json:"messages"`
	MaxTokens   int                       `json:"max_tokens"`
	System      json.RawMessage           `json:"system"`
	Stream      bool                      `json:"stream"`
	Temperature *float64                  `json:"temperature"`
	TopP        *float64                  `json:"top_p"`
	TopK        *float64                  `json:"top_k"`
	Tools       []antigravity.Tool        `json:"tools"`
	ToolChoice  *antigravity.ToolChoice   `json:"tool_choice"`
	Thinking    *anthropicThinkingRequest `json:"thinking"`
}

type anthropicThinkingRequest struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// extractSystemPrompt reads Anthropic's `system`, which is a string or a list of
// text blocks. Dropping it entirely (as an early version did) throws away the
// whole agent contract of clients like Claude Code.
func extractSystemPrompt(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func validateAnthropicRequest(req *anthropicRequest) string {
	if req.Model == "" {
		return "Model is required and must be a string"
	}
	if msg := validateModelName(req.Model); msg != "" {
		return msg
	}
	if req.Messages == nil {
		return "Messages must be an array"
	}
	if len(req.Messages) == 0 {
		return "Messages array cannot be empty"
	}
	if len(req.Messages) > maxMessagesPerRequest {
		return "Too many messages (max " + strconv.Itoa(maxMessagesPerRequest) + ")"
	}
	for i, msg := range req.Messages {
		if msg.Role == "" {
			return "Message at index " + strconv.Itoa(i) + " must have a role"
		}
	}
	if req.MaxTokens < 0 {
		return "max_tokens must be a positive number"
	}
	if req.MaxTokens > 0 {
		if msg := validateMaxTokens(req.MaxTokens); msg != "" {
			return msg
		}
	}
	if msg := validateTemperature(req.Temperature); msg != "" {
		return msg
	}
	if msg := validateTopP(req.TopP); msg != "" {
		return msg
	}
	if msg := validateTopK(req.TopK); msg != "" {
		return msg
	}
	if req.Tools != nil {
		if len(req.Tools) > maxToolsPerRequest {
			return "Too many tools (max " + strconv.Itoa(maxToolsPerRequest) + ")"
		}
	}
	return ""
}

func toChatRequest(req *anthropicRequest) antigravity.ChatRequest {
	out := antigravity.ChatRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		MaxTokens:   req.MaxTokens,
		System:      extractSystemPrompt(req.System),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		TopK:        req.TopK,
	}
	if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
		out.ThinkingBudget = req.Thinking.BudgetTokens
	}
	return out
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}

	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body")
		return
	}
	if message := validateAnthropicRequest(&req); message != "" {
		w.Header().Set("X-Log-Reason", message)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", message)
		return
	}

	globalLimiter.wait(r.Context())

	chatReq := toChatRequest(&req)
	opts := antigravity.CallOptions{AllowRotation: true}

	if req.Stream {
		streamAnthropic(w, r, chatReq, opts)
		return
	}

	result, err := antigravity.CreateChatCompletion(r.Context(), chatReq, opts)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	content := make([]map[string]any, 0, len(result.ContentBlocks)+1)
	if result.Reasoning != "" {
		content = append(content, map[string]any{
			"type":      "thinking",
			"thinking":  result.Reasoning,
			"signature": "",
		})
	}
	for _, block := range result.ContentBlocks {
		if block.Type == "tool_use" {
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    block.ID,
				"name":  block.Name,
				"input": block.Input,
			})
			continue
		}
		content = append(content, map[string]any{"type": "text", "text": block.Text})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":            antigravity.GenerateMessageID(),
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         req.Model,
		"stop_reason":   result.StopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  result.Usage.InputTokens,
			"output_tokens": result.Usage.OutputTokens,
		},
	})
}

// sseWriter serializes writes to the response and flushes each frame, so the
// keep-alive goroutine and the stream cannot interleave mid-frame.
type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	failed  bool
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	flusher, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: flusher}
}

func (s *sseWriter) write(payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return errors.New("stream closed")
	}
	if _, err := io.WriteString(s.w, payload); err != nil {
		s.failed = true
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

// startKeepAlive emits an SSE comment frame every 15s until the returned stop
// function is called or the request ends. A reasoning model can stay silent for
// minutes before its first token, and an idle connection is exactly what proxies
// and clients time out; a comment frame is ignored by SSE parsers but keeps the
// socket alive.
//
// Both streaming endpoints need this. Only the Anthropic one had it, so an
// OpenAI-endpoint client could lose a slow request while the model was thinking.
// keepAliveInterval is a var, not a const, so tests can shrink it instead of
// waiting out a real 15-second tick.
var keepAliveInterval = 15 * time.Second

func startKeepAlive(ctx context.Context, stream *sseWriter) func() {
	pingCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Jittered around keepAliveInterval (≈13..17s in production) so idle
		// streams across processes do not ping in lock-step. The test base of
		// 15ms jitters proportionally, so silence tests are unaffected.
		timer := time.NewTimer(antigravity.KeepAliveDelay(keepAliveInterval))
		defer timer.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-timer.C:
				if err := stream.write(": ping\n\n"); err != nil {
					return
				}
				timer.Reset(antigravity.KeepAliveDelay(keepAliveInterval))
			}
		}
	}()

	// Cancelling alone is not enough: the goroutine may already be past its
	// select and about to write. Since this stop runs as the handler's deferred
	// cleanup, returning early would let a ping land after the handler returned,
	// which is not allowed on a ResponseWriter. Wait for the goroutine to exit.
	return func() {
		cancel()
		<-done
	}
}

func streamAnthropic(
	w http.ResponseWriter, r *http.Request,
	chatReq antigravity.ChatRequest, opts antigravity.CallOptions,
) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	stream := newSSEWriter(w)
	defer startKeepAlive(r.Context(), stream)()

	err := antigravity.CreateChatCompletionStream(r.Context(), chatReq, opts, stream.write)
	if err == nil {
		return
	}

	var upstream *apperr.UpstreamError
	if errors.As(err, &upstream) && upstream.Status == 429 {
		logx.Warn("Stream error: Antigravity 429 rate limit (rotation may continue)")
	} else if r.Context().Err() == nil {
		logx.Error("Stream error: %v", err)
	}

	// The response is already committed, so the failure has to travel as an
	// in-band SSE error event.
	payload, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": err.Error()},
	})
	_ = stream.write("event: error\ndata: " + string(payload) + "\n\n")
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"type": errType, "message": message},
	})
}

// writeUpstreamError renders a provider failure for the client.
func writeUpstreamError(w http.ResponseWriter, err error) {
	var upstream *apperr.UpstreamError
	if errors.As(err, &upstream) {
		message, reason := apperr.SummarizeUpstream(upstream)
		w.Header().Set("X-Log-Reason", apperr.LogReason(upstream))

		payload := map[string]any{
			"type":     "upstream_error",
			"message":  message,
			"provider": upstream.Provider,
		}
		if reason != "" {
			payload["reason"] = reason
		}
		if upstream.Status == 429 && upstream.Body != "" {
			detail := upstream.Body
			if len(detail) > 800 {
				detail = detail[:800]
			}
			payload["detail"] = detail
		}
		writeJSON(w, upstream.Status, map[string]any{"error": payload})
		return
	}

	var local *apperr.AntigravityError
	if errors.As(err, &local) {
		w.Header().Set("X-Log-Reason", apperr.LogReason(local))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"type": local.Code, "message": local.Message},
		})
		return
	}

	w.Header().Set("X-Log-Reason", apperr.LogReason(err))
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": map[string]any{"type": "error", "message": err.Error()},
	})
}
