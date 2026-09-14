package server

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Global spacing between outbound requests.
//
// The protection that actually matters is per-account (see the account manager's
// AcquireLock), because Google rate-limits per credential. An earlier build held
// a full second here for every request regardless of account, which taxed an
// agent loop by a second per turn — 13+ seconds on a 13-turn run. The global
// floor can therefore be much lower.
//
// Raise GRAVITY_MIN_REQUEST_INTERVAL_MS if 429s ever climb.
const defaultGlobalInterval = 250 * time.Millisecond

type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	lastCall time.Time
}

func resolveGlobalInterval() time.Duration {
	for _, key := range []string{
		"GRAVITY_MIN_REQUEST_INTERVAL_MS", "ANTI_API_MIN_REQUEST_INTERVAL_MS",
	} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		if ms, err := strconv.Atoi(raw); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return defaultGlobalInterval
}

var globalLimiter = &rateLimiter{interval: resolveGlobalInterval()}

// wait blocks until the minimum spacing since the previous call has elapsed.
func (rl *rateLimiter) wait(ctx context.Context) {
	if rl.interval <= 0 {
		return
	}

	rl.mu.Lock()
	now := time.Now()
	var sleep time.Duration
	if !rl.lastCall.IsZero() {
		if elapsed := now.Sub(rl.lastCall); elapsed < rl.interval {
			sleep = rl.interval - elapsed
		}
	}
	// Reserve this request's slot before unlocking so concurrent callers stack
	// rather than all waking at the same instant.
	rl.lastCall = now.Add(sleep)
	rl.mu.Unlock()

	if sleep <= 0 {
		return
	}
	timer := time.NewTimer(sleep)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
