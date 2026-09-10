package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gravity-go/internal/apperr"
	"gravity-go/internal/appstate"
)

// stubUpstream stands in for Google's streamGenerateContent endpoint.
type stubUpstream struct {
	server   *httptest.Server
	requests int32
	// bodies records each request body the handler received.
	bodies chan string
}

// newStubUpstream points baseURLs at a local server for the duration of a test.
func newStubUpstream(t *testing.T, handler http.HandlerFunc) *stubUpstream {
	t.Helper()

	stub := &stubUpstream{bodies: make(chan string, 16)}
	stub.server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&stub.requests, 1)
			body, _ := io.ReadAll(r.Body)
			select {
			case stub.bodies <- string(body):
			default:
			}
			handler(w, r)
		}))

	originalURLs := baseURLs
	baseURLs = []string{stub.server.URL}
	t.Cleanup(func() {
		baseURLs = originalURLs
		stub.server.Close()
	})
	return stub
}

// sseChunks renders JSON values as an SSE body.
func sseChunks(values ...string) string {
	var out strings.Builder
	for _, value := range values {
		out.WriteString("data: " + value + "\n\n")
	}
	return out.String()
}

func textChunk(text string) string {
	return fmt.Sprintf(
		`{"response":{"candidates":[{"content":{"parts":[{"text":%q}]}}]}}`, text)
}

// useSingleTokenAuth configures the process-level credential path, so tests do
// not depend on the account manager or on disk.
func useSingleTokenAuth(t *testing.T) {
	t.Helper()
	t.Setenv("GRAVITY_DATA_DIR", t.TempDir())

	appstate.SetAuth(appstate.Auth{
		AccessToken:    "test-token",
		TokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		UserEmail:      "test@example.com",
		ProjectID:      "test-project",
	})
	t.Cleanup(appstate.ClearAuth)

	// Keep the manager empty so resolveAccount falls through to the token above.
	Accounts.mu.Lock()
	Accounts.accounts = map[string]*Account{}
	Accounts.queue = nil
	Accounts.loaded = true
	Accounts.mu.Unlock()
}

func TestCreateChatCompletionHappyPath(t *testing.T) {
	useSingleTokenAuth(t)

	stub := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseChunks(
			textChunk("Hello "),
			`{"response":{"candidates":[{"content":{"parts":[{"text":"world"}]}}],`+
				`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"thoughtsTokenCount":1}}}`,
		))
	})

	result, err := CreateChatCompletion(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{userMessage("hi")},
	}, CallOptions{})
	if err != nil {
		t.Fatalf("CreateChatCompletion() error = %v", err)
	}

	if len(result.ContentBlocks) != 1 || result.ContentBlocks[0].Text != "Hello world" {
		t.Errorf("blocks = %+v, want one coalesced 'Hello world'", result.ContentBlocks)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want 10 in / 5 out", result.Usage)
	}
	if got := atomic.LoadInt32(&stub.requests); got != 1 {
		t.Errorf("upstream calls = %d, want 1", got)
	}
}

// The upstream envelope must carry the auth header, the impersonated user agent
// and the SSE query flag, or Google rejects or mis-routes the call.
func TestUpstreamRequestShape(t *testing.T) {
	useSingleTokenAuth(t)

	var gotAuth, gotAgent, gotAccept, gotQuery string
	stub := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAgent = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotQuery = r.URL.RawQuery
		fmt.Fprint(w, sseChunks(textChunk("ok")))
	})

	if _, err := CreateChatCompletion(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{userMessage("hi")},
		System:   "be brief",
	}, CallOptions{}); err != nil {
		t.Fatalf("CreateChatCompletion() error = %v", err)
	}

	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.HasPrefix(gotAgent, "antigravity/") {
		t.Errorf("User-Agent = %q, want the impersonated IDE agent", gotAgent)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if gotQuery != "alt=sse" {
		t.Errorf("query = %q, want alt=sse", gotQuery)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(<-stub.bodies), &payload); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if payload["requestType"] != "agent" {
		t.Errorf("requestType = %v, want agent", payload["requestType"])
	}
	if payload["project"] != "test-project" {
		t.Errorf("project = %v, want test-project", payload["project"])
	}
	// The mapped upstream id must be sent, not the client-facing one.
	if payload["model"] != "gemini-3.7-flash-tiered" {
		t.Errorf("model = %v, want the mapped upstream id", payload["model"])
	}
}

func TestCreateChatCompletionStreamEmitsAnthropicEvents(t *testing.T) {
	useSingleTokenAuth(t)

	newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{textChunk("Hel"), textChunk("lo")} {
			fmt.Fprint(w, "data: "+chunk+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	var frames []string
	err := CreateChatCompletionStream(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{userMessage("hi")},
	}, CallOptions{}, func(frame string) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream() error = %v", err)
	}

	joined := strings.Join(frames, "")
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("stream missing %q", want)
		}
	}
	if !strings.Contains(joined, `"text":"Hel"`) || !strings.Contains(joined, `"text":"lo"`) {
		t.Error("stream did not carry both text deltas")
	}
}

// A 400 that names the missing thought_signature must be repaired and retried,
// not surfaced: otherwise one replayed call ends the whole agent turn.
func TestMissingThoughtSignature400IsRepairedAndRetried(t *testing.T) {
	useSingleTokenAuth(t)

	var calls int32
	stub := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, upstream400)
			return
		}
		fmt.Fprint(w, sseChunks(textChunk("recovered")))
	})

	result, err := CreateChatCompletion(context.Background(), ChatRequest{
		Model: "gemini-3.7-flash-high",
		Messages: []Message{
			userMessage("go"),
			{Role: "assistant", Content: MessageContent{Blocks: []ContentBlock{{
				Type: "tool_use", ID: "unsigned-1", Name: "read", Input: map[string]any{},
			}}}},
		},
	}, CallOptions{})
	if err != nil {
		t.Fatalf("CreateChatCompletion() error = %v", err)
	}

	if result.ContentBlocks[0].Text != "recovered" {
		t.Errorf("text = %q, want the retried response", result.ContentBlocks[0].Text)
	}
	if got := atomic.LoadInt32(&stub.requests); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (original + repaired retry)", got)
	}

	// The retry must have degraded the unsigned call to text.
	<-stub.bodies // the original attempt
	second := <-stub.bodies
	if !strings.Contains(second, "[Previous tool call]") {
		t.Error("the retry did not carry the degraded tool call")
	}
}

// A 400 with nothing repairable must surface immediately rather than looping.
func TestUnrepairable400SurfacesImmediately(t *testing.T) {
	useSingleTokenAuth(t)

	stub := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"malformed request"}}`)
	})

	_, err := CreateChatCompletion(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{userMessage("hi")},
	}, CallOptions{})
	if err == nil {
		t.Fatal("CreateChatCompletion() should have failed")
	}

	var upstream *apperr.UpstreamError
	if !ok(err, &upstream) {
		t.Fatalf("error type = %T, want *apperr.UpstreamError", err)
	}
	if upstream.Status != 400 {
		t.Errorf("status = %d, want 400", upstream.Status)
	}
	if got := atomic.LoadInt32(&stub.requests); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (400 is terminal)", got)
	}
}

// A 429 must not be retried against a different endpoint: it is account-specific.
func TestQuotaExhausted429Surfaces(t *testing.T) {
	useSingleTokenAuth(t)

	newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"details":[{"reason":"QUOTA_EXHAUSTED"}]}}`)
	})

	_, err := CreateChatCompletion(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{userMessage("hi")},
	}, CallOptions{})
	if err == nil {
		t.Fatal("CreateChatCompletion() should have failed")
	}

	var upstream *apperr.UpstreamError
	if !ok(err, &upstream) {
		t.Fatalf("error type = %T, want *apperr.UpstreamError", err)
	}
	if upstream.Status != 429 {
		t.Errorf("status = %d, want 429", upstream.Status)
	}

	reason, message := apperr.Summarize429(upstream)
	if reason != apperr.ReasonQuotaExhausted {
		t.Errorf("reason = %v, want quota_exhausted", reason)
	}
	if !strings.Contains(message, "quota exhausted") {
		t.Errorf("message = %q", message)
	}
}

// A 500 is endpoint-specific, so the next base URL should be tried.
func TestServerErrorFallsBackToNextEndpoint(t *testing.T) {
	useSingleTokenAuth(t)

	failing := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
		}))
	defer failing.Close()

	healthy := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, sseChunks(textChunk("second endpoint")))
		}))
	defer healthy.Close()

	originalURLs := baseURLs
	baseURLs = []string{failing.URL, healthy.URL}
	defer func() { baseURLs = originalURLs }()

	result, err := CreateChatCompletion(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{userMessage("hi")},
	}, CallOptions{})
	if err != nil {
		t.Fatalf("CreateChatCompletion() error = %v", err)
	}
	if result.ContentBlocks[0].Text != "second endpoint" {
		t.Errorf("text = %q, want the healthy endpoint's response",
			result.ContentBlocks[0].Text)
	}
}

// A cancelled client must abort the upstream call rather than leaving it running.
func TestContextCancellationAborts(t *testing.T) {
	useSingleTokenAuth(t)

	release := make(chan struct{})
	newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, sseChunks(textChunk("too late")))
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := CreateChatCompletion(ctx, ChatRequest{
			Model:    "gemini-3.7-flash-high",
			Messages: []Message{userMessage("hi")},
		}, CallOptions{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("CreateChatCompletion() should fail once the context is cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not abort the request")
	}
}

// A response that yields no parsable chunk is an error, not a silent empty reply.
func TestEmptyStreamIsAnError(t *testing.T) {
	useSingleTokenAuth(t)

	newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "")
	})

	err := CreateChatCompletionStream(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{userMessage("hi")},
	}, CallOptions{}, func(string) error { return nil })

	if err == nil {
		t.Error("an empty upstream stream should surface as an error")
	}
}

// ok is errors.As with a friendlier call site.
func ok(err error, target **apperr.UpstreamError) bool {
	upstream, is := err.(*apperr.UpstreamError)
	if is {
		*target = upstream
	}
	return is
}

// useTwoAccounts installs a real two-account manager in stored order, so
// rotation runs through the same code path production uses.
func useTwoAccounts(t *testing.T, primary, backup string) {
	t.Helper()
	t.Setenv("GRAVITY_DATA_DIR", t.TempDir())
	appstate.ClearAuth()

	Accounts.mu.Lock()
	Accounts.accounts = map[string]*Account{}
	Accounts.queue = nil
	for _, email := range []string{primary, backup} {
		Accounts.accounts[email] = &Account{
			ID: email, Email: email,
			AccessToken: "token-" + email,
			ProjectID:   "project-" + email,
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}
		Accounts.queue = append(Accounts.queue, email)
	}
	Accounts.loaded = true
	Accounts.mu.Unlock()

	t.Cleanup(func() {
		Accounts.mu.Lock()
		Accounts.accounts = map[string]*Account{}
		Accounts.queue = nil
		Accounts.loaded = false
		Accounts.mu.Unlock()
	})
}

// End to end: the primary account is used until it reports its quota is gone,
// at which point the same request transparently completes on the backup. This
// is the whole point of the rotation policy — drain one, then move on — so it
// is asserted against the real chat path rather than the manager alone.
func TestExhaustedAccountFailsOverToTheNext(t *testing.T) {
	const primary, backup = "gg.star.sa@gmail.com", "d7omcracking@gmail.com"
	useTwoAccounts(t, primary, backup)

	var mu sync.Mutex
	var tokensSeen []string

	newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		tokensSeen = append(tokensSeen, auth)
		mu.Unlock()

		// The primary is out of weekly quota; the backup still works.
		if strings.Contains(auth, "token-"+primary) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED",`+
				`"message":"Quota exceeded","details":[{"reason":"QUOTA_EXHAUSTED"}]}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseChunks(textChunk("served by the backup")))
	})

	// AllowRotation is what both HTTP handlers pass; without it a request stays
	// pinned to one account and a quota error is returned to the caller.
	result, err := CreateChatCompletion(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{userMessage("hi")},
	}, CallOptions{AllowRotation: true})
	if err != nil {
		t.Fatalf("request failed instead of failing over: %v", err)
	}
	if len(result.ContentBlocks) == 0 {
		t.Fatal("failover produced no content")
	}

	mu.Lock()
	seen := append([]string(nil), tokensSeen...)
	mu.Unlock()

	if len(seen) < 2 {
		t.Fatalf("upstream saw %d attempt(s), want the primary then the backup: %v",
			len(seen), seen)
	}
	if !strings.Contains(seen[0], "token-"+primary) {
		t.Errorf("first attempt used %q, want the primary %q", seen[0], primary)
	}
	if !strings.Contains(seen[len(seen)-1], "token-"+backup) {
		t.Errorf("final attempt used %q, want the backup %q", seen[len(seen)-1], backup)
	}

	// The exhausted account must now be on cooldown and demoted, so the next
	// request starts on the backup rather than re-probing a dead account.
	if !Accounts.IsRateLimited(primary) {
		t.Error("the exhausted account was not put on cooldown")
	}
	if next := Accounts.NextAvailable(context.Background(), false); next == nil {
		t.Error("no account available after failover")
	} else if next.AccountID != backup {
		t.Errorf("next request would use %q, want %q", next.AccountID, backup)
	}
}

// Pausing every account must actually stop traffic.
//
// resolveAccount falls back to the process-level token when no managed account
// is available, and that token belongs to one of the same Google accounts. Left
// unguarded, switching every account off in the dashboard still served requests
// from the paused credential: the switch looked right and did nothing.
func TestAllAccountsPausedRefusesToServe(t *testing.T) {
	const primary, backup = "gg.star.sa@gmail.com", "d7omcracking@gmail.com"
	useTwoAccounts(t, primary, backup)

	// A process-level token is present, exactly as it is in a real install.
	appstate.SetAuth(appstate.Auth{
		AccessToken:    "fallback-token",
		TokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		UserEmail:      primary,
		ProjectID:      "fallback-project",
	})
	t.Cleanup(appstate.ClearAuth)

	var served int32
	newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&served, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseChunks(textChunk("should never be reached")))
	})

	Accounts.SetEnabled(primary, false)
	Accounts.SetEnabled(backup, false)

	_, err := CreateChatCompletion(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{userMessage("hi")},
	}, CallOptions{AllowRotation: true})

	if err == nil {
		t.Fatal("request succeeded with every account paused")
	}
	if n := atomic.LoadInt32(&served); n != 0 {
		t.Errorf("upstream was called %d time(s) with every account paused", n)
	}
	// Error() renders only "provider upstream error (status)"; the text the
	// caller actually receives comes from the body via SummarizeUpstream, so
	// that is what must explain itself.
	var upstream *apperr.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("error = %v, want an UpstreamError", err)
	}
	message, _ := apperr.SummarizeUpstream(upstream)
	if !strings.Contains(strings.ToLower(message), "paused") {
		t.Errorf("client-visible message = %q, want it to say the accounts are paused",
			message)
	}
	if upstream.Status != 503 {
		t.Errorf("status = %d, want 503 (service paused, not a quota error)",
			upstream.Status)
	}

	// Switching one back on restores service.
	Accounts.SetEnabled(backup, true)
	if _, err := CreateChatCompletion(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{userMessage("hi")},
	}, CallOptions{AllowRotation: true}); err != nil {
		t.Errorf("request failed after re-enabling an account: %v", err)
	}
}
