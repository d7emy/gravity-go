package antigravity

import (
	"testing"
	"time"
)

func TestParseDurationMs(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
		ok    bool
	}{
		{"1.5s", 1500 * time.Millisecond, true},
		{"200ms", 200 * time.Millisecond, true},
		{"42s", 42 * time.Second, true},
		{"2m30s", 2*time.Minute + 30*time.Second, true},
		{"1h", time.Hour, true},
		{"1h16m0.667s", time.Hour + 16*time.Minute + 667*time.Millisecond, true},
		{"nonsense", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, ok := ParseDurationMs(tc.input)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("ParseDurationMs(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// Retry-After is the server telling us exactly how long to wait, so it wins over
// anything in the body.
func TestParseRetryDelayPrefersHeader(t *testing.T) {
	body := `{"error":{"details":[{"@type":"RetryInfo","retryDelay":"60s"}]}}`

	got, ok := ParseRetryDelay(body, "5")
	if !ok {
		t.Fatal("ParseRetryDelay() returned no delay")
	}
	if got != 5*time.Second {
		t.Errorf("delay = %v, want the header's 5s to win over the body's 60s", got)
	}
}

func TestParseRetryDelayFromRetryInfo(t *testing.T) {
	body := `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"42s"}]}}`

	got, ok := ParseRetryDelay(body, "")
	if !ok {
		t.Fatal("ParseRetryDelay() returned no delay")
	}
	if got != 42*time.Second {
		t.Errorf("delay = %v, want 42s", got)
	}
}

func TestParseRetryDelayFromQuotaResetDelay(t *testing.T) {
	body := `{"error":{"details":[{"metadata":{"quotaResetDelay":"2m30s"}}]}}`

	got, ok := ParseRetryDelay(body, "")
	if !ok {
		t.Fatal("ParseRetryDelay() returned no delay")
	}
	if got != 2*time.Minute+30*time.Second {
		t.Errorf("delay = %v, want 2m30s", got)
	}
}

func TestParseRetryDelayFromFreeText(t *testing.T) {
	cases := []struct {
		body string
		want time.Duration
	}{
		{"please try again in 30s", 30 * time.Second},
		{"try again in 1m 15s", 75 * time.Second},
		{"retry after 12 seconds", 12 * time.Second},
		{"quota will reset in 90 seconds", 90 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			got, ok := ParseRetryDelay(tc.body, "")
			if !ok {
				t.Fatalf("ParseRetryDelay(%q) returned no delay", tc.body)
			}
			if got != tc.want {
				t.Errorf("delay = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseRetryDelayReturnsFalseWhenAbsent(t *testing.T) {
	if _, ok := ParseRetryDelay("something went wrong", ""); ok {
		t.Error("ParseRetryDelay() found a delay where there is none")
	}
}

// A stated delay is honoured but floored at 2s so we never hot-loop, and capped
// at 30s so a huge quota reset does not stall the request forever.
func TestRetryStrategy429RespectsStatedDelayWithinBounds(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"tiny delay is floored", `{"error":{"details":[{"metadata":{"quotaResetDelay":"1s"}}]}}`, 2 * time.Second},
		{"normal delay passes through", `{"error":{"details":[{"metadata":{"quotaResetDelay":"10s"}}]}}`, 10*time.Second + 500*time.Millisecond},
		{"huge delay is capped", `{"error":{"details":[{"metadata":{"quotaResetDelay":"1h"}}]}}`, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			strategy := DetermineRetryStrategy(429, tc.body, "")
			if strategy.Kind != RetryFixed {
				t.Fatalf("kind = %v, want RetryFixed", strategy.Kind)
			}
			if strategy.Delay != tc.want {
				t.Errorf("delay = %v, want %v", strategy.Delay, tc.want)
			}
		})
	}
}

func TestRetryStrategyByStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   RetryStrategyKind
	}{
		{"capacity 429 is a short fixed wait", 429, "MODEL_CAPACITY_EXHAUSTED", RetryFixed},
		{"rate limit 429 backs off linearly", 429, "rate limit exceeded", RetryLinear},
		{"quota 429 backs off exponentially", 429, "RESOURCE_EXHAUSTED quota", RetryExponential},
		{"503 backs off exponentially", 503, "", RetryExponential},
		{"529 backs off exponentially", 529, "", RetryExponential},
		{"500 backs off linearly", 500, "", RetryLinear},
		{"401 retries fast after a refresh", 401, "", RetryFixed},
		{"404 is not retried", 404, "", RetryNone},
		{"400 is not retried", 400, "", RetryNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetermineRetryStrategy(tc.status, tc.body, "").Kind; got != tc.want {
				t.Errorf("kind = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCalculateRetryDelay(t *testing.T) {
	t.Run("linear grows with the attempt", func(t *testing.T) {
		strategy := RetryStrategy{Kind: RetryLinear, Base: 2 * time.Second}
		for attempt, want := range []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second} {
			got, ok := CalculateRetryDelay(strategy, attempt)
			if !ok {
				t.Fatalf("attempt %d returned no delay", attempt)
			}
			if got != want {
				t.Errorf("attempt %d delay = %v, want %v", attempt, got, want)
			}
		}
	})

	t.Run("exponential doubles then caps", func(t *testing.T) {
		strategy := RetryStrategy{Kind: RetryExponential, Base: time.Second, Max: 8 * time.Second}
		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
		for attempt, expected := range want {
			got, ok := CalculateRetryDelay(strategy, attempt)
			if !ok {
				t.Fatalf("attempt %d returned no delay", attempt)
			}
			if got != expected {
				t.Errorf("attempt %d delay = %v, want %v", attempt, got, expected)
			}
		}
	})

	t.Run("no_retry yields nothing", func(t *testing.T) {
		if _, ok := CalculateRetryDelay(RetryStrategy{Kind: RetryNone}, 0); ok {
			t.Error("RetryNone should not produce a delay")
		}
	})
}

// 429 must never trigger an endpoint switch: it is account-specific, not
// endpoint-specific.
func TestShouldTryNextEndpoint(t *testing.T) {
	cases := map[int]bool{
		403: true, 404: true, 408: true, 500: true, 503: true,
		429: false, 400: false, 401: false, 200: false,
	}
	for status, want := range cases {
		if got := shouldTryNextEndpoint(status); got != want {
			t.Errorf("shouldTryNextEndpoint(%d) = %v, want %v", status, got, want)
		}
	}
}

// When every endpoint fails, the most actionable error should be the one
// surfaced to the client.
func TestRetryablePriorityOrdering(t *testing.T) {
	if retryablePriority(529) <= retryablePriority(500) {
		t.Error("529 should outrank a generic 500")
	}
	if retryablePriority(500) <= retryablePriority(429) {
		t.Error("500 should outrank 429")
	}
	if retryablePriority(429) <= retryablePriority(404) {
		t.Error("429 should outrank 404")
	}
	if retryablePriority(200) != 0 {
		t.Error("a success status should have no retry priority")
	}
}

// quota_exhausted means rotate accounts; everything else means wait on this one.
func TestParseRateLimitReason(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   RateLimitReason
	}{
		{"explicit quota", 429, `{"error":{"details":[{"reason":"QUOTA_EXHAUSTED"}]}}`, reasonQuotaExhausted},
		{"explicit rate limit", 429, `{"error":{"details":[{"reason":"RATE_LIMIT_EXCEEDED"}]}}`, reasonRateLimit},
		{"explicit capacity", 429, `{"error":{"details":[{"reason":"MODEL_CAPACITY_EXHAUSTED"}]}}`, reasonModelCapacity},
		{"per minute wording", 429, `{"error":{"message":"too many requests per minute"}}`, reasonRateLimit},
		{
			"bare RESOURCE_EXHAUSTED reads as throttling",
			429,
			`{"error":{"status":"RESOURCE_EXHAUSTED"}}`,
			reasonRateLimit,
		},
		{"exhausted without quota reads as throttling", 429, "resource exhausted", reasonRateLimit},
		{"5xx is a server error", 500, "", reasonServerError},
		{"other statuses are unknown", 418, "", reasonUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRateLimitReason(tc.status, tc.body); got != tc.want {
				t.Errorf("parseRateLimitReason() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Repeated quota exhaustion escalates the cooldown, so a spent account is not
// hammered.
func TestDefaultRateLimitDurationEscalates(t *testing.T) {
	want := []time.Duration{time.Minute, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	for failures, expected := range want {
		got := defaultRateLimitDuration(reasonQuotaExhausted, failures)
		if got != expected {
			t.Errorf("failures=%d duration = %v, want %v", failures, got, expected)
		}
	}

	// Transient reasons stay short regardless of the streak.
	if got := defaultRateLimitDuration(reasonRateLimit, 10); got != 30*time.Second {
		t.Errorf("rate limit duration = %v, want a flat 30s", got)
	}
	if got := defaultRateLimitDuration(reasonModelCapacity, 10); got != 15*time.Second {
		t.Errorf("capacity duration = %v, want a flat 15s", got)
	}
}
