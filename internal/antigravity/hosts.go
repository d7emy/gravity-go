package antigravity

import (
	"context"
	"sync"
	"time"

	"gravity-go/internal/apperr"
	"gravity-go/internal/logx"
)

// hostRefusalTTL is how long a host that turned an account away is tried last for
// that account. It is short because it is only a hint: a host that has started
// serving the account again is rediscovered cheaply, by one extra request.
const hostRefusalTTL = 5 * time.Minute

// hostRefusals remembers, per account, which upstream hosts answered a plain 429
// while another host went on to serve the same request.
//
// Some accounts are turned away by one of the hosts for every model, with a 429
// that says nothing about the account's own limits. Hosts are tried in random order
// and a 429 used to be read as the account being rate limited, so each such answer
// cost a wait, a retry and sometimes a rest and a switch of account, with no limit
// anywhere. On such accounts 12 identical tiny requests averaged 4.5 s and one
// failed; with a fixed host order they averaged 2.6 s and none did.
type hostRefusals struct {
	mu    sync.Mutex
	until map[string]time.Time
	now   func() time.Time
}

var refusals = newHostRefusals()

func newHostRefusals() *hostRefusals {
	return &hostRefusals{until: map[string]time.Time{}, now: time.Now}
}

func refusalKey(accountID, host string) string { return accountID + "\x00" + host }

// mark records that host turned the account away, and drops entries that expired.
func (r *hostRefusals) mark(accountID, host string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for key, until := range r.until {
		if !until.After(now) {
			delete(r.until, key)
		}
	}
	r.until[refusalKey(accountID, host)] = now.Add(hostRefusalTTL)
}

// refused reports whether host is currently remembered as turning the account away.
func (r *hostRefusals) refused(accountID, host string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.until[refusalKey(accountID, host)]
	return ok && until.After(r.now())
}

// forget clears the memory. Tests use it to start from a known state.
func (r *hostRefusals) forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.until = map[string]time.Time{}
}

// orderedBaseURLs is shuffledBaseURLs with the hosts remembered as refusing this
// account moved to the end. The order stays random among the rest, and every host
// is still tried, the refusing one last.
func orderedBaseURLs(accountID string) []string {
	hosts := shuffledBaseURLs()
	if len(hosts) < 2 {
		return hosts
	}
	ordered := make([]string, 0, len(hosts))
	var refusing []string
	for _, host := range hosts {
		if refusals.refused(accountID, host) {
			refusing = append(refusing, host)
		} else {
			ordered = append(ordered, host)
		}
	}
	return append(ordered, refusing...)
}

// heldAnswer is a plain 429 set aside during a host sweep.
type heldAnswer struct{ body, retryAfter string }

// beginAttempt clears what the previous attempt held back.
func (s *attemptState) beginAttempt() {
	s.deferred = nil
	s.refusedHosts = s.refusedHosts[:0]
	s.lastHeld = false
}

// tryHost records which host is about to be called and how many are left after it.
func (s *attemptState) tryHost(host string, left int) {
	s.host, s.hostsLeft = host, left
}

// wasHeld reports whether the last classifyFailure call set its 429 aside or
// ignored it. Such an answer says nothing about what to do once every host has
// failed, so callers must not remember it as the last error.
func (s *attemptState) wasHeld() bool { return s.lastHeld }

// holdPlain429 decides whether a 429 is better judged later.
//
// A plain 429 may be this host turning the account away rather than the account
// being limited, so the request goes on to the other hosts first, and the 429 is
// judged as an account limit only if none of them serves it (flushDeferred). A host
// already remembered as refusing this account is ignored outright: its 429 carries
// no information, and must not hide a real 404 from the hosts that did answer.
//
// A spent quota is never held back: every host would say the same, so it is handled
// at once, as before.
func (s *attemptState) holdPlain429(body, retryAfter string) bool {
	if isQuotaExhaustedErrorText(body) {
		return false
	}
	if refusals.refused(s.accountID, s.host) {
		s.lastHeld = true
		return true
	}
	if s.hostsLeft == 0 {
		return false
	}
	if s.deferred == nil {
		s.deferred = &heldAnswer{body: body, retryAfter: retryAfter}
	}
	s.refusedHosts = append(s.refusedHosts, s.host)
	s.lastHeld = true
	return true
}

// served is called when a host accepted the request. Any host that answered a
// plain 429 earlier in the same attempt was refusing this account on its own, so
// it is remembered and tried last next time.
func (s *attemptState) served() {
	for _, host := range s.refusedHosts {
		refusals.mark(s.accountID, host)
		logx.Debug("[Antigravity] %s answered a plain 429 while another host served the request; trying it last for this account for %s",
			host, hostRefusalTTL)
	}
	s.refusedHosts = s.refusedHosts[:0]
	s.deferred = nil
}

// flushDeferred judges a held-back 429 once every host has been tried without
// serving the request: now it is the account's own limit, handled as it always
// was. It returns "" when nothing was held.
func (s *attemptState) flushDeferred(
	ctx context.Context, request map[string]any, allowRotation bool,
) (string, error) {
	held := s.deferred
	s.deferred = nil
	if held == nil {
		return "", nil
	}
	if s.accountID == "" {
		// The process-level token has no account to rest or rotate away from.
		return "fail", apperr.NewUpstream("antigravity", 429, held.body, held.retryAfter)
	}
	return s.classify429(ctx, held.body, held.retryAfter, request, allowRotation)
}
