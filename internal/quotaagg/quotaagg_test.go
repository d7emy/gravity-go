package quotaagg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gravity-go/internal/antigravity"
	"gravity-go/internal/authstore"
)

// The summary an account metered only on Claude/GPT gets: no Gemini group.
const claudeOnlySummary = `{"groups": [{
  "displayName": "Claude and GPT models",
  "buckets": [
    {"bucketId": "3p-weekly", "window": "weekly", "remainingFraction": 1, "resetTime": "2026-10-31T11:53:43Z"},
    {"bucketId": "3p-5h", "window": "5h", "remainingFraction": 1, "resetTime": "2026-10-24T16:53:43Z"}
  ]
}]}`

// The same summary for an account that is metered on Gemini too.
const withGeminiSummary = `{"groups": [
  {"displayName": "Gemini models", "buckets": [
    {"bucketId": "gemini-weekly", "window": "weekly", "remainingFraction": 0.73, "resetTime": "2026-10-31T00:00:00Z"},
    {"bucketId": "gemini-5h", "window": "5h", "remainingFraction": 1, "resetTime": "2026-10-24T16:00:00Z"}
  ]},
  {"displayName": "Claude and GPT models", "buckets": [
    {"bucketId": "3p-weekly", "window": "weekly", "remainingFraction": 0.45, "resetTime": "2026-10-31T11:53:43Z"}
  ]}
]}`

// Per-model quota as fetchAvailableModels reports it for the first kind of
// account: Gemini models are listed with quota even though the summary omits them.
// They share one pool, so every Gemini model carries the same fraction and reset.
const modelCatalogue = `{"models": {
  "claude-opus-4-6-thinking": {"quotaInfo": {"remainingFraction": 1, "resetTime": "2026-10-24T16:57:08Z"}},
  "gemini-2.5-pro":           {"quotaInfo": {"remainingFraction": 0.4, "resetTime": "2026-10-24T16:57:08Z"}},
  "gemini-3.8-flash-tiered":  {"quotaInfo": {"remainingFraction": 0.4, "resetTime": "2026-10-24T16:57:08Z"}},
  "tab_flash_lite_preview":   {"quotaInfo": {"remainingFraction": 1}}
}}`

type stubReply struct {
	status int
	body   string
}

type cloudCodeStub struct {
	summaryCalls, modelsCalls atomic.Int32
}

// stubCloudCode serves the two quota endpoints and points the client at them.
func stubCloudCode(t *testing.T, summary, models stubReply) *cloudCodeStub {
	t.Helper()
	stub := &cloudCodeStub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reply stubReply
		switch r.URL.Path {
		case "/v1internal:retrieveUserQuotaSummary":
			stub.summaryCalls.Add(1)
			reply = summary
		case "/v1internal:fetchAvailableModels":
			stub.modelsCalls.Add(1)
			reply = models
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	}))
	previous := antigravity.SetCloudCodeBaseURLForTest(server.URL)
	t.Cleanup(func() {
		antigravity.SetCloudCodeBaseURLForTest(previous)
		server.Close()
	})
	return stub
}

func testAccount() *authstore.Account {
	// ProjectID is set so no lookup, and no write back to the auth store, happens.
	return &authstore.Account{ID: "a@example.com", ProjectID: "project"}
}

func barKeys(bars []antigravity.QuotaBar) []string {
	keys := make([]string, len(bars))
	for i, bar := range bars {
		keys[i] = bar.Key
	}
	return keys
}

func assertKeys(t *testing.T, bars []antigravity.QuotaBar, want ...string) {
	t.Helper()
	got := barKeys(bars)
	if len(got) != len(want) {
		t.Fatalf("bar keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bar keys = %v, want %v", got, want)
		}
	}
}

// The reported bug: a Claude-only summary left the dashboard with no Gemini rows
// at all, although per-model quota for Gemini was available.
func TestQueryQuotaAddsGeminiWhenSummaryOmitsIt(t *testing.T) {
	stubCloudCode(t,
		stubReply{http.StatusOK, claudeOnlySummary},
		stubReply{http.StatusOK, modelCatalogue})

	bars, err := queryQuota(context.Background(), "token", "project", testAccount())
	if err != nil {
		t.Fatalf("queryQuota: %v", err)
	}

	assertKeys(t, bars, "gemini", "3p-weekly", "3p-5h")
	if bars[0].Percentage != 40 || bars[0].ResetTime != "2026-10-24T16:57:08Z" {
		t.Errorf("Gemini = %+v, want 40%% resetting 16:57:08Z", bars[0])
	}
	// The Claude rows keep the summary's own numbers.
	if bars[1].Label != "Claude weekly" || bars[2].Label != "Claude 5h" {
		t.Errorf("Claude rows changed: %+v", bars[1:])
	}
}

func TestQueryQuotaTrustsSummaryWhenItHasGemini(t *testing.T) {
	stub := stubCloudCode(t,
		stubReply{http.StatusOK, withGeminiSummary},
		stubReply{http.StatusOK, modelCatalogue})

	bars, err := queryQuota(context.Background(), "token", "project", testAccount())
	if err != nil {
		t.Fatalf("queryQuota: %v", err)
	}

	assertKeys(t, bars, "gemini-weekly", "gemini-5h", "3p-weekly")
	if n := stub.modelsCalls.Load(); n != 0 {
		t.Errorf("per-model quota fetched %d time(s); the summary already had Gemini", n)
	}
}

// The per-model lookup only fills a gap. If it fails, the account still shows
// what the summary gave rather than dropping to placeholders or cached numbers.
func TestQueryQuotaKeepsSummaryWhenModelLookupFails(t *testing.T) {
	stubCloudCode(t,
		stubReply{http.StatusOK, claudeOnlySummary},
		stubReply{http.StatusInternalServerError, `{"error":"boom"}`})

	bars, err := queryQuota(context.Background(), "token", "project", testAccount())
	if err != nil {
		t.Fatalf("queryQuota: %v", err)
	}

	assertKeys(t, bars, "3p-weekly", "3p-5h")
}

func TestQueryQuotaWithoutGeminiModelsAddsNothing(t *testing.T) {
	stubCloudCode(t,
		stubReply{http.StatusOK, claudeOnlySummary},
		stubReply{http.StatusOK, `{"models": {"claude-opus-4-6-thinking": {"quotaInfo": {"remainingFraction": 1}}}}`})

	bars, err := queryQuota(context.Background(), "token", "project", testAccount())
	if err != nil {
		t.Fatalf("queryQuota: %v", err)
	}

	assertKeys(t, bars, "3p-weekly", "3p-5h")
}

// Unchanged behaviour, pinned because this path shares code with the fix: when
// the summary endpoint itself fails, the grouped per-model bars take over.
func TestQueryQuotaFallsBackToGroupedBarsWhenSummaryFails(t *testing.T) {
	stubCloudCode(t,
		stubReply{http.StatusInternalServerError, `{"error":"boom"}`},
		stubReply{http.StatusOK, modelCatalogue})

	bars, err := queryQuota(context.Background(), "token", "project", testAccount())
	if err != nil {
		t.Fatalf("queryQuota: %v", err)
	}

	assertKeys(t, bars, "claude_gpt", "gpro", "gflash")
}
