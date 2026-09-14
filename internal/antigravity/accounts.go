package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gravity-go/internal/appstate"
	"gravity-go/internal/authstore"
	"gravity-go/internal/logx"
	"gravity-go/internal/paths"
)

// Account is one rotatable Google credential.
type Account struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	ProjectID    string `json:"projectId"`

	// Disabled takes an account out of rotation without deleting it. It is
	// deliberately "Disabled" and not "Enabled": accounts.json files written
	// before this field existed have no key for it, and JSON decodes a missing
	// bool as false. Spelled this way that means "not disabled", so upgrading
	// leaves every existing account working. Spelled the other way it would
	// switch all of them off.
	Disabled bool `json:"disabled,omitempty"`

	// Runtime-only rate limit state, never persisted.
	rateLimitedUntil    int64 `json:"-"`
	consecutiveFailures int   `json:"-"`
}

// Resolved is a credential ready to send a request with.
type Resolved struct {
	AccessToken string
	ProjectID   string
	Email       string
	AccountID   string
}

// RateLimitReason classifies why an account was throttled.
type RateLimitReason string

const (
	reasonQuotaExhausted RateLimitReason = "quota_exhausted"
	reasonRateLimit      RateLimitReason = "rate_limit_exceeded"
	reasonModelCapacity  RateLimitReason = "model_capacity_exhausted"
	reasonServerError    RateLimitReason = "server_error"
	reasonUnknown        RateLimitReason = "unknown"
)

const (
	defaultLockWaitTimeout = 45 * time.Second
	minRequestInterval     = time.Second
	tokenRefreshWindow     = 5 * time.Minute
)

// Manager owns the account pool and all rotation decisions.
type Manager struct {
	mu       sync.Mutex
	accounts map[string]*Account
	queue    []string
	loaded   bool

	// Per-account concurrency gates. Each account gets a buffered channel used
	// as a semaphore; capacity is accountConcurrency().
	gates    map[string]chan struct{}
	inFlight map[string]int
	lastCall map[string]int64

	// refreshing coalesces concurrent token refreshes per account.
	refreshing map[string]chan struct{}
}

// Accounts is the process-wide account manager.
var Accounts = &Manager{
	accounts:   map[string]*Account{},
	gates:      map[string]chan struct{}{},
	inFlight:   map[string]int{},
	lastCall:   map[string]int64{},
	refreshing: map[string]chan struct{}{},
}

func accountsFile() string { return paths.DataFile("accounts.json") }

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return fallback
}

// accountConcurrency is how many requests one account may have in flight.
//
// Default 1 fully serializes a single account, which is what protects against
// Google's per-credential rate limiting. Raising it lets an agent run parallel
// tool calls on one account: throughput goes up, 429 risk goes up with it. This
// stays deliberately opt-in.
func accountConcurrency() int {
	n := envInt("GRAVITY_ACCOUNT_CONCURRENCY", envInt("ANTI_API_ACCOUNT_CONCURRENCY", 1))
	if n < 1 {
		return 1
	}
	if n > 8 {
		return 8
	}
	return n
}

// accountInterval is the minimum spacing between two calls on one account.
func accountInterval() time.Duration {
	ms := envInt("GRAVITY_ACCOUNT_INTERVAL_MS", envInt("ANTI_API_ACCOUNT_INTERVAL_MS", -1))
	if ms < 0 {
		return minRequestInterval
	}
	return time.Duration(ms) * time.Millisecond
}

func lockWaitTimeout() time.Duration {
	ms := envInt("GRAVITY_ACCOUNT_LOCK_WAIT_TIMEOUT_MS",
		envInt("ANTI_API_ACCOUNT_LOCK_WAIT_TIMEOUT_MS", -1))
	if ms <= 0 {
		return defaultLockWaitTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

func nowMs() int64 { return time.Now().UnixMilli() }

// persistedAccount is the accounts.json entry shape (no runtime state).
type persistedAccount struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	ProjectID    string `json:"projectId"`
	Disabled     bool   `json:"disabled,omitempty"`
}

// Load reads accounts.json, preserving live rate-limit state.
func (m *Manager) Load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
}

func (m *Manager) loadLocked() {
	if data, err := os.ReadFile(accountsFile()); err == nil {
		var parsed struct {
			Accounts []persistedAccount `json:"accounts"`
		}
		if err := json.Unmarshal(data, &parsed); err == nil {
			for _, entry := range parsed.Accounts {
				if entry.ID == "" {
					continue
				}
				// Load runs on many requests; resetting cooldowns here would make
				// every 429 backoff evaporate, so carry runtime state forward.
				var limitedUntil int64
				var failures int
				if existing, ok := m.accounts[entry.ID]; ok {
					limitedUntil = existing.rateLimitedUntil
					failures = existing.consecutiveFailures
				}
				account := &Account{
					ID:                  entry.ID,
					Email:               entry.Email,
					AccessToken:         entry.AccessToken,
					RefreshToken:        entry.RefreshToken,
					ExpiresAt:           entry.ExpiresAt,
					ProjectID:           entry.ProjectID,
					Disabled:            entry.Disabled,
					rateLimitedUntil:    limitedUntil,
					consecutiveFailures: failures,
				}
				m.accounts[entry.ID] = account
				// Seed the rotation queue in stored order. Without this the queue
				// falls back to map iteration, which Go randomises -- so which
				// account drains first would change on every restart.
				if !containsString(m.queue, entry.ID) {
					m.queue = append(m.queue, entry.ID)
				}
				m.syncToAuthStoreLocked(account)
			}
		}
	}

	if len(m.accounts) == 0 {
		m.hydrateFromAuthStoreLocked("")
	}

	// Fall back to migrating the single process-level credential.
	if len(m.accounts) == 0 {
		auth := appstate.GetAuth()
		if auth.AccessToken != "" && auth.RefreshToken != "" {
			id := auth.UserEmail
			if id == "" {
				id = "default"
			}
			email := auth.UserEmail
			if email == "" {
				email = "unknown"
			}
			m.accounts[id] = &Account{
				ID:           id,
				Email:        email,
				AccessToken:  auth.AccessToken,
				RefreshToken: auth.RefreshToken,
				ExpiresAt:    auth.TokenExpiresAt,
				ProjectID:    auth.ProjectID,
			}
		}
	}

	m.loaded = true
}

func (m *Manager) ensureLoadedLocked() {
	if !m.loaded {
		m.loadLocked()
	}
}

func (m *Manager) hydrateFromAuthStoreLocked(accountID string) {
	var stored []authstore.Account
	if accountID != "" {
		if account, ok := authstore.Get(accountID); ok {
			stored = []authstore.Account{account}
		}
	} else {
		stored = authstore.List()
	}

	for _, entry := range stored {
		if _, exists := m.accounts[entry.ID]; exists {
			continue
		}
		email := entry.Email
		if email == "" {
			email = entry.Login
		}
		if email == "" {
			email = entry.ID
		}
		m.accounts[entry.ID] = &Account{
			ID:           entry.ID,
			Email:        email,
			AccessToken:  entry.AccessToken,
			RefreshToken: entry.RefreshToken,
			ExpiresAt:    entry.ExpiresAt,
			ProjectID:    entry.ProjectID,
		}
		if !containsString(m.queue, entry.ID) {
			m.queue = append(m.queue, entry.ID)
		}
	}
}

// syncToAuthStoreLocked writes an account through only when something changed,
// so the per-request Load cannot rewrite every auth file each time.
func (m *Manager) syncToAuthStoreLocked(a *Account) {
	stored, ok := authstore.Get(a.ID)
	if ok &&
		stored.AccessToken == a.AccessToken &&
		stored.RefreshToken == a.RefreshToken &&
		stored.ExpiresAt == a.ExpiresAt &&
		stored.ProjectID == a.ProjectID &&
		stored.Email == a.Email {
		return
	}
	_ = authstore.Save(authstore.Account{
		ID:           a.ID,
		Provider:     authstore.Provider,
		Email:        a.Email,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    a.ExpiresAt,
		ProjectID:    a.ProjectID,
		Label:        a.Email,
	})
}

func (m *Manager) saveLocked() {
	if _, err := paths.EnsureDataDir(); err != nil {
		logx.Warn("Failed to prepare data dir: %v", err)
		return
	}
	// Persist in rotation order, not map order. Ranging the map here rewrote
	// accounts.json in a different order on every save, so the stored order --
	// which decides which account is drained first on the next start -- shuffled
	// itself behind our back.
	entries := make([]persistedAccount, 0, len(m.accounts))
	for _, id := range m.orderedIDsLocked() {
		a := m.accounts[id]
		if a == nil {
			continue
		}
		entries = append(entries, persistedAccount{
			ID:           a.ID,
			Email:        a.Email,
			AccessToken:  a.AccessToken,
			RefreshToken: a.RefreshToken,
			ExpiresAt:    a.ExpiresAt,
			ProjectID:    a.ProjectID,
			Disabled:     a.Disabled,
		})
	}
	payload, err := json.MarshalIndent(struct {
		Accounts []persistedAccount `json:"accounts"`
	}{Accounts: entries}, "", "  ")
	if err != nil {
		logx.Warn("Failed to encode accounts: %v", err)
		return
	}
	// 0600: this file holds refresh tokens.
	if err := os.WriteFile(accountsFile(), payload, 0o600); err != nil {
		logx.Warn("Failed to save accounts: %v", err)
	}
}

// Save flushes the account list to disk.
func (m *Manager) Save() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveLocked()
}

// Add registers a new account and appends it to the rotation queue.
func (m *Manager) Add(a Account) {
	m.mu.Lock()
	m.accounts[a.ID] = &Account{
		ID:           a.ID,
		Email:        a.Email,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    a.ExpiresAt,
		ProjectID:    a.ProjectID,
	}
	if !containsString(m.queue, a.ID) {
		m.queue = append(m.queue, a.ID)
	}
	m.saveLocked()
	m.mu.Unlock()

	_ = authstore.Save(authstore.Account{
		ID:           a.ID,
		Provider:     authstore.Provider,
		Email:        a.Email,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    a.ExpiresAt,
		ProjectID:    a.ProjectID,
		Label:        a.Email,
	})
}

// Remove deletes an account by id or by email.
func (m *Manager) Remove(idOrEmail string) bool {
	m.mu.Lock()

	target := ""
	if _, ok := m.accounts[idOrEmail]; ok {
		target = idOrEmail
	} else {
		for id, a := range m.accounts {
			if a.Email == idOrEmail {
				target = id
				break
			}
		}
	}
	if target == "" {
		m.mu.Unlock()
		logx.Warn("Account not found: %s", idOrEmail)
		return false
	}

	delete(m.accounts, target)
	m.queue = removeString(m.queue, target)
	delete(m.gates, target)
	delete(m.inFlight, target)
	delete(m.lastCall, target)
	m.saveLocked()
	m.mu.Unlock()

	authstore.Delete(target)
	return true
}

// Count returns how many accounts are registered.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()
	return len(m.accounts)
}

// Emails lists every account email.
func (m *Manager) Emails() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()
	out := make([]string, 0, len(m.accounts))
	for _, id := range m.orderedIDsLocked() {
		if a, ok := m.accounts[id]; ok {
			out = append(out, a.Email)
		}
	}
	return out
}

// Has reports whether an account exists.
func (m *Manager) Has(accountID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()
	_, ok := m.accounts[accountID]
	return ok
}

func (m *Manager) ensureQueueLocked() {
	if len(m.queue) == 0 && len(m.accounts) > 0 {
		// Only reached for accounts that arrived without a stored order. Sort
		// rather than ranging the map: an arbitrary order is fine, a different
		// arbitrary order on every run is not, because it decides which account
		// is drained first.
		ids := make([]string, 0, len(m.accounts))
		for id := range m.accounts {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		m.queue = append(m.queue, ids...)
	}
}

// orderedIDsLocked returns queue order, appending any account not yet queued.
func (m *Manager) orderedIDsLocked() []string {
	m.ensureQueueLocked()
	seen := map[string]bool{}
	out := make([]string, 0, len(m.accounts))
	for _, id := range m.queue {
		if _, ok := m.accounts[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	// Anything not in the queue is sorted before being appended, for the same
	// reason as ensureQueueLocked: map order must not decide rotation.
	unqueued := make([]string, 0)
	for id := range m.accounts {
		if !seen[id] {
			unqueued = append(unqueued, id)
		}
	}
	sort.Strings(unqueued)
	return append(out, unqueued...)
}

// MoveToEndOfQueue demotes a failing account so the next request skips it.
func (m *Manager) MoveToEndOfQueue(accountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureQueueLocked()
	if !containsString(m.queue, accountID) {
		return
	}
	m.queue = append(removeString(m.queue, accountID), accountID)
}

// SetEnabled switches an account in or out of rotation and persists the change.
// It reports whether the account exists.
//
// Disabling does not delete anything: credentials, project id and quota history
// are all kept, so the account can be switched back on without signing in again.
func (m *Manager) SetEnabled(accountID string, enabled bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()

	account, ok := m.accounts[accountID]
	if !ok {
		return false
	}
	if account.Disabled == !enabled {
		return true // already in the requested state
	}

	account.Disabled = !enabled
	if enabled {
		// Coming back on, clear any stale cooldown so the account is usable
		// immediately rather than sitting out a backoff from before it was
		// switched off.
		account.rateLimitedUntil = 0
		account.consecutiveFailures = 0
	}
	m.saveLocked()

	state := "enabled"
	if !enabled {
		state = "disabled"
	}
	logx.Info("Account %s %s", account.Email, state)
	return true
}

// IsEnabled reports whether an account is in rotation.
func (m *Manager) IsEnabled(accountID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()
	account, ok := m.accounts[accountID]
	return ok && !account.Disabled
}

// EnabledCount returns how many accounts are currently in rotation.
func (m *Manager) EnabledCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()
	n := 0
	for _, account := range m.accounts {
		if !account.Disabled {
			n++
		}
	}
	return n
}

// MarkRateLimited puts an account on cooldown for a fixed duration.
func (m *Manager) MarkRateLimited(accountID string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Load first: without this the lookup misses on a cold manager and the
	// cooldown is silently dropped, which would leave the next request pointed
	// straight back at the account that just ran out.
	m.ensureLoadedLocked()
	account, ok := m.accounts[accountID]
	if !ok {
		return
	}
	account.rateLimitedUntil = nowMs() + d.Milliseconds()
	account.consecutiveFailures++
	logx.Warn("Account %s rate limited for %.0fs (failures: %d)",
		account.Email, d.Seconds(), account.consecutiveFailures)
}

// MarkSuccess clears an account's cooldown and failure streak.
func (m *Manager) MarkSuccess(accountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if account, ok := m.accounts[accountID]; ok {
		account.rateLimitedUntil = 0
		account.consecutiveFailures = 0
	}
}

// IsRateLimited reports whether an account is currently on cooldown.
func (m *Manager) IsRateLimited(accountID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	account, ok := m.accounts[accountID]
	return ok && account.rateLimitedUntil > nowMs()
}

// HoldRateLimit temporarily reserves an account during a retry wait so
// concurrent requests do not select it. The returned release restores the prior
// state unless a longer real cooldown was written meanwhile. It returns nil when
// the account already has a later cooldown.
func (m *Manager) HoldRateLimit(accountID string, d time.Duration) func() {
	m.mu.Lock()
	defer m.mu.Unlock()

	account, ok := m.accounts[accountID]
	if !ok {
		return nil
	}
	until := nowMs() + d.Milliseconds()
	previous := account.rateLimitedUntil
	if previous > until {
		return nil
	}
	account.rateLimitedUntil = until

	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if account.rateLimitedUntil != until {
			return // a real cooldown replaced our hold
		}
		if previous > nowMs() {
			account.rateLimitedUntil = previous
		} else {
			account.rateLimitedUntil = 0
		}
	}
}

// parseRateLimitReason reads Google's 429 body to decide whether this is a hard
// quota wall (rotate accounts) or transient throttling (wait and retry).
func parseRateLimitReason(statusCode int, errorText string) RateLimitReason {
	if statusCode != 429 {
		if statusCode >= 500 {
			return reasonServerError
		}
		return reasonUnknown
	}

	trimmed := strings.TrimSpace(errorText)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var root map[string]any
		if err := json.Unmarshal([]byte(trimmed), &root); err == nil {
			if errNode, ok := root["error"].(map[string]any); ok {
				if details, ok := errNode["details"].([]any); ok {
					for _, detail := range details {
						node, ok := detail.(map[string]any)
						if !ok {
							continue
						}
						switch node["reason"] {
						case "QUOTA_EXHAUSTED":
							return reasonQuotaExhausted
						case "RATE_LIMIT_EXCEEDED":
							return reasonRateLimit
						case "MODEL_CAPACITY_EXHAUSTED":
							return reasonModelCapacity
						}
					}
				}
				if message, ok := errNode["message"].(string); ok {
					lower := strings.ToLower(message)
					if strings.Contains(lower, "per minute") ||
						strings.Contains(lower, "rate limit") ||
						strings.Contains(lower, "too many requests") {
						return reasonRateLimit
					}
				}
				// RESOURCE_EXHAUSTED with no explicit QUOTA_EXHAUSTED detail is
				// far more often throttling than a spent quota.
				if status, ok := errNode["status"].(string); ok && status == "RESOURCE_EXHAUSTED" {
					return reasonRateLimit
				}
			}
		}
	}

	lower := strings.ToLower(errorText)
	switch {
	case strings.Contains(lower, "per minute"),
		strings.Contains(lower, "rate limit"),
		strings.Contains(lower, "too many requests"):
		return reasonRateLimit
	case strings.Contains(lower, "model_capacity"), strings.Contains(lower, "capacity"):
		return reasonModelCapacity
	case strings.Contains(lower, "quota"):
		return reasonQuotaExhausted
	case strings.Contains(lower, "exhausted"):
		// "exhausted" without "quota" reads as short-lived throttling.
		return reasonRateLimit
	}
	return reasonUnknown
}

// defaultRateLimitDuration escalates the cooldown as an account keeps failing.
func defaultRateLimitDuration(reason RateLimitReason, failures int) time.Duration {
	switch reason {
	case reasonQuotaExhausted:
		switch {
		case failures <= 1:
			return time.Minute
		case failures == 2:
			return 5 * time.Minute
		case failures == 3:
			return 30 * time.Minute
		default:
			return 2 * time.Hour
		}
	case reasonRateLimit:
		return 30 * time.Second
	case reasonModelCapacity:
		return 15 * time.Second
	case reasonServerError:
		return 20 * time.Second
	}
	return time.Minute
}

// RateLimitOutcome describes the cooldown applied to an account.
type RateLimitOutcome struct {
	Reason   RateLimitReason
	Duration time.Duration
}

// MarkRateLimitedFromError classifies an upstream error and applies a cooldown.
func (m *Manager) MarkRateLimitedFromError(
	accountID string, statusCode int, errorText, retryAfterHeader string,
) *RateLimitOutcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoadedLocked()

	account, ok := m.accounts[accountID]
	if !ok {
		return nil
	}

	reason := parseRateLimitReason(statusCode, errorText)
	account.consecutiveFailures++

	var duration time.Duration
	if delay, found := ParseRetryDelay(errorText, retryAfterHeader); found {
		// Trust the server's own number, with a little headroom.
		duration = delay + 500*time.Millisecond
		if duration < 2*time.Second {
			duration = 2 * time.Second
		}
	} else if statusCode == 429 {
		// A 429 with no stated delay: back off briefly rather than probing a
		// quota endpoint, which would itself consume rate limit.
		duration = 10 * time.Second
	} else {
		duration = defaultRateLimitDuration(reason, account.consecutiveFailures)
	}

	account.rateLimitedUntil = nowMs() + duration.Milliseconds()
	logx.Warn("Account %s rate limited (%s) for %.0fs (failures: %d)",
		account.Email, reason, duration.Seconds(), account.consecutiveFailures)
	return &RateLimitOutcome{Reason: reason, Duration: duration}
}

// ClearAllRateLimits drops every cooldown.
func (m *Manager) ClearAllRateLimits() {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, a := range m.accounts {
		if a.rateLimitedUntil != 0 {
			a.rateLimitedUntil = 0
			a.consecutiveFailures = 0
			count++
		}
	}
	if count > 0 {
		logx.Warn("Optimistic reset: cleared rate limits for %d account(s)", count)
	}
}

// AcquireLock serializes work on one account, honouring the configured
// concurrency and the minimum spacing between calls.
//
// On timeout it force-recovers rather than failing the request: a leaked lock
// would otherwise wedge the account permanently.
func (m *Manager) AcquireLock(ctx context.Context, accountID string) func() {
	m.mu.Lock()
	m.ensureLoadedLocked()
	gate, ok := m.gates[accountID]
	if !ok {
		gate = make(chan struct{}, accountConcurrency())
		m.gates[accountID] = gate
	}
	m.mu.Unlock()

	acquired := false
	timer := time.NewTimer(lockWaitTimeout())
	defer timer.Stop()

	select {
	case gate <- struct{}{}:
		acquired = true
	case <-timer.C:
		logx.Warn("[AccountLock] Wait timeout for %s; proceeding without the gate", accountID)
	case <-ctx.Done():
		logx.Debug("[AccountLock] Request cancelled while waiting for %s", accountID)
	}

	// Space calls on the same account apart.
	//
	// The slot is reserved including the wait, mirroring the global limiter. An
	// earlier version stamped the clock before sleeping, so the timestamp recorded
	// when a caller began waiting rather than when its call actually went out, and
	// each subsequent caller measured its gap from that too-early mark. Spacing
	// drifted shorter with every request, which is exactly how a per-credential
	// rate limit gets tripped.
	if interval := accountInterval(); interval > 0 {
		// Jitter only ever extends the spacing (0..350ms) so the configured
		// floor is always honoured; a 0ms test interval stays exactly 0.
		interval += spacingJitter(interval)
		m.mu.Lock()
		now := nowMs()
		var sleepMs int64
		if last := m.lastCall[accountID]; last > 0 {
			if elapsed := now - last; elapsed < interval.Milliseconds() {
				sleepMs = interval.Milliseconds() - elapsed
			}
		}
		m.lastCall[accountID] = now + sleepMs
		m.mu.Unlock()

		if sleepMs > 0 {
			timer := time.NewTimer(time.Duration(sleepMs) * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
		}
	}

	m.mu.Lock()
	m.inFlight[accountID]++
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if m.inFlight[accountID] > 0 {
				m.inFlight[accountID]--
			}
			m.mu.Unlock()
			if acquired {
				<-gate
			}
		})
	}
}

// refreshIfNeededLocked renews an account's token when it is close to expiry.
// It must be called with m.mu held; it releases the lock across the network call.
// needsRefreshLocked reports whether a token is within the refresh window.
func needsRefreshLocked(account *Account) bool {
	return account.ExpiresAt > 0 &&
		nowMs() > account.ExpiresAt-tokenRefreshWindow.Milliseconds()
}

func (m *Manager) refreshIfNeededLocked(ctx context.Context, account *Account) bool {
	return m.refreshLocked(ctx, account, false)
}

// refreshLocked renews an account's token. With force set it refreshes even when
// the token has not reached the expiry window, which is what a 401 needs.
func (m *Manager) refreshLocked(ctx context.Context, account *Account, force bool) bool {
	if (!force && !needsRefreshLocked(account)) || account.RefreshToken == "" {
		return true
	}

	// Coalesce concurrent refreshes of one account. Without this, every caller
	// that saw the expiring token fired its own refresh at Google; reusing a
	// refresh token can invalidate the previously issued access token, so the
	// slower call could hand back a token another request was already using.
	if waiting, inFlight := m.refreshing[account.ID]; inFlight {
		m.mu.Unlock()
		select {
		case <-waiting:
		case <-ctx.Done():
		}
		m.mu.Lock()
		// The winner already produced a fresh token, which satisfies a forced
		// caller too. Report whether their refresh succeeded.
		return account.rateLimitedUntil <= nowMs() && !needsRefreshLocked(account)
	}

	done := make(chan struct{})
	m.refreshing[account.ID] = done

	// Snapshot everything needed off-lock. Reading account fields during the
	// unlocked window would be an unsynchronized read of state other goroutines
	// write under this same mutex.
	refreshToken := account.RefreshToken
	needProject := account.ProjectID == ""

	m.mu.Unlock()
	tokens, err := RefreshAccessToken(ctx, refreshToken)
	var projectID string
	if err == nil && needProject {
		projectID = GetProjectID(ctx, tokens.AccessToken)
	}
	m.mu.Lock()

	delete(m.refreshing, account.ID)
	close(done)

	if err != nil {
		logx.Warn("Failed to refresh token for %s: %v", account.Email, err)
		// Park the account briefly so rotation moves past it.
		account.rateLimitedUntil = nowMs() + time.Minute.Milliseconds()
		return false
	}

	account.AccessToken = tokens.AccessToken
	account.ExpiresAt = nowMs() + tokens.ExpiresIn*1000
	if projectID != "" {
		account.ProjectID = projectID
	}
	m.saveLocked()
	m.syncToAuthStoreLocked(account)
	return true
}

// ensureProjectIDLocked resolves a usable project id, falling back to a
// generated one. Must be called with m.mu held.
func (m *Manager) ensureProjectIDLocked(ctx context.Context, account *Account) string {
	if account.ProjectID != "" && account.ProjectID != "unknown" {
		return account.ProjectID
	}

	token := account.AccessToken
	m.mu.Unlock()
	resolved := GetProjectID(ctx, token)
	m.mu.Lock()

	if resolved == "" {
		resolved = GenerateMockProjectID()
		logx.Warn("Account %s missing project_id, using fallback %s", account.Email, resolved)
	}
	account.ProjectID = resolved
	m.saveLocked()
	m.syncToAuthStoreLocked(account)
	return resolved
}

// NextAvailable picks the next usable account.
//
// Sticky by default: the head of the queue wins so one account is drained before
// rotating, which keeps request patterns per credential predictable. forceRotate
// skips the head, used after that account just failed.
func (m *Manager) NextAvailable(ctx context.Context, forceRotate bool) *Resolved {
	return m.nextAvailable(ctx, forceRotate, "")
}

// NextExcluding returns the next usable account other than excludeID.
//
// Rotation callers must use this rather than forceRotate. Failing over also
// demotes the failed account with MoveToEndOfQueue, which puts a healthy account
// at the head -- and forceRotate would then skip precisely the account it is
// meant to select. Naming the account to avoid is immune to the queue being
// reordered underneath the caller.
func (m *Manager) NextExcluding(ctx context.Context, excludeID string) *Resolved {
	return m.nextAvailable(ctx, false, excludeID)
}

func (m *Manager) nextAvailable(
	ctx context.Context, forceRotate bool, excludeID string,
) *Resolved {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ensureLoadedLocked()
	if len(m.accounts) == 0 {
		m.hydrateFromAuthStoreLocked("")
	}
	if len(m.accounts) == 0 {
		return nil
	}
	m.ensureQueueLocked()

	now := nowMs()
	ordered := m.orderedIDsLocked()

	// Prefer an account that is neither limited nor busy.
	hasIdle := false
	for _, id := range ordered {
		if id == excludeID {
			continue
		}
		account := m.accounts[id]
		if account == nil || account.Disabled || account.rateLimitedUntil > now {
			continue
		}
		if m.inFlight[id] == 0 {
			hasIdle = true
			break
		}
	}

	for i, id := range ordered {
		if id == excludeID {
			continue
		}
		if forceRotate && i == 0 && len(ordered) > 1 {
			continue
		}
		account := m.accounts[id]
		if account == nil || account.Disabled || account.rateLimitedUntil > now {
			continue
		}
		if hasIdle && m.inFlight[id] > 0 {
			continue
		}
		if !m.refreshIfNeededLocked(ctx, account) {
			continue
		}
		return &Resolved{
			AccessToken: account.AccessToken,
			ProjectID:   m.ensureProjectIDLocked(ctx, account),
			Email:       account.Email,
			AccountID:   account.ID,
		}
	}

	// Everything is on cooldown: find the shortest wait.
	var best *Account
	var minWait int64 = -1
	for _, id := range ordered {
		if id == excludeID {
			continue
		}
		account := m.accounts[id]
		// A disabled account is off limits even here. This branch exists to break
		// a deadlock when everything is briefly rate limited; handing back an
		// account the user switched off would be ignoring an explicit instruction.
		if account == nil || account.Disabled {
			continue
		}
		if account.rateLimitedUntil == 0 {
			best = account
			minWait = 0
			break
		}
		wait := account.rateLimitedUntil - now
		if wait < 0 {
			wait = 0
		}
		if minWait < 0 || wait < minWait {
			minWait = wait
			best = account
		}
	}
	if best == nil {
		return nil
	}

	// If the wait is trivial, treat it as a scheduling race rather than a real
	// wall and let the request through.
	if minWait >= 0 && minWait <= 2000 {
		// Clear only the cooldowns that were about to lapse anyway. This used to
		// wipe every account's cooldown because one of them happened to be free,
		// which threw away a quota-exhausted account's long backoff and sent the
		// very next request back to an account known to be empty.
		cleared := 0
		for _, id := range ordered {
			account := m.accounts[id]
			if account == nil || account.Disabled {
				continue
			}
			if account.rateLimitedUntil > now+2000 {
				continue
			}
			account.rateLimitedUntil = 0
			account.consecutiveFailures = 0
			cleared++
		}
		logx.Warn("All accounts rate limited, cleared %d short cooldown(s)", cleared)
		projectID := best.ProjectID
		if projectID == "" {
			projectID = "unknown"
		}
		return &Resolved{
			AccessToken: best.AccessToken,
			ProjectID:   projectID,
			Email:       best.Email,
			AccountID:   best.ID,
		}
	}

	logx.Warn("All accounts rate limited, min wait %.0fs", float64(minWait)/1000)
	return nil
}

// ByID returns one account, refreshing its token first. It returns nil when the
// account is missing or currently on cooldown.
func (m *Manager) ByID(ctx context.Context, accountID string) *Resolved {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ensureLoadedLocked()
	if _, ok := m.accounts[accountID]; !ok {
		m.hydrateFromAuthStoreLocked(accountID)
	}
	account, ok := m.accounts[accountID]
	if !ok {
		return nil
	}
	// Asking for an account by name must not bypass the switch: "off" means off,
	// including for a caller that pinned this account explicitly.
	if account.Disabled {
		return nil
	}
	if account.rateLimitedUntil > nowMs() {
		return nil
	}
	if !m.refreshIfNeededLocked(ctx, account) {
		return nil
	}
	return &Resolved{
		AccessToken: account.AccessToken,
		ProjectID:   m.ensureProjectIDLocked(ctx, account),
		Email:       account.Email,
		AccountID:   account.ID,
	}
}

// EmailFor returns an account's email without touching the network.
func (m *Manager) EmailFor(accountID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if account, ok := m.accounts[accountID]; ok {
		return account.Email
	}
	return ""
}

// RefreshProjectID forces a fresh project lookup, used when upstream reports the
// stored project no longer exists.
func (m *Manager) RefreshProjectID(ctx context.Context, accountID, accessTokenOverride string) string {
	m.mu.Lock()
	m.ensureLoadedLocked()
	if _, ok := m.accounts[accountID]; !ok {
		m.hydrateFromAuthStoreLocked(accountID)
	}
	account, ok := m.accounts[accountID]
	if !ok {
		m.mu.Unlock()
		return ""
	}
	if strings.TrimSpace(accessTokenOverride) != "" {
		account.AccessToken = accessTokenOverride
	}
	token := account.AccessToken
	m.mu.Unlock()

	resolved := GetProjectID(ctx, token)
	if resolved == "" {
		resolved = GenerateMockProjectID()
	}

	m.mu.Lock()
	account.ProjectID = resolved
	m.saveLocked()
	m.syncToAuthStoreLocked(account)
	m.mu.Unlock()
	return resolved
}

func containsString(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func removeString(list []string, target string) []string {
	out := list[:0]
	for _, v := range list {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

// ForceRefresh renews an account's token regardless of its expiry and returns the
// updated credential. Refreshes coalesce with the request path, so a dashboard
// poll and an in-flight completion cannot each fire one at Google.
func (m *Manager) ForceRefresh(ctx context.Context, accountID string) *Resolved {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ensureLoadedLocked()
	if _, ok := m.accounts[accountID]; !ok {
		m.hydrateFromAuthStoreLocked(accountID)
	}
	account, ok := m.accounts[accountID]
	if !ok {
		return nil
	}
	if !m.refreshLocked(ctx, account, true) {
		return nil
	}
	return &Resolved{
		AccessToken: account.AccessToken,
		ProjectID:   account.ProjectID,
		Email:       account.Email,
		AccountID:   account.ID,
	}
}

// CurrentToken returns a usable token for an account, refreshing it if it is near
// expiry. Unlike ByID it does not resolve a missing project id, so it never makes
// an extra network call for callers that already have one.
func (m *Manager) CurrentToken(ctx context.Context, accountID string) *Resolved {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ensureLoadedLocked()
	if _, ok := m.accounts[accountID]; !ok {
		m.hydrateFromAuthStoreLocked(accountID)
	}
	account, ok := m.accounts[accountID]
	if !ok {
		return nil
	}
	if !m.refreshIfNeededLocked(ctx, account) {
		return nil
	}
	return &Resolved{
		AccessToken: account.AccessToken,
		ProjectID:   account.ProjectID,
		Email:       account.Email,
		AccountID:   account.ID,
	}
}
