// Package apperr carries the error shapes the HTTP layer knows how to render.
package apperr

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AntigravityError is a local failure (no IDE data, bad token, ...).
type AntigravityError struct {
	Message string
	Code    string
}

func (e *AntigravityError) Error() string { return e.Message }

// NewAntigravity builds an AntigravityError, defaulting the code.
func NewAntigravity(message, code string) *AntigravityError {
	if code == "" {
		code = "antigravity_error"
	}
	return &AntigravityError{Message: message, Code: code}
}

// UpstreamError is a non-2xx from the provider, carrying the raw body so the
// 429 classifier below can read Google's reason codes out of it.
type UpstreamError struct {
	Provider   string
	Status     int
	Body       string
	RetryAfter string
	// Retryable marks a 429 the router may fall back on rather than surface.
	Retryable bool
	// StreamingStarted records that bytes already reached the client, so the
	// caller must not restart the response.
	StreamingStarted bool
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("%s upstream error (%d)", e.Provider, e.Status)
}

// NewUpstream builds an UpstreamError.
func NewUpstream(provider string, status int, body, retryAfter string) *UpstreamError {
	return &UpstreamError{Provider: provider, Status: status, Body: body, RetryAfter: retryAfter}
}

// Reason429 enumerates why upstream returned 429. The distinction matters:
// quota_exhausted means rotate to another account, the rest mean wait.
type Reason429 string

const (
	ReasonQuotaExhausted  Reason429 = "quota_exhausted"
	ReasonRateLimit       Reason429 = "rate_limit_exceeded"
	ReasonModelCapacity   Reason429 = "model_capacity_exhausted"
	ReasonResourceExalted Reason429 = "resource_exhausted"
	ReasonUnknown         Reason429 = "unknown"
)

type parsedUpstream struct {
	Reason  string
	Message string
	Status  string
	Type    string
}

func parseUpstreamBody(body string) parsedUpstream {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return parsedUpstream{}
	}
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return parsedUpstream{Message: trimmed}
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return parsedUpstream{Message: trimmed}
	}

	errNode := raw
	if nested, ok := raw["error"].(map[string]any); ok {
		errNode = nested
	}

	out := parsedUpstream{}
	if s, ok := errNode["type"].(string); ok {
		out.Type = s
	}
	if s, ok := errNode["message"].(string); ok {
		out.Message = s
	}
	if s, ok := errNode["status"].(string); ok {
		out.Status = s
	}
	if details, ok := errNode["details"].([]any); ok {
		for _, detail := range details {
			node, ok := detail.(map[string]any)
			if !ok {
				continue
			}
			if reason, ok := node["reason"].(string); ok {
				out.Reason = reason
				break
			}
		}
	}
	return out
}

// Summarize429 classifies a 429 body into a reason plus a human sentence.
func Summarize429(e *UpstreamError) (Reason429, string) {
	providerName := e.Provider
	if providerName == "antigravity" {
		providerName = "Antigravity"
	}

	parsed := parseUpstreamBody(e.Body)
	lower := strings.ToLower(parsed.Message)

	reason := ReasonUnknown
	switch {
	case parsed.Reason == "QUOTA_EXHAUSTED",
		parsed.Type == "usage_limit_reached",
		strings.Contains(lower, "usage limit"),
		strings.Contains(lower, "quota") && strings.Contains(lower, "reset"):
		reason = ReasonQuotaExhausted
	case parsed.Reason == "MODEL_CAPACITY_EXHAUSTED",
		strings.Contains(lower, "no capacity"),
		strings.Contains(lower, "capacity"):
		reason = ReasonModelCapacity
	case parsed.Reason == "RATE_LIMIT_EXCEEDED",
		strings.Contains(lower, "rate limit"),
		strings.Contains(lower, "per minute"),
		strings.Contains(lower, "too many requests"):
		reason = ReasonRateLimit
	case parsed.Status == "RESOURCE_EXHAUSTED":
		reason = ReasonResourceExalted
	}

	switch reason {
	case ReasonQuotaExhausted:
		return reason, fmt.Sprintf("%s quota exhausted for this account or model.", providerName)
	case ReasonModelCapacity:
		return reason, fmt.Sprintf("%s model capacity exhausted (temporary). Quota may still be available.", providerName)
	case ReasonRateLimit:
		return reason, fmt.Sprintf("%s rate limit exceeded (requests too fast). Quota may still be available.", providerName)
	case ReasonResourceExalted:
		return reason, fmt.Sprintf("%s resource exhausted (temporary). Quota may still be available.", providerName)
	default:
		return ReasonUnknown, fmt.Sprintf("%s upstream error (429).", providerName)
	}
}

// SummarizeUpstream renders any UpstreamError for the client.
func SummarizeUpstream(e *UpstreamError) (message string, reason string) {
	if e.Status == 429 {
		r, m := Summarize429(e)
		return m, string(r)
	}
	if e.Body != "" {
		return e.Body, ""
	}
	return e.Error(), ""
}

// LogReason is the short tag written to the X-Log-Reason header.
func LogReason(err error) string {
	switch e := err.(type) {
	case *UpstreamError:
		switch {
		case e.Status == 429:
			reason, _ := Summarize429(e)
			switch reason {
			case ReasonQuotaExhausted:
				return "quota exhausted"
			case ReasonModelCapacity:
				return "model capacity exhausted"
			case ReasonResourceExalted:
				return "resource exhausted"
			default:
				return "rate limited"
			}
		case e.Status == 401:
			return "unauthorized"
		case e.Status == 403:
			return "forbidden"
		case e.Status == 404:
			return "not found"
		default:
			return "upstream error"
		}
	case *AntigravityError:
		if e.Code != "" {
			return e.Code
		}
		return "antigravity error"
	}
	return "internal error"
}
