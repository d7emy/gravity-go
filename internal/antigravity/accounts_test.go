package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"gravity-go/internal/authstore"
)

// newTestManager builds an isolated manager with pre-seeded accounts, bypassing
// disk and network so rotation logic can be tested on its own.
func newTestManager(t *testing.T, ids ...string) *Manager {
	t.Helper()
	t.Setenv("GRAVITY_DATA_DIR", t.TempDir())
	// Remove the inter-call spacing so tests are not paced by a real 1s sleep.
	t.Setenv("GRAVITY_ACCOUNT_INTERVAL_MS", "0")

	m := &Manager{
		accounts:   map[string]*Account{},
		gates:      map[string]chan struct{}{},
		inFlight:   map[string]int{},
		lastCall:   map[string]int64{},
		refreshing: map[string]chan struct{}{},
		loaded:     true,
	}
	for _, id := range ids {
		m.accounts[id] = &Account{
			ID:    id,
			Email: id + "@example.com",
			// A non-empty project id keeps NextAvailable off the network.
			ProjectID:   "project-" + id,
			AccessToken: "token-" + id,
		}
		m.queue = append(m.queue, id)
	}
	return m
}

// Sticky by default: one account is drained before rotating, which keeps request
// patterns per credential predictable.
func TestNextAvailableIsSticky(t *testing.T) {
	m := newTestManager(t, "a", "b", "c")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		got := m.NextAvailable(ctx, false)
		if got == nil {
			t.Fatal("NextAvailable() returned nil")
		}
		if got.AccountID != "a" {
			t.Errorf("call %d picked %q, want the queue head 'a'", i, got.AccountID)
		}
	}
}

func TestForceRotateSkipsQueueHead(t *testing.T) {
	m := newTestManager(t, "a", "b", "c")

	got := m.NextAvailable(context.Background(), true)
	if got == nil {
		t.Fatal("NextAvailable() returned nil")
	}
	if got.AccountID == "a" {
		t.Error("forceRotate should not return the queue head")
	}
}

func TestRateLimitedAccountIsSkipped(t *testing.T) {
	m := newTestManager(t, "a", "b")
	m.MarkRateLimited("a", time.Minute)

	got := m.NextAvailable(context.Background(), false)
	if got == nil {
		t.Fatal("NextAvailable() returned nil")
	}
	if got.AccountID != "b" {
		t.Errorf("picked %q, want 'b' since 'a' is on cooldown", got.AccountID)
	}
}

func TestMarkSuccessClearsCooldown(t *testing.T) {
	m := newTestManager(t, "a")
	m.MarkRateLimited("a", time.Minute)

	if !m.IsRateLimited("a") {
		t.Fatal("account should be rate limited")
	}
	m.MarkSuccess("a")
	if m.IsRateLimited("a") {
		t.Error("MarkSuccess should clear the cooldown")
	}
}

// A failing account goes to the back so the next request does not pick it again.
func TestMoveToEndOfQueue(t *testing.T) {
	m := newTestManager(t, "a", "b", "c")
	m.MoveToEndOfQueue("a")

	m.mu.Lock()
	last := m.queue[len(m.queue)-1]
	m.mu.Unlock()

	if last != "a" {
		t.Errorf("queue tail = %q, want 'a'", last)
	}
	if got := m.NextAvailable(context.Background(), false); got == nil || got.AccountID != "b" {
		t.Errorf("next pick = %v, want 'b' now that 'a' is demoted", got)
	}
}

// When every account is on a trivially short cooldown, that is a scheduling race
// rather than a real wall: let the request through.
func TestAllShortCooldownsAreCleared(t *testing.T) {
	m := newTestManager(t, "a", "b")
	m.MarkRateLimited("a", 500*time.Millisecond)
	m.MarkRateLimited("b", 500*time.Millisecond)

	if got := m.NextAvailable(context.Background(), false); got == nil {
		t.Error("NextAvailable() = nil, want a pick when all cooldowns are trivially short")
	}
}

// A genuinely long cooldown across every account must report exhaustion rather
// than pretending an account is usable.
func TestAllLongCooldownsReturnNil(t *testing.T) {
	m := newTestManager(t, "a", "b")
	m.MarkRateLimited("a", time.Hour)
	m.MarkRateLimited("b", time.Hour)

	if got := m.NextAvailable(context.Background(), false); got != nil {
		t.Errorf("NextAvailable() = %v, want nil when everything is on a long cooldown", got)
	}
}

// HoldRateLimit reserves an account during a retry wait so a concurrent request
// cannot pick it and immediately eat another 429.
func TestHoldRateLimitReservesThenRestores(t *testing.T) {
	m := newTestManager(t, "a")

	release := m.HoldRateLimit("a", time.Minute)
	if release == nil {
		t.Fatal("HoldRateLimit() returned no release func")
	}
	if !m.IsRateLimited("a") {
		t.Error("account should be reserved during the hold")
	}

	release()
	if m.IsRateLimited("a") {
		t.Error("release should have lifted the reservation")
	}
}

// A real cooldown written during the hold must survive the release.
func TestHoldRateLimitDoesNotClobberRealCooldown(t *testing.T) {
	m := newTestManager(t, "a")

	release := m.HoldRateLimit("a", time.Second)
	if release == nil {
		t.Fatal("HoldRateLimit() returned no release func")
	}

	m.MarkRateLimited("a", time.Hour)
	release()

	if !m.IsRateLimited("a") {
		t.Error("the real cooldown was clobbered by the hold's release")
	}
}

func TestHoldRateLimitYieldsToLongerExistingCooldown(t *testing.T) {
	m := newTestManager(t, "a")
	m.MarkRateLimited("a", time.Hour)

	if release := m.HoldRateLimit("a", time.Second); release != nil {
		t.Error("HoldRateLimit() should decline when a longer cooldown already exists")
	}
}

func TestMarkRateLimitedFromErrorClassifies(t *testing.T) {
	m := newTestManager(t, "a")

	outcome := m.MarkRateLimitedFromError("a", 429,
		`{"error":{"details":[{"reason":"QUOTA_EXHAUSTED"}]}}`, "")
	if outcome == nil {
		t.Fatal("MarkRateLimitedFromError() = nil")
	}
	if outcome.Reason != reasonQuotaExhausted {
		t.Errorf("reason = %v, want quota_exhausted", outcome.Reason)
	}
	if !m.IsRateLimited("a") {
		t.Error("account should be on cooldown after a 429")
	}
}

// A 429 with no stated delay backs off briefly rather than probing a quota
// endpoint, which would itself consume rate limit.
func TestMarkRateLimitedFromErrorUsesShortDefaultFor429(t *testing.T) {
	m := newTestManager(t, "a")

	outcome := m.MarkRateLimitedFromError("a", 429, "slow down", "")
	if outcome == nil {
		t.Fatal("MarkRateLimitedFromError() = nil")
	}
	if outcome.Duration != 10*time.Second {
		t.Errorf("duration = %v, want a 10s default", outcome.Duration)
	}
}

func TestMarkRateLimitedFromErrorHonoursStatedDelay(t *testing.T) {
	m := newTestManager(t, "a")

	outcome := m.MarkRateLimitedFromError("a", 429,
		`{"error":{"details":[{"metadata":{"quotaResetDelay":"45s"}}]}}`, "")
	if outcome == nil {
		t.Fatal("MarkRateLimitedFromError() = nil")
	}
	if outcome.Duration < 45*time.Second {
		t.Errorf("duration = %v, want at least the stated 45s", outcome.Duration)
	}
}

func TestAddAndRemoveAccount(t *testing.T) {
	m := newTestManager(t)

	m.Add(Account{ID: "new", Email: "new@example.com", AccessToken: "t", ProjectID: "p"})
	if !m.Has("new") {
		t.Fatal("account was not added")
	}
	if got := m.Count(); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}

	if !m.Remove("new") {
		t.Error("Remove() = false, want true")
	}
	if m.Has("new") {
		t.Error("account survived removal")
	}
}

func TestRemoveAccountByEmail(t *testing.T) {
	m := newTestManager(t)
	m.Add(Account{ID: "id-1", Email: "person@example.com", AccessToken: "t", ProjectID: "p"})

	if !m.Remove("person@example.com") {
		t.Error("Remove() by email = false, want true")
	}
	if m.Has("id-1") {
		t.Error("account survived removal by email")
	}
}

func TestRemoveUnknownAccountReportsFalse(t *testing.T) {
	m := newTestManager(t, "a")
	if m.Remove("nope") {
		t.Error("Remove() = true for an unknown account")
	}
}

// Default concurrency of 1 fully serializes one account, which is what protects
// against Google's per-credential rate limiting.
func TestAcquireLockSerializesOneAccount(t *testing.T) {
	m := newTestManager(t, "a")
	ctx := context.Background()

	var mu sync.Mutex
	concurrent, peak := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := m.AcquireLock(ctx, "a")
			defer release()

			mu.Lock()
			concurrent++
			if concurrent > peak {
				peak = concurrent
			}
			mu.Unlock()

			time.Sleep(2 * time.Millisecond)

			mu.Lock()
			concurrent--
			mu.Unlock()
		}()
	}
	wg.Wait()

	if peak > 1 {
		t.Errorf("peak concurrency = %d, want 1 with the default gate", peak)
	}
}

func TestAcquireLockRespectsConfiguredConcurrency(t *testing.T) {
	t.Setenv("GRAVITY_ACCOUNT_CONCURRENCY", "3")
	m := newTestManager(t, "a")
	ctx := context.Background()

	var mu sync.Mutex
	concurrent, peak := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := m.AcquireLock(ctx, "a")
			defer release()

			mu.Lock()
			concurrent++
			if concurrent > peak {
				peak = concurrent
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			concurrent--
			mu.Unlock()
		}()
	}
	wg.Wait()

	if peak > 3 {
		t.Errorf("peak concurrency = %d, want at most the configured 3", peak)
	}
}

// Releasing twice must not corrupt the gate.
func TestAcquireLockReleaseIsIdempotent(t *testing.T) {
	m := newTestManager(t, "a")
	release := m.AcquireLock(context.Background(), "a")

	release()
	release()

	done := make(chan struct{})
	go func() {
		r := m.AcquireLock(context.Background(), "a")
		r()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gate wedged after a double release")
	}
}

// The manager is reached concurrently from every in-flight request, so its state
// must be race-free. Run with -race to make this meaningful.
func TestManagerIsRaceFreeUnderConcurrentUse(t *testing.T) {
	m := newTestManager(t, "a", "b", "c")
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("%c", 'a'+i%3)

			switch i % 6 {
			case 0:
				m.NextAvailable(ctx, false)
			case 1:
				m.NextAvailable(ctx, true)
			case 2:
				m.MarkRateLimited(id, 10*time.Millisecond)
			case 3:
				m.MarkSuccess(id)
			case 4:
				release := m.AcquireLock(ctx, id)
				release()
			case 5:
				m.MoveToEndOfQueue(id)
			}
		}(i)
	}
	wg.Wait()
}

func TestEmailForKnownAndUnknown(t *testing.T) {
	m := newTestManager(t, "a")

	if got := m.EmailFor("a"); got != "a@example.com" {
		t.Errorf("EmailFor(a) = %q", got)
	}
	if got := m.EmailFor("missing"); got != "" {
		t.Errorf("EmailFor(missing) = %q, want empty", got)
	}
}

func TestClearAllRateLimits(t *testing.T) {
	m := newTestManager(t, "a", "b")
	m.MarkRateLimited("a", time.Hour)
	m.MarkRateLimited("b", time.Hour)

	m.ClearAllRateLimits()

	if m.IsRateLimited("a") || m.IsRateLimited("b") {
		t.Error("ClearAllRateLimits() left a cooldown in place")
	}
}

// writeAccountsFile lays down an accounts.json in the given order and points the
// manager at a fresh data dir, so loading exercises the real persisted path.
func writeAccountsFile(t *testing.T, emails ...string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GRAVITY_DATA_DIR", dir)

	// Loading accounts syncs them into the shared auth store cache, which does
	// not track which data directory it was filled from. Clear it on both sides
	// so these accounts cannot leak into other tests in this package.
	authstore.ResetCacheForTest()
	t.Cleanup(authstore.ResetCacheForTest)

	entries := make([]map[string]any, 0, len(emails))
	for _, e := range emails {
		entries = append(entries, map[string]any{
			"id": e, "email": e,
			"accessToken": "token-" + e,
			"projectId":   "project-" + e,
			// Far-future expiry keeps the loader off the network.
			"expiresAt": time.Now().Add(24 * time.Hour).UnixMilli(),
		})
	}
	data, err := json.Marshal(map[string]any{"accounts": entries})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), data, 0o600); err != nil {
		t.Fatalf("write accounts.json: %v", err)
	}
}

// Regression: the rotation queue was seeded by ranging the accounts map, and Go
// randomises map iteration. Which account drained first therefore changed from
// one run to the next. The stored file order has to decide it.
func TestQueueOrderFollowsTheStoredFileNotMapOrder(t *testing.T) {
	const first, second = "gg.star.sa@gmail.com", "d7omcracking@gmail.com"

	// Repeat: a single run could match the intended order by chance.
	for i := 0; i < 20; i++ {
		writeAccountsFile(t, first, second)

		m := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
			refreshing: map[string]chan struct{}{}}
		m.mu.Lock()
		m.loadLocked()
		order := m.orderedIDsLocked()
		m.mu.Unlock()

		if len(order) != 2 {
			t.Fatalf("run %d: got %d accounts, want 2", i, len(order))
		}
		if order[0] != first || order[1] != second {
			t.Fatalf("run %d: order = %v, want [%s %s] — file order must win",
				i, order, first, second)
		}
	}
}

// The behaviour asked for end to end: one account is used exclusively until it
// is exhausted, and only then does traffic move to the next one.
func TestOneAccountIsDrainedBeforeSwitching(t *testing.T) {
	const primary, backup = "gg.star.sa@gmail.com", "d7omcracking@gmail.com"
	writeAccountsFile(t, primary, backup)

	m := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
		refreshing: map[string]chan struct{}{}}
	ctx := context.Background()

	// Every request lands on the primary while it is healthy.
	for i := 0; i < 10; i++ {
		got := m.NextAvailable(ctx, false)
		if got == nil {
			t.Fatalf("request %d: no account available", i)
		}
		if got.AccountID != primary {
			t.Fatalf("request %d went to %q, want %q — traffic must not spread "+
				"across accounts while the first still has quota",
				i, got.AccountID, primary)
		}
	}

	// The primary hits its quota.
	m.MarkRateLimited(primary, time.Hour)

	// Now, and only now, traffic moves to the backup — and stays there.
	for i := 0; i < 10; i++ {
		got := m.NextAvailable(ctx, false)
		if got == nil {
			t.Fatalf("after exhaustion, request %d: no account available", i)
		}
		if got.AccountID != backup {
			t.Fatalf("after exhaustion, request %d went to %q, want %q",
				i, got.AccountID, backup)
		}
	}

	// When the cooldown expires the primary is eligible again and, being the
	// queue head, reclaims the traffic.
	m.MarkSuccess(primary)
	if got := m.NextAvailable(ctx, false); got == nil || got.AccountID != primary {
		t.Errorf("after recovery got %v, want %q back at the head", got, primary)
	}
}

// Marking a cooldown must work on a manager that has not served a request yet.
// It used to miss the account entirely and drop the signal on the floor.
func TestMarkRateLimitedLoadsAccountsFirst(t *testing.T) {
	const primary = "gg.star.sa@gmail.com"
	writeAccountsFile(t, primary, "d7omcracking@gmail.com")

	m := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
		refreshing: map[string]chan struct{}{}}

	// No prior call has loaded anything.
	m.MarkRateLimited(primary, time.Hour)

	if !m.IsRateLimited(primary) {
		t.Error("cooldown was dropped because the account store had not loaded yet")
	}
}

// With every account exhausted there is nothing to hand out, and the caller must
// be told rather than handed a rate-limited account.
func TestAllAccountsExhaustedReturnsNothing(t *testing.T) {
	const primary, backup = "gg.star.sa@gmail.com", "d7omcracking@gmail.com"
	writeAccountsFile(t, primary, backup)

	m := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
		refreshing: map[string]chan struct{}{}}

	m.MarkRateLimited(primary, time.Hour)
	m.MarkRateLimited(backup, time.Hour)

	if got := m.NextAvailable(context.Background(), false); got != nil {
		t.Errorf("got %q, want nil when every account is on cooldown", got.AccountID)
	}
}

// Regression: the "everything is on cooldown" fallback cleared every account's
// cooldown whenever the shortest one was trivial. A quota-exhausted account with
// a long backoff therefore had it wiped because some other account was free, and
// the next request went straight back to the account known to be empty.
func TestShortCooldownRecoveryKeepsLongCooldowns(t *testing.T) {
	m := newTestManager(t, "a", "b")

	// "a" is genuinely exhausted; "b" is momentarily busy.
	m.MarkRateLimited("a", time.Hour)
	m.MarkRateLimited("b", 500*time.Millisecond)

	// Asking for an account trips the short-wait recovery path.
	if got := m.NextAvailable(context.Background(), false); got == nil {
		t.Fatal("expected the trivially-delayed account to be handed out")
	} else if got.AccountID != "b" {
		t.Errorf("picked %q, want 'b' — the only one with a trivial wait", got.AccountID)
	}

	if m.IsRateLimited("b") {
		t.Error("'b' had a sub-second cooldown and should have been cleared")
	}
	if !m.IsRateLimited("a") {
		t.Error("'a' had an hour-long cooldown and must survive: clearing it sends " +
			"the next request back to an exhausted account")
	}
}

// Rotation must move off the named account even once the queue has been
// reordered by demoting it — the case that made failover pick the dead account.
func TestNextExcludingSurvivesQueueDemotion(t *testing.T) {
	m := newTestManager(t, "a", "b")

	// This is the exact order of operations the 429 path performs.
	m.MarkRateLimited("a", time.Hour)
	m.MoveToEndOfQueue("a") // queue is now [b, a]

	got := m.NextExcluding(context.Background(), "a")
	if got == nil {
		t.Fatal("NextExcluding returned nil; 'b' is healthy and should be available")
	}
	if got.AccountID != "b" {
		t.Errorf("picked %q, want 'b'", got.AccountID)
	}

	// And the exhausted account keeps its cooldown through all of it.
	if !m.IsRateLimited("a") {
		t.Error("'a' lost its cooldown during rotation")
	}
}

// With three accounts the old forceRotate flaw is unmistakable. Failing over
// demotes the dead account, so the queue becomes [b, c, a]; skipping "the head"
// then skips b — a perfectly healthy account — and jumps to c. Rotation must
// hand over to the next account in order, not leapfrog one.
func TestRotationHandsOverToTheNextAccountInOrder(t *testing.T) {
	m := newTestManager(t, "a", "b", "c")

	// Exactly what the quota-exhausted path does.
	m.MarkRateLimited("a", time.Hour)
	m.MoveToEndOfQueue("a")

	got := m.NextExcluding(context.Background(), "a")
	if got == nil {
		t.Fatal("NextExcluding returned nil with two healthy accounts available")
	}
	if got.AccountID != "b" {
		t.Errorf("rotated to %q, want 'b' — the next account in queue order; "+
			"skipping it leaves an unused account and drains 'c' out of turn",
			got.AccountID)
	}
}

// Regression: saving ranged over the accounts map, so accounts.json came back
// in a different order after every write. Since the stored order decides which
// account is drained first, the drain target silently changed from run to run —
// the deeper cause of the non-deterministic rotation.
func TestSaveRoundTripsTheOrderItLoaded(t *testing.T) {
	const first, second = "gg.star.sa@gmail.com", "d7omcracking@gmail.com"

	// Repeat: with two accounts a single save has even odds of looking correct.
	for i := 0; i < 20; i++ {
		writeAccountsFile(t, first, second)

		m := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
			refreshing: map[string]chan struct{}{}}
		m.mu.Lock()
		m.loadLocked()
		m.saveLocked()
		m.mu.Unlock()

		// Read the file back exactly as the next process start would.
		reloaded := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
			refreshing: map[string]chan struct{}{}}
		reloaded.mu.Lock()
		reloaded.loadLocked()
		order := reloaded.orderedIDsLocked()
		reloaded.mu.Unlock()

		if len(order) != 2 {
			t.Fatalf("run %d: got %d accounts, want 2", i, len(order))
		}
		if order[0] != first {
			t.Fatalf("run %d: after a save the drain target became %q, want %q — "+
				"saving must not reorder the file", i, order[0], first)
		}
	}
}

// Demotion is a deliberate reorder and must survive a save, or a failed account
// would climb back to the front on the next start.
func TestDemotionIsPersisted(t *testing.T) {
	const first, second = "gg.star.sa@gmail.com", "d7omcracking@gmail.com"
	writeAccountsFile(t, first, second)

	m := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
		refreshing: map[string]chan struct{}{}}
	m.mu.Lock()
	m.loadLocked()
	m.mu.Unlock()

	m.MoveToEndOfQueue(first)

	m.mu.Lock()
	m.saveLocked()
	m.mu.Unlock()

	reloaded := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
		refreshing: map[string]chan struct{}{}}
	reloaded.mu.Lock()
	reloaded.loadLocked()
	order := reloaded.orderedIDsLocked()
	reloaded.mu.Unlock()

	if order[0] != second {
		t.Errorf("after demoting %q the head is %q, want %q", first, order[0], second)
	}
}

// Regression: the per-account spacing stamped the clock before sleeping, so the
// recorded time was when a caller started waiting, not when its call went out.
// Every later caller then measured its gap from that too-early mark and spacing
// drifted shorter and shorter — the opposite of what a per-credential rate limit
// needs. The slot must be reserved including the wait.
func TestAccountSpacingDoesNotDriftAcrossCalls(t *testing.T) {
	m := newTestManager(t, "a")
	// After newTestManager: it zeroes the interval so other tests are not paced
	// by a real sleep, and would otherwise undo this.
	t.Setenv("GRAVITY_ACCOUNT_INTERVAL_MS", "120")
	t.Setenv("GRAVITY_ACCOUNT_CONCURRENCY", "1")
	ctx := context.Background()

	const calls = 4
	starts := make([]time.Time, 0, calls)
	for i := 0; i < calls; i++ {
		release := m.AcquireLock(ctx, "a")
		starts = append(starts, time.Now())
		// A call that returns immediately is the worst case: nothing but the
		// limiter separates one request from the next.
		release()
	}

	const want = 120 * time.Millisecond
	// Allow a little slack for timer granularity, but not enough to hide drift.
	const slack = 25 * time.Millisecond
	for i := 1; i < len(starts); i++ {
		gap := starts[i].Sub(starts[i-1])
		if gap < want-slack {
			t.Errorf("gap between call %d and %d was %v, want at least %v",
				i-1, i, gap.Round(time.Millisecond), want)
		}
	}
}

// Spacing must also hold when callers arrive at once rather than in sequence.
func TestAccountSpacingHoldsForConcurrentCallers(t *testing.T) {
	m := newTestManager(t, "a")
	t.Setenv("GRAVITY_ACCOUNT_INTERVAL_MS", "100")
	t.Setenv("GRAVITY_ACCOUNT_CONCURRENCY", "1")
	ctx := context.Background()

	const callers = 4
	var mu sync.Mutex
	var starts []time.Time

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := m.AcquireLock(ctx, "a")
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()

	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })

	total := starts[len(starts)-1].Sub(starts[0])
	// Three gaps of 100ms between four callers.
	minTotal := time.Duration(callers-1) * 100 * time.Millisecond
	if total < minTotal-40*time.Millisecond {
		t.Errorf("%d concurrent callers finished within %v, want at least %v — "+
			"they are not being spaced apart from each other",
			callers, total.Round(time.Millisecond), minTotal)
	}
}

// A disabled account must never be selected, and disabling must not disturb the
// order of the ones still in rotation.
func TestDisabledAccountIsSkipped(t *testing.T) {
	m := newTestManager(t, "a", "b", "c")
	ctx := context.Background()

	if got := m.NextAvailable(ctx, false); got == nil || got.AccountID != "a" {
		t.Fatalf("precondition: got %v, want 'a' at the head", got)
	}

	if !m.SetEnabled("a", false) {
		t.Fatal("SetEnabled reported the account missing")
	}
	if m.IsEnabled("a") {
		t.Error("IsEnabled still reports 'a' as enabled")
	}

	for i := 0; i < 5; i++ {
		got := m.NextAvailable(ctx, false)
		if got == nil {
			t.Fatalf("request %d: nothing available with two accounts still on", i)
		}
		if got.AccountID == "a" {
			t.Fatalf("request %d went to the disabled account", i)
		}
		if got.AccountID != "b" {
			t.Errorf("request %d went to %q, want 'b' — the next enabled account "+
				"in order", i, got.AccountID)
		}
	}
}

// Switching an account back on returns it to service immediately, at its old
// position, without needing to sign in again.
func TestEnablingRestoresAnAccount(t *testing.T) {
	m := newTestManager(t, "a", "b")
	ctx := context.Background()

	m.SetEnabled("a", false)
	if got := m.NextAvailable(ctx, false); got == nil || got.AccountID != "b" {
		t.Fatalf("while disabled, got %v, want 'b'", got)
	}

	m.SetEnabled("a", true)
	if !m.IsEnabled("a") {
		t.Error("account did not come back enabled")
	}
	if got := m.NextAvailable(ctx, false); got == nil || got.AccountID != "a" {
		t.Errorf("after re-enabling, got %v, want 'a' back at the head", got)
	}
}

// Re-enabling clears a stale cooldown: an account switched off while rate
// limited should be usable the moment it is switched back on, not sit out a
// backoff from before the pause.
func TestEnablingClearsStaleCooldown(t *testing.T) {
	m := newTestManager(t, "a", "b")

	m.MarkRateLimited("a", time.Hour)
	m.SetEnabled("a", false)
	m.SetEnabled("a", true)

	if m.IsRateLimited("a") {
		t.Error("account is still on cooldown after being switched back on")
	}
}

// With every account switched off there is nothing to serve with, and the
// caller must be told rather than handed a disabled account.
func TestAllAccountsDisabledYieldsNothing(t *testing.T) {
	m := newTestManager(t, "a", "b")

	m.SetEnabled("a", false)
	m.SetEnabled("b", false)

	if n := m.EnabledCount(); n != 0 {
		t.Errorf("EnabledCount = %d, want 0", n)
	}
	if got := m.NextAvailable(context.Background(), false); got != nil {
		t.Errorf("got %q, want nil when every account is switched off", got.AccountID)
	}
}

// The emergency "everything is rate limited" path must not resurrect a disabled
// account. Switching one off is an explicit instruction, not a hint.
func TestCooldownRecoveryNeverRevivesADisabledAccount(t *testing.T) {
	m := newTestManager(t, "a", "b")

	m.SetEnabled("a", false)
	// "b" is briefly limited, which is exactly what trips the recovery branch.
	m.MarkRateLimited("b", 500*time.Millisecond)

	got := m.NextAvailable(context.Background(), false)
	if got != nil && got.AccountID == "a" {
		t.Fatal("the disabled account was handed out by the cooldown recovery path")
	}
	if m.IsEnabled("a") {
		t.Error("the disabled account was re-enabled by the recovery path")
	}
}

// Pinning an account by id must not bypass the switch.
func TestByIDRefusesADisabledAccount(t *testing.T) {
	m := newTestManager(t, "a", "b")
	m.SetEnabled("a", false)

	if got := m.ByID(context.Background(), "a"); got != nil {
		t.Error("ByID returned a disabled account; \"off\" must mean off even for " +
			"a caller that asked for it by name")
	}
	if got := m.ByID(context.Background(), "b"); got == nil {
		t.Error("ByID refused an enabled account")
	}
}

// The switch has to survive a restart, or a paused account quietly comes back
// the next time the process starts.
func TestDisabledStatePersistsAcrossRestart(t *testing.T) {
	const first, second = "gg.star.sa@gmail.com", "d7omcracking@gmail.com"
	writeAccountsFile(t, first, second)

	m := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
		gates: map[string]chan struct{}{}, lastCall: map[string]int64{},
		refreshing: map[string]chan struct{}{}}
	m.mu.Lock()
	m.loadLocked()
	m.mu.Unlock()

	if !m.SetEnabled(first, false) {
		t.Fatal("SetEnabled reported the account missing")
	}

	// Reload exactly as a fresh process would.
	reloaded := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
		gates: map[string]chan struct{}{}, lastCall: map[string]int64{},
		refreshing: map[string]chan struct{}{}}

	if reloaded.IsEnabled(first) {
		t.Errorf("%s came back enabled after a restart", first)
	}
	if !reloaded.IsEnabled(second) {
		t.Errorf("%s should still be enabled", second)
	}
	if got := reloaded.NextAvailable(context.Background(), false); got == nil {
		t.Fatal("nothing available after restart")
	} else if got.AccountID != second {
		t.Errorf("after restart traffic went to %q, want %q", got.AccountID, second)
	}
}

// Upgrade safety: an accounts.json written before the switch existed has no
// "disabled" key at all. JSON decodes a missing bool as false, so the field is
// spelled "disabled" rather than "enabled" — the other way round, every existing
// account would load as switched off and the proxy would refuse to serve.
func TestAccountsFileWithoutTheFieldLoadsEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GRAVITY_DATA_DIR", dir)
	authstore.ResetCacheForTest()
	t.Cleanup(authstore.ResetCacheForTest)

	// Exactly the shape an older build wrote: no "disabled" key anywhere.
	legacy := `{"accounts":[
        {"id":"old@example.com","email":"old@example.com",
         "accessToken":"t","projectId":"p"}
    ]}`
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := &Manager{accounts: map[string]*Account{}, inFlight: map[string]int{},
		gates: map[string]chan struct{}{}, lastCall: map[string]int64{},
		refreshing: map[string]chan struct{}{}}

	if !m.IsEnabled("old@example.com") {
		t.Error("an account from a file predating the switch loaded as disabled; " +
			"upgrading would silently stop the proxy serving")
	}
	if n := m.EnabledCount(); n != 1 {
		t.Errorf("EnabledCount = %d, want 1", n)
	}
}
