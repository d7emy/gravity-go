package antigravity

import (
	"crypto/rand"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

// Upstream request shaping shared by chat, search, quota and OAuth.
//
// Wire semantics stay identical to what the backend expects (same hosts,
// paths, envelope keys, auth scheme, IDE-like agent). What differs from a
// naive port is transport behaviour: header set, endpoint order and timing
// all carry small per-process randomness so one client does not march in
// lock-step like every other clone. All jitter fails open (no jitter) and is
// disabled entirely with GRAVITY_JITTER=0 for deterministic tests.

// upstreamAcceptLanguage is sent on data-plane calls. It is benign — real IDE
// traffic carries locale headers — and it moves this client off the minimal
// four-header set without adding anything the backend could reject.
const upstreamAcceptLanguage = "en-US,en;q=0.9"

// setUpstreamAgent sets the single IDE-like User-Agent. Header and body copies
// must agree, so callers use UserAgent() for the body field and this helper
// for the header.
func setUpstreamAgent(req *http.Request) {
	req.Header.Set("User-Agent", UserAgent())
}

// setUpstreamLocale adds the benign locale header.
func setUpstreamLocale(req *http.Request) {
	req.Header.Set("Accept-Language", upstreamAcceptLanguage)
}

// setUpstreamHeaders applies the full data-plane header set: auth, content
// type, IDE agent, locale and the caller's Accept value.
//
// Note on header order: Go's Transport serializes headers in sorted key order,
// so insertion order is not a fingerprint surface here. Differentiation comes
// from the header *set* (Accept-Language) plus timing, not ordering tricks.
func setUpstreamHeaders(req *http.Request, accessToken, accept string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	setUpstreamAgent(req)
	setUpstreamLocale(req)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
}

// jitterEnabled reports whether transport jitter applies. Tests set
// GRAVITY_JITTER=0 for determinism.
func jitterEnabled() bool {
	v := strings.TrimSpace(os.Getenv("GRAVITY_JITTER"))
	return !(v == "0" || strings.EqualFold(v, "false"))
}

// jitterBelow returns a uniform random value in [0, n). It fails open to 0:
// jitter must never fail a request.
func jitterBelow(n int64) int64 {
	if n <= 0 || !jitterEnabled() {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return v.Int64()
}

// jitterRange returns a uniform random value in [min, max]. Bounds are clamped
// so a misconfiguration cannot produce a negative sleep.
func jitterRange(min, max time.Duration) time.Duration {
	if max <= min || !jitterEnabled() {
		return min
	}
	span := int64(max - min)
	return min + time.Duration(jitterBelow(span+1))
}

// retryHoldJitter spreads the transient-429 hold so concurrent processes do
// not retry in lock-step. Returns 0..300ms.
func retryHoldJitter() time.Duration {
	return time.Duration(jitterBelow(int64(300 * time.Millisecond)))
}

// retrySleepJitter spreads the in-place 429 sleep. Returns 0..300ms on top of
// the protocol-mandated delay+200ms.
func retrySleepJitter() time.Duration {
	return time.Duration(jitterBelow(int64(300 * time.Millisecond)))
}

// cooldownJitter spreads rate-limit cooldowns so demoted accounts do not all
// re-probe at the same instant. Returns 0..1s.
func cooldownJitter() time.Duration {
	return time.Duration(jitterBelow(int64(time.Second)))
}

// spacingJitter spreads the per-account minimum spacing. The base interval
// (default 1s) is a floor; jitter only ever adds, in [-0ms, +350ms] with a
// small chance of running early-free. Clamped at zero so a 0ms test interval
// stays exactly 0 and tests are never paced by jitter.
func spacingJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	return time.Duration(jitterBelow(int64(350 * time.Millisecond)))
}

// shuffledBaseURLs returns the upstream hosts in random order for one attempt.
// Single-entry test stubs are unaffected. Production keeps all three hosts;
// the sweep still covers every endpoint, just not in clone-identical order.
func shuffledBaseURLs() []string {
	out := make([]string, len(baseURLs))
	copy(out, baseURLs)
	if !jitterEnabled() || len(out) < 2 {
		return out
	}
	for i := len(out) - 1; i > 0; i-- {
		j := int(jitterBelow(int64(i + 1)))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// QuotaFetchTimeout bounds the dashboard quota refresh. Jittered 3..5s around
// the documented 4s so dashboard polls across processes do not synchronize.
func QuotaFetchTimeout() time.Duration {
	return jitterRange(3*time.Second, 5*time.Second)
}

// KeepAliveDelay returns the next SSE keep-alive delay around base. At the
// production 15s base this yields ~13..17s; at the 15ms test base it yields
// ~13..17ms so the silence test still sees multiple pings in 90ms.
func KeepAliveDelay(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	lo := base - base/8
	hi := base + base/8
	if lo < time.Millisecond {
		lo = time.Millisecond
	}
	if hi <= lo {
		return base
	}
	return jitterRange(lo, hi)
}
