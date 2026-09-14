package antigravity

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// durationPattern matches Google's Duration strings: "1.5s", "200ms", "1h16m0.667s".
var durationPattern = regexp.MustCompile(`([\d.]+)\s*(ms|s|m|h)`)

// ParseDurationMs parses a Google Duration string into milliseconds.
func ParseDurationMs(s string) (time.Duration, bool) {
	matches := durationPattern.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return 0, false
	}
	var total float64
	for _, m := range matches {
		value, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		switch m[2] {
		case "ms":
			total += value
		case "s":
			total += value * 1000
		case "m":
			total += value * 60 * 1000
		case "h":
			total += value * 60 * 60 * 1000
		}
	}
	return time.Duration(math.Round(total)) * time.Millisecond, true
}

func parseRetryAfterHeader(header string) (time.Duration, bool) {
	if header == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(header, 64); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

var retryTextPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)try again in (\d+)m\s*(\d+)s`),
	regexp.MustCompile(`(?i)(?:try again in|backoff for|wait)\s*(\d+)s`),
	regexp.MustCompile(`(?i)quota will reset in (\d+) second`),
	regexp.MustCompile(`(?i)retry after (\d+) second`),
	regexp.MustCompile(`(?i)\(wait (\d+)s\)`),
}

func parseRetryDelayFromText(text string) (time.Duration, bool) {
	for _, pattern := range retryTextPatterns {
		m := pattern.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		if len(m) >= 3 {
			minutes, err1 := strconv.Atoi(m[1])
			seconds, err2 := strconv.Atoi(m[2])
			if err1 == nil && err2 == nil {
				return time.Duration(minutes*60+seconds) * time.Second, true
			}
		}
		if len(m) >= 2 {
			if seconds, err := strconv.Atoi(m[1]); err == nil {
				return time.Duration(seconds) * time.Second, true
			}
		}
	}
	return 0, false
}

// ParseRetryDelay extracts how long upstream wants us to wait, checking the
// Retry-After header, then structured RetryInfo/quotaResetDelay in the JSON
// body, then free-text patterns.
func ParseRetryDelay(errorText, retryAfterHeader string) (time.Duration, bool) {
	if d, ok := parseRetryAfterHeader(retryAfterHeader); ok {
		return d, true
	}

	var root map[string]any
	if err := json.Unmarshal([]byte(errorText), &root); err == nil {
		if errNode, ok := root["error"].(map[string]any); ok {
			if details, ok := errNode["details"].([]any); ok {
				// RetryInfo.retryDelay
				for _, detail := range details {
					node, ok := detail.(map[string]any)
					if !ok {
						continue
					}
					if typeStr, ok := node["@type"].(string); ok &&
						strings.Contains(typeStr, "RetryInfo") {
						if delay, ok := node["retryDelay"].(string); ok {
							if d, ok := ParseDurationMs(delay); ok {
								return d, true
							}
						}
					}
				}
				// metadata.quotaResetDelay
				for _, detail := range details {
					node, ok := detail.(map[string]any)
					if !ok {
						continue
					}
					metadata, ok := node["metadata"].(map[string]any)
					if !ok {
						continue
					}
					if delay, ok := metadata["quotaResetDelay"].(string); ok {
						if d, ok := ParseDurationMs(delay); ok {
							return d, true
						}
					}
				}
			}
			if retryAfter, ok := errNode["retry_after"].(float64); ok {
				return time.Duration(retryAfter * float64(time.Second)), true
			}
		}
	}

	return parseRetryDelayFromText(errorText)
}

// RetryStrategyKind names a backoff shape.
type RetryStrategyKind int

const (
	RetryNone RetryStrategyKind = iota
	RetryFixed
	RetryLinear
	RetryExponential
)

// RetryStrategy describes how to back off after a given status.
type RetryStrategy struct {
	Kind  RetryStrategyKind
	Base  time.Duration
	Max   time.Duration
	Delay time.Duration
}

// DetermineRetryStrategy picks a backoff shape from the upstream status/body.
func DetermineRetryStrategy(statusCode int, errorText, retryAfterHeader string) RetryStrategy {
	switch statusCode {
	case 429:
		if delay, ok := ParseRetryDelay(errorText, retryAfterHeader); ok {
			// Add headroom, floor at 2s so we never hot-loop, cap at 30s.
			actual := delay + 500*time.Millisecond
			if actual < 2*time.Second {
				actual = 2 * time.Second
			}
			if actual > 30*time.Second {
				actual = 30 * time.Second
			}
			return RetryStrategy{Kind: RetryFixed, Delay: actual}
		}
		lower := strings.ToLower(errorText)
		switch {
		case strings.Contains(lower, "model_capacity"), strings.Contains(lower, "capacity"):
			// Server-side GPU shortage: short and temporary.
			return RetryStrategy{Kind: RetryFixed, Delay: 15 * time.Second}
		case strings.Contains(lower, "per minute"),
			strings.Contains(lower, "rate limit"),
			strings.Contains(lower, "too many requests"):
			return RetryStrategy{Kind: RetryLinear, Base: 2 * time.Second}
		case strings.Contains(lower, "resource_exhausted"), strings.Contains(lower, "quota"):
			return RetryStrategy{Kind: RetryExponential, Base: 5 * time.Second, Max: 30 * time.Second}
		}
		return RetryStrategy{Kind: RetryLinear, Base: 2 * time.Second}

	case 503, 529:
		return RetryStrategy{Kind: RetryExponential, Base: time.Second, Max: 8 * time.Second}
	case 500:
		return RetryStrategy{Kind: RetryLinear, Base: 500 * time.Millisecond}
	case 401, 403:
		// Fast retry: the caller refreshes the token in between.
		return RetryStrategy{Kind: RetryFixed, Delay: 100 * time.Millisecond}
	}
	return RetryStrategy{Kind: RetryNone}
}

// CalculateRetryDelay resolves a strategy to a concrete wait for one attempt.
func CalculateRetryDelay(s RetryStrategy, attempt int) (time.Duration, bool) {
	switch s.Kind {
	case RetryFixed:
		return s.Delay, true
	case RetryLinear:
		return s.Base * time.Duration(attempt+1), true
	case RetryExponential:
		d := s.Base * time.Duration(1<<uint(attempt))
		if d > s.Max {
			d = s.Max
		}
		return d, true
	}
	return 0, false
}
