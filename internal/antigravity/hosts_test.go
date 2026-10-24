package antigravity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gravity-go/internal/apperr"
)

// What one upstream host answers, for some accounts, whatever the model: a 429
// that says nothing about the account's own limits. Other hosts serve the same
// account fine.
const plainRefusal = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

const notFoundAnswer = `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`

type stubHost struct {
	server   *httptest.Server
	requests atomic.Int32
}

func (h *stubHost) count() int { return int(h.requests.Load()) }

// stubHosts starts one stub per handler and points baseURLs at them, in order.
// The host order is fixed (GRAVITY_JITTER=0), per-account spacing is off and the
// refusal memory starts empty, so each test controls exactly what the sweep sees.
func stubHosts(t *testing.T, handlers ...http.HandlerFunc) []*stubHost {
	t.Helper()
	t.Setenv("GRAVITY_JITTER", "0")
	t.Setenv("GRAVITY_ACCOUNT_INTERVAL_MS", "0")
	refusals.forget()
	t.Cleanup(refusals.forget)

	hosts := make([]*stubHost, len(handlers))
	urls := make([]string, len(handlers))
	for i, handler := range handlers {
		host := &stubHost{}
		handler := handler
		host.server = httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				host.requests.Add(1)
				handler(w, r)
			}))
		hosts[i] = host
		urls[i] = host.server.URL
	}

	original := baseURLs
	baseURLs = urls
	t.Cleanup(func() {
		baseURLs = original
		for _, host := range hosts {
			host.server.Close()
		}
	})
	return hosts
}

func answerWith(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

func serveText(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseChunks(textChunk(text)))
	}
}

// byToken answers differently depending on which account's token arrived.
func byToken(account string, forThatAccount, forOthers http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "token-"+account) {
			forThatAccount(w, r)
			return
		}
		forOthers(w, r)
	}
}

func hostTestRequest(model string) ChatRequest {
	return ChatRequest{Model: model, Messages: []Message{userMessage("hi")}}
}

func TestHostRefusalMemoryExpires(t *testing.T) {
	memory := newHostRefusals()
	now := time.Now()
	memory.now = func() time.Time { return now }

	memory.mark("acct", "https://a.example")
	if !memory.refused("acct", "https://a.example") {
		t.Fatal("a marked host is not remembered")
	}
	if memory.refused("acct", "https://b.example") {
		t.Error("another host was remembered as refusing")
	}
	if memory.refused("other", "https://a.example") {
		t.Error("another account was remembered as refused")
	}

	now = now.Add(hostRefusalTTL - time.Second)
	if !memory.refused("acct", "https://a.example") {
		t.Error("forgotten before the time was up")
	}
	now = now.Add(2 * time.Second)
	if memory.refused("acct", "https://a.example") {
		t.Error("still remembered after the time was up")
	}
}

func TestRefusingHostIsTriedLast(t *testing.T) {
	t.Setenv("GRAVITY_JITTER", "0")
	refusals.forget()
	t.Cleanup(refusals.forget)
	original := baseURLs
	baseURLs = []string{"https://a.example", "https://b.example", "https://c.example"}
	t.Cleanup(func() { baseURLs = original })

	refusals.mark("acct", "https://a.example")

	want := []string{"https://b.example", "https://c.example", "https://a.example"}
	if got := orderedBaseURLs("acct"); !slices.Equal(got, want) {
		t.Errorf("order = %v, want the refusing host last: %v", got, want)
	}
	if got := orderedBaseURLs("other"); got[0] != "https://a.example" {
		t.Errorf("another account's order = %v, want it unaffected", got)
	}

	// With the shuffle on, the refusing host stays last and the others still take
	// turns leading: the order is only constrained, not made fixed.
	t.Setenv("GRAVITY_JITTER", "1")
	leaders := map[string]bool{}
	for range 60 {
		got := orderedBaseURLs("acct")
		if got[len(got)-1] != "https://a.example" {
			t.Fatalf("order = %v, want the refusing host last", got)
		}
		leaders[got[0]] = true
	}
	if len(leaders) != 2 {
		t.Errorf("leaders = %v, want both remaining hosts to lead sometimes", leaders)
	}
}

// The reported problem. One host answers a plain 429 for this account; the next
// serves it. That 429 is the host's, not the account's limit: the request must not
// wait it out (it used to, for 2s or more), and the account must not be rested.
func TestPlain429FromOneHostIsNotTheAccountsLimit(t *testing.T) {
	const primary, backup = acctFirst, acctSecond
	useTwoAccounts(t, primary, backup)
	hosts := stubHosts(t, answerWith(429, plainRefusal), serveText("served by the second host"))

	start := time.Now()
	result, err := CreateChatCompletion(context.Background(),
		hostTestRequest("gemini-3.7-flash-high"), CallOptions{AllowRotation: true})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("request failed although a host could serve it: %v", err)
	}
	if len(result.ContentBlocks) == 0 {
		t.Fatal("no content came back")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("took %v: the host's 429 was waited out as if it were the account's limit", elapsed)
	}
	if hosts[0].count() != 1 || hosts[1].count() != 1 {
		t.Errorf("host calls = %d and %d, want one each", hosts[0].count(), hosts[1].count())
	}
	if Accounts.IsRateLimited(primary) {
		t.Error("the account was rested because one host turned it away")
	}
	if next := Accounts.NextAvailable(context.Background(), false); next == nil || next.AccountID != primary {
		t.Errorf("the next request would not use the primary account: %+v", next)
	}

	// The refusing host is remembered, so the next request does not meet it first.
	if !refusals.refused(primary, baseURLs[0]) {
		t.Error("the refusing host was not remembered")
	}
	if _, err := CreateChatCompletion(context.Background(),
		hostTestRequest("gemini-3.7-flash-high"), CallOptions{AllowRotation: true}); err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	if hosts[0].count() != 1 {
		t.Errorf("the refusing host was called again (%d calls) instead of being tried last",
			hosts[0].count())
	}
	if hosts[1].count() != 2 {
		t.Errorf("the serving host answered %d calls, want 2", hosts[1].count())
	}
}

// Streaming is the path most clients use, and has its own copy of the loop.
func TestPlain429FromOneHostIsNotTheAccountsLimitWhenStreaming(t *testing.T) {
	const primary, backup = acctFirst, acctSecond
	useTwoAccounts(t, primary, backup)
	hosts := stubHosts(t, answerWith(429, plainRefusal), serveText("served by the second host"))

	var events int
	start := time.Now()
	err := CreateChatCompletionStream(context.Background(),
		hostTestRequest("gemini-3.7-flash-high"), CallOptions{AllowRotation: true},
		func(string) error { events++; return nil })
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("stream failed although a host could serve it: %v", err)
	}
	if events == 0 {
		t.Fatal("no events were emitted")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("took %v: the host's 429 was waited out as if it were the account's limit", elapsed)
	}
	if Accounts.IsRateLimited(primary) {
		t.Error("the account was rested because one host turned it away")
	}
	if !refusals.refused(primary, baseURLs[0]) {
		t.Error("the refusing host was not remembered")
	}
	if hosts[0].count() != 1 || hosts[1].count() != 1 {
		t.Errorf("host calls = %d and %d, want one each", hosts[0].count(), hosts[1].count())
	}
}

// A spent quota is the account's own limit on every host, so it is still handled
// at once: rest, demote, move to the next account, without sweeping the other hosts
// first.
func TestSpentQuotaIsNotSweptAcrossHosts(t *testing.T) {
	const primary, backup = acctFirst, acctSecond
	useTwoAccounts(t, primary, backup)
	quotaGone := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded",` +
		`"details":[{"reason":"QUOTA_EXHAUSTED"}]}}`
	hosts := stubHosts(t,
		byToken(primary, answerWith(429, quotaGone), serveText("served by the backup")),
		serveText("never reached"))

	result, err := CreateChatCompletion(context.Background(),
		hostTestRequest("gemini-3.7-flash-high"), CallOptions{AllowRotation: true})
	if err != nil {
		t.Fatalf("request failed instead of moving to the backup: %v", err)
	}
	if len(result.ContentBlocks) == 0 {
		t.Fatal("no content came back")
	}
	if hosts[1].count() != 0 {
		t.Errorf("the second host was called %d time(s): a spent quota must not be swept across hosts",
			hosts[1].count())
	}
	if !Accounts.IsRateLimited(primary) {
		t.Error("the exhausted account was not rested")
	}
}

// When no host serves the request, a plain 429 is the account's limit after all
// and is handled exactly as it always was: wait, retry, then rest the account and
// move to the next one.
func TestEveryHostAnswering429IsStillTheAccountsLimit(t *testing.T) {
	const primary, backup = acctFirst, acctSecond
	useTwoAccounts(t, primary, backup)
	// A short stated delay keeps the in-place retries quick.
	throttled := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Too many requests",` +
		`"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.1s"}]}}`
	both := byToken(primary, answerWith(429, throttled), serveText("served by the backup"))
	hosts := stubHosts(t, both, both)

	result, err := CreateChatCompletion(context.Background(),
		hostTestRequest("gemini-3.7-flash-high"), CallOptions{AllowRotation: true})
	if err != nil {
		t.Fatalf("request failed instead of moving to the backup: %v", err)
	}
	if len(result.ContentBlocks) == 0 {
		t.Fatal("no content came back")
	}
	if !Accounts.IsRateLimited(primary) {
		t.Error("an account that every host refused was not rested")
	}
	if refusals.refused(primary, baseURLs[0]) || refusals.refused(primary, baseURLs[1]) {
		t.Error("hosts were remembered as refusing although none of them served the request")
	}
	if hosts[0].count() < 2 || hosts[1].count() < 2 {
		t.Errorf("host calls = %d and %d, want every host tried more than once",
			hosts[0].count(), hosts[1].count())
	}
}

// A host remembered as refusing the account must not hide a real answer. Asking
// for a model the account does not have gets a 404 from the hosts that know about
// it; the refusing host's 429 used to win and read as "resource exhausted".
func TestRefusingHostDoesNotHideAMissingModel(t *testing.T) {
	const primary, backup = acctFirst, acctSecond
	useTwoAccounts(t, primary, backup)
	stubHosts(t,
		answerWith(404, notFoundAnswer), answerWith(404, notFoundAnswer), answerWith(429, plainRefusal))
	// Learned earlier, from requests the first two hosts could serve.
	refusals.mark(primary, baseURLs[2])

	start := time.Now()
	_, err := CreateChatCompletion(context.Background(),
		hostTestRequest("gemini-3.1-pro-high"), CallOptions{AllowRotation: true})
	elapsed := time.Since(start)

	var upstream *apperr.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("error = %v, want an UpstreamError", err)
	}
	if upstream.Status != 404 {
		t.Errorf("status = %d, want 404: the refusing host's 429 hid the real answer", upstream.Status)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("took %v, want an immediate answer", elapsed)
	}
	if Accounts.IsRateLimited(primary) || Accounts.IsRateLimited(backup) {
		t.Error("accounts were rested because of a model that does not exist")
	}
}

// Search tries the other hosts after a 429 already; what it gains is learning
// which host refuses the account, so it stops starting there.
func TestSearchRemembersTheHostThatRefusedTheAccount(t *testing.T) {
	useSingleTokenAuth(t)
	hosts := stubHosts(t, answerWith(429, plainRefusal), answerWith(200, groundedSearchResponse))

	if _, err := PerformWebSearch(context.Background(), "latest go release", ""); err != nil {
		t.Fatalf("search failed although a host could serve it: %v", err)
	}
	if !refusals.refused("", baseURLs[0]) {
		t.Fatal("the refusing host was not remembered")
	}

	if _, err := PerformWebSearch(context.Background(), "latest go release", ""); err != nil {
		t.Fatalf("second search failed: %v", err)
	}
	if hosts[0].count() != 1 {
		t.Errorf("the refusing host was called %d times, want it tried last and so not reached", hosts[0].count())
	}
}
