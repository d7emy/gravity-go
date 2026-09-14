package antigravity

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The upstream agent must always look like the real IDE: header and body
// copies agree, and the shape is pinned so behaviour-gating keeps working.
func TestUserAgentShape(t *testing.T) {
	t.Setenv("GRAVITY_USER_AGENT", "")
	t.Setenv("ANTIGRAVITY_USER_AGENT", "")
	t.Setenv("GRAVITY_IDE_VERSION", "")
	t.Setenv("ANTIGRAVITY_IDE_VERSION", "")

	ua := UserAgent()
	matched, _ := regexp.MatchString(`^antigravity/\d+\.\d+\.\d+ [a-z0-9]+/[a-z0-9]+$`, ua)
	if !matched {
		t.Errorf("UserAgent() = %q, want antigravity/X.Y.Z os/arch", ua)
	}
	if !strings.HasPrefix(ua, "antigravity/"+defaultIDEVersion+" ") {
		t.Errorf("UserAgent() = %q, want pinned default version %s", ua, defaultIDEVersion)
	}
}

// GRAVITY_ aliases win over ANTIGRAVITY_ ones, matching the rest of the
// project's env convention.
func TestUserAgentEnvPrecedence(t *testing.T) {
	t.Setenv("GRAVITY_USER_AGENT", "custom-debug-agent/1.0")
	t.Setenv("ANTIGRAVITY_USER_AGENT", "other-agent/2.0")
	if got := UserAgent(); got != "custom-debug-agent/1.0" {
		t.Errorf("UserAgent() = %q, want GRAVITY_ override to win", got)
	}

	t.Setenv("GRAVITY_USER_AGENT", "")
	if got := UserAgent(); got != "other-agent/2.0" {
		t.Errorf("UserAgent() = %q, want ANTIGRAVITY_ fallback", got)
	}

	t.Setenv("ANTIGRAVITY_USER_AGENT", "")
	t.Setenv("GRAVITY_IDE_VERSION", "9.9.9")
	t.Setenv("ANTIGRAVITY_IDE_VERSION", "8.8.8")
	if got := IDEVersion(); got != "9.9.9" {
		t.Errorf("IDEVersion() = %q, want GRAVITY_ pin to win", got)
	}
}

func TestNormalizeUpstreamPlatform(t *testing.T) {
	if got := normalizeUpstreamPlatform("darwin"); got != "macos" {
		t.Errorf("darwin -> %q, want macos", got)
	}
	if got := normalizeUpstreamPlatform("Windows"); got != "windows" {
		t.Errorf("Windows -> %q, want windows", got)
	}
	if got := normalizeUpstreamPlatform("linux"); got != "linux" {
		t.Errorf("linux -> %q, want linux", got)
	}
}

// Every data-plane request carries the same header set: auth, JSON type, the
// single IDE agent, locale and the caller's Accept value.
func TestSetUpstreamHeaders(t *testing.T) {
	t.Setenv("GRAVITY_USER_AGENT", "")
	t.Setenv("ANTIGRAVITY_USER_AGENT", "")

	req, _ := http.NewRequest(http.MethodPost, "https://example.com", nil)
	setUpstreamHeaders(req, "tok123", "text/event-stream")

	if got := req.Header.Get("Authorization"); got != "Bearer tok123" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q", got)
	}
	if got := req.Header.Get("Accept-Language"); got != upstreamAcceptLanguage {
		t.Errorf("Accept-Language = %q, want %q", got, upstreamAcceptLanguage)
	}
	if got := req.Header.Get("User-Agent"); got != UserAgent() {
		t.Errorf("User-Agent = %q, want agreement with UserAgent()", got)
	}
}

// GRAVITY_JITTER=0 disables everything for deterministic tests.
func TestJitterDisabled(t *testing.T) {
	t.Setenv("GRAVITY_JITTER", "0")

	if got := retryHoldJitter(); got != 0 {
		t.Errorf("retryHoldJitter() = %v with jitter off, want 0", got)
	}
	if got := spacingJitter(time.Second); got != 0 {
		t.Errorf("spacingJitter(1s) = %v with jitter off, want 0", got)
	}
	if got := cooldownJitter(); got != 0 {
		t.Errorf("cooldownJitter() = %v with jitter off, want 0", got)
	}
	if got := QuotaFetchTimeout(); got != 3*time.Second {
		t.Errorf("QuotaFetchTimeout() = %v with jitter off, want 3s floor", got)
	}
	if got := KeepAliveDelay(15 * time.Second); got != 15*time.Second-15*time.Second/8 {
		t.Errorf("KeepAliveDelay(15s) = %v with jitter off, want lower bound", got)
	}
}

// Jitter bounds: spacing only extends, cooldown/hold stay small, quota window
// stays 3..5s, keep-alive stays within 1/8 of base.
func TestJitterBounds(t *testing.T) {
	t.Setenv("GRAVITY_JITTER", "1")

	for i := 0; i < 200; i++ {
		if got := spacingJitter(time.Second); got < 0 || got > 350*time.Millisecond {
			t.Fatalf("spacingJitter(1s) = %v, want 0..350ms", got)
		}
		if got := retryHoldJitter(); got < 0 || got > 300*time.Millisecond {
			t.Fatalf("retryHoldJitter() = %v, want 0..300ms", got)
		}
		if got := cooldownJitter(); got < 0 || got > time.Second {
			t.Fatalf("cooldownJitter() = %v, want 0..1s", got)
		}
		if got := QuotaFetchTimeout(); got < 3*time.Second || got > 5*time.Second {
			t.Fatalf("QuotaFetchTimeout() = %v, want 3..5s", got)
		}
		if got := KeepAliveDelay(15 * time.Second); got < 15*time.Second-15*time.Second/8 ||
			got > 15*time.Second+15*time.Second/8 {
			t.Fatalf("KeepAliveDelay(15s) = %v, want base ±1/8", got)
		}
	}
	// A zero base stays zero: tests that disable spacing must never be paced.
	if got := spacingJitter(0); got != 0 {
		t.Errorf("spacingJitter(0) = %v, want 0", got)
	}
}

// Shuffling preserves the host set; single-entry stubs are untouched.
func TestShuffledBaseURLs(t *testing.T) {
	t.Setenv("GRAVITY_JITTER", "1")

	original := baseURLs
	baseURLs = []string{"https://a.example", "https://b.example", "https://c.example"}
	defer func() { baseURLs = original }()

	seen := map[string]int{}
	for i := 0; i < 60; i++ {
		got := shuffledBaseURLs()
		if len(got) != 3 {
			t.Fatalf("shuffled len = %d, want 3", len(got))
		}
		sorted := append([]string(nil), got...)
		sort.Strings(sorted)
		if sorted[0] != "https://a.example" || sorted[2] != "https://c.example" {
			t.Fatalf("shuffled = %v, want same host set", got)
		}
		seen[got[0]]++
	}
	// With 60 shuffles of 3 hosts every host should lead at least once.
	// (Probability of a miss is astronomically small; failure means broken shuffle.)
	for h, n := range seen {
		if n == 0 {
			t.Errorf("host %s never led in 60 shuffles", h)
		}
	}

	baseURLs = []string{"https://only.example"}
	if got := shuffledBaseURLs(); len(got) != 1 || got[0] != "https://only.example" {
		t.Errorf("single-entry shuffle = %v, want untouched", got)
	}
}
