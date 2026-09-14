package antigravity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubOAuth points the token and project endpoints at a local server and counts
// how many refreshes actually reach it.
func stubOAuth(t *testing.T) *int32 {
	t.Helper()

	var refreshes int32
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				atomic.AddInt32(&refreshes, 1)
				// A slow endpoint widens the window for a duplicate refresh.
				time.Sleep(120 * time.Millisecond)
				fmt.Fprint(w, `{"access_token":"fresh-token","expires_in":3600}`)
				return
			}
			fmt.Fprint(w, `{"cloudaicompanionProject":"stub-project"}`)
		}))

	origToken, origProject := oauthTokenURL, oauthProjectURL
	oauthTokenURL = server.URL + "/token"
	oauthProjectURL = server.URL + "/project"
	t.Cleanup(func() {
		oauthTokenURL, oauthProjectURL = origToken, origProject
		server.Close()
	})
	return &refreshes
}

// expiredAccountManager seeds one account whose token is already expired, so any
// use of it must refresh first.
func expiredAccountManager(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("GRAVITY_DATA_DIR", t.TempDir())
	t.Setenv("GRAVITY_ACCOUNT_INTERVAL_MS", "0")

	m := &Manager{
		accounts:   map[string]*Account{},
		gates:      map[string]chan struct{}{},
		inFlight:   map[string]int{},
		lastCall:   map[string]int64{},
		refreshing: map[string]chan struct{}{},
		loaded:     true,
	}
	m.accounts["a"] = &Account{
		ID:           "a",
		Email:        "a@example.com",
		AccessToken:  "stale-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		ProjectID:    "existing-project",
	}
	m.queue = []string{"a"}
	return m
}

func TestExpiredTokenIsRefreshed(t *testing.T) {
	refreshes := stubOAuth(t)
	m := expiredAccountManager(t)

	got := m.NextAvailable(context.Background(), false)
	if got == nil {
		t.Fatal("NextAvailable() = nil")
	}
	if got.AccessToken != "fresh-token" {
		t.Errorf("token = %q, want the refreshed one", got.AccessToken)
	}
	if n := atomic.LoadInt32(refreshes); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
}

// Regression: two goroutines both seeing an expiring token each fired their own
// refresh at Google. Google can invalidate the earlier token when a refresh is
// reused, so the second call could hand back a token the first request was
// already using. Refreshes for one account must coalesce into a single call.
func TestConcurrentUseRefreshesTokenOnlyOnce(t *testing.T) {
	refreshes := stubOAuth(t)
	m := expiredAccountManager(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	tokens := make([]string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if got := m.NextAvailable(ctx, false); got != nil {
				tokens[i] = got.AccessToken
			}
		}(i)
	}
	wg.Wait()

	if n := atomic.LoadInt32(refreshes); n != 1 {
		t.Errorf("refreshes = %d, want exactly 1 for a single account", n)
	}
	for i, token := range tokens {
		if token != "fresh-token" {
			t.Errorf("caller %d got token %q, want the refreshed one", i, token)
		}
	}
}

// ByID goes through the same refresh path and must coalesce with it.
func TestByIDCoalescesWithNextAvailable(t *testing.T) {
	refreshes := stubOAuth(t)
	m := expiredAccountManager(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				m.NextAvailable(ctx, false)
			} else {
				m.ByID(ctx, "a")
			}
		}(i)
	}
	wg.Wait()

	if n := atomic.LoadInt32(refreshes); n != 1 {
		t.Errorf("refreshes = %d, want exactly 1 across both entry points", n)
	}
}

// A failed refresh must park the account rather than hand out a stale token.
func TestFailedRefreshParksAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		}))
	defer server.Close()

	orig := oauthTokenURL
	oauthTokenURL = server.URL
	defer func() { oauthTokenURL = orig }()

	m := expiredAccountManager(t)

	if got := m.NextAvailable(context.Background(), false); got != nil {
		t.Errorf("NextAvailable() = %+v, want nil after a failed refresh", got)
	}
	if !m.IsRateLimited("a") {
		t.Error("account should be parked after a failed refresh")
	}
}

// The dashboard's quota poll and the request path both need a current token.
// They used to refresh independently, so a poll landing next to a completion
// fired two refreshes at Google for one account. Both now go through the
// manager, which coalesces them.
func TestQuotaAndRequestPathsShareOneRefresh(t *testing.T) {
	refreshes := stubOAuth(t)
	m := expiredAccountManager(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				m.NextAvailable(ctx, false) // request path
			case 1:
				m.CurrentToken(ctx, "a") // quota path
			case 2:
				m.ByID(ctx, "a")
			}
		}(i)
	}
	wg.Wait()

	if n := atomic.LoadInt32(refreshes); n != 1 {
		t.Errorf("refreshes = %d, want 1 shared across the quota and request paths", n)
	}
}

// ForceRefresh renews even a token that is not near expiry, which is what a 401
// on a seemingly-valid token needs.
func TestForceRefreshRenewsUnexpiredToken(t *testing.T) {
	refreshes := stubOAuth(t)
	m := expiredAccountManager(t)

	// Give the account a token that is comfortably in the future.
	m.mu.Lock()
	m.accounts["a"].ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	m.accounts["a"].AccessToken = "still-valid-but-rejected"
	m.mu.Unlock()

	if got := m.CurrentToken(context.Background(), "a"); got == nil ||
		got.AccessToken != "still-valid-but-rejected" {
		t.Fatalf("CurrentToken() should not refresh an unexpired token, got %+v", got)
	}
	if n := atomic.LoadInt32(refreshes); n != 0 {
		t.Fatalf("refreshes = %d, want 0 before forcing", n)
	}

	got := m.ForceRefresh(context.Background(), "a")
	if got == nil {
		t.Fatal("ForceRefresh() = nil")
	}
	if got.AccessToken != "fresh-token" {
		t.Errorf("token = %q, want the forced refresh result", got.AccessToken)
	}
	if n := atomic.LoadInt32(refreshes); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
}

func TestForceRefreshUnknownAccountReturnsNil(t *testing.T) {
	stubOAuth(t)
	m := expiredAccountManager(t)

	if got := m.ForceRefresh(context.Background(), "nobody"); got != nil {
		t.Errorf("ForceRefresh(unknown) = %+v, want nil", got)
	}
}
