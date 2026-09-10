package server

import (
	"math"
	"strconv"
)

// Validation ceilings.
//
// maxMessagesPerRequest was 1000, carried over from the TypeScript build. That
// is far too low for real agentic use: every tool call and its result is a
// message, so a long session reaches four figures quickly, and a 669k-token
// opencode session was rejected outright. The genuine limits are the model's
// context window and the 64MB body cap, both enforced elsewhere; this is only a
// backstop against a pathological payload, so it is set well clear of anything
// a real conversation produces.
const (
	maxModelNameLength    = 256
	maxMessagesPerRequest = 100000
	// Also inherited from the TypeScript build. A single client sends 15-20
	// tools, but stacking MCP servers multiplies that quickly, and the ceiling
	// applies to the whole set. Gemini takes every declaration in one envelope,
	// so the real cost is prompt tokens, which the context window already bounds.
	maxToolsPerRequest = 10000
	maxTokensLimit     = 1000000
)

func validateModelName(model string) string {
	if len(model) > maxModelNameLength {
		return "Model name too long (max " + strconv.Itoa(maxModelNameLength) + " characters)"
	}
	return ""
}

func validateTemperature(v *float64) string {
	if v == nil {
		return ""
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > 2 {
		return "temperature must be a number between 0 and 2"
	}
	return ""
}

func validateTopP(v *float64) string {
	if v == nil {
		return ""
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) {
		return "top_p must be a finite number"
	}
	return ""
}

func validateTopK(v *float64) string {
	if v == nil {
		return ""
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) {
		return "top_k must be a finite number"
	}
	return ""
}

func validateMaxTokens(v int) string {
	if v > maxTokensLimit {
		return "max_tokens too large (max " + strconv.Itoa(maxTokensLimit) + ")"
	}
	return ""
}
