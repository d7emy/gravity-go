package server

import (
	"context"
	"encoding/json"
	"fmt"
	"gravity-go/internal/antigravity"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("GRAVITY_DATA_DIR", t.TempDir())

	server := httptest.NewServer(New())
	t.Cleanup(server.Close)
	return server
}

func getJSON(t *testing.T, server *httptest.Server, path string) map[string]any {
	t.Helper()

	resp, err := server.Client().Get(server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("GET %s: response is not JSON: %v", path, err)
	}
	return out
}

func TestHealthEndpoint(t *testing.T) {
	server := newTestServer(t)

	body := getJSON(t, server, "/health")
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	if _, present := body["authenticated"]; !present {
		t.Error("health should report an authenticated flag")
	}
}

// The dashboard is embedded, so it must serve without any files on disk.
func TestDashboardIsEmbedded(t *testing.T) {
	server := newTestServer(t)

	resp, err := server.Client().Get(server.URL + "/quota")
	if err != nil {
		t.Fatalf("GET /quota: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", contentType)
	}
	if resp.ContentLength == 0 {
		t.Error("dashboard page is empty")
	}
}

func TestRootRedirectsToQuota(t *testing.T) {
	server := newTestServer(t)

	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}
	if location := resp.Header.Get("Location"); location != "/quota" {
		t.Errorf("Location = %q, want /quota", location)
	}
}

// Only the whitelisted models may appear, on every alias path.
func TestModelListingsAreWhitelisted(t *testing.T) {
	server := newTestServer(t)

	for _, path := range []string{"/v1/models", "/v1beta/models", "/models"} {
		t.Run(path, func(t *testing.T) {
			body := getJSON(t, server, path)

			data, ok := body["data"].([]any)
			if !ok {
				t.Fatalf("data = %v, want an array", body["data"])
			}
			if len(data) == 0 {
				t.Fatal("model listing is empty")
			}

			for _, entry := range data {
				model := entry.(map[string]any)
				id, _ := model["id"].(string)
				// Assert against the whitelist itself rather than a copy of it.
				// A hardcoded list here breaks on every model addition without
				// catching anything the real check would miss.
				if !antigravity.IsVisibleModel(id) {
					t.Errorf("unexpected model %q in the listing", id)
				}
				// Both the Anthropic and OpenAI shapes must be present.
				if model["type"] != "model" || model["object"] != "model" {
					t.Errorf("model %q missing a compatibility field: %v", id, model)
				}
			}
		})
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	server := newTestServer(t)

	before := getJSON(t, server, "/settings")
	if before["captureLogs"] != false {
		t.Fatalf("captureLogs = %v, want false by default", before["captureLogs"])
	}

	resp, err := server.Client().Post(server.URL+"/settings", "application/json",
		strings.NewReader(`{"captureLogs":true}`))
	if err != nil {
		t.Fatalf("POST /settings: %v", err)
	}
	resp.Body.Close()

	after := getJSON(t, server, "/settings")
	if after["captureLogs"] != true {
		t.Errorf("captureLogs = %v, want true after the update", after["captureLogs"])
	}
	// A partial update must not reset the untouched keys.
	if after["language"] != before["language"] {
		t.Errorf("language changed from %v to %v on a partial update",
			before["language"], after["language"])
	}
}

// The dashboard iterates a provider-keyed map, so every key must be present even
// though this build only populates antigravity.
func TestAuthAccountsKeepsAllProviderKeys(t *testing.T) {
	server := newTestServer(t)

	body := getJSON(t, server, "/auth/accounts")
	accounts, ok := body["accounts"].(map[string]any)
	if !ok {
		t.Fatalf("accounts = %v, want an object", body["accounts"])
	}

	for _, provider := range []string{"antigravity", "codex", "copilot", "zed", "kiro", "grok"} {
		value, present := accounts[provider]
		if !present {
			t.Errorf("provider key %q is missing", provider)
			continue
		}
		if _, isArray := value.([]any); !isArray {
			t.Errorf("provider %q = %v, want an array", provider, value)
		}
	}
}

func TestUnsupportedEndpointsAnswerExplicitly(t *testing.T) {
	server := newTestServer(t)

	cases := []struct {
		path       string
		wantStatus int
	}{
		{"/v1/embeddings", http.StatusNotImplemented},
		{"/embeddings", http.StatusNotImplemented},
		{"/v1/responses", http.StatusNotImplemented},
		{"/v1/images/generations", http.StatusNotImplemented},
		{"/bundle/import", http.StatusGone},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := server.Client().Post(server.URL+tc.path, "application/json",
				strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("POST %s: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestMessagesRejectsInvalidPayloads(t *testing.T) {
	server := newTestServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"not JSON", `not json at all`},
		{"no model", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"empty messages", `{"model":"gemini-3.7-flash-high","messages":[]}`},
		{"message with no role", `{"model":"gemini-3.7-flash-high","messages":[{"content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := server.Client().Post(server.URL+"/v1/messages",
				"application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST /v1/messages: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}

			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("error response is not JSON: %v", err)
			}
			errNode, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("body = %v, want an error object", body)
			}
			if errNode["type"] != "invalid_request_error" {
				t.Errorf("error type = %v, want invalid_request_error", errNode["type"])
			}
		})
	}
}

// All three Anthropic prefixes must be routed, for client compatibility.
func TestAllMessagePrefixesAreRouted(t *testing.T) {
	server := newTestServer(t)

	for _, path := range []string{"/v1/messages", "/v1beta/messages", "/messages"} {
		t.Run(path, func(t *testing.T) {
			resp, err := server.Client().Post(server.URL+path, "application/json",
				strings.NewReader(`{"model":"x","messages":[]}`))
			if err != nil {
				t.Fatalf("POST %s: %v", path, err)
			}
			defer resp.Body.Close()

			// 400 proves the route exists and reached validation; 404 would mean
			// it was never registered.
			if resp.StatusCode == http.StatusNotFound {
				t.Errorf("%s is not routed", path)
			}
		})
	}
}

// An arbitrary web page must not be able to drive the local proxy.
func TestCORSRejectsRemoteOrigins(t *testing.T) {
	server := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/health", nil)
	req.Header.Set("Origin", "https://evil.example.com")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if allowed := resp.Header.Get("Access-Control-Allow-Origin"); allowed != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for a remote origin", allowed)
	}
}

func TestCORSAllowsLocalhost(t *testing.T) {
	server := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/health", nil)
	req.Header.Set("Origin", "http://localhost:3000")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if allowed := resp.Header.Get("Access-Control-Allow-Origin"); allowed != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the localhost origin echoed", allowed)
	}
}

func TestUsageResetRoundTrip(t *testing.T) {
	server := newTestServer(t)

	resp, err := server.Client().Post(server.URL+"/usage/reset", "application/json",
		strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /usage/reset: %v", err)
	}
	resp.Body.Close()

	body := getJSON(t, server, "/usage")
	if body["totalCost"] != float64(0) {
		t.Errorf("totalCost = %v, want 0 after a reset", body["totalCost"])
	}
}

func TestUnknownPathReturns404(t *testing.T) {
	server := newTestServer(t)

	resp, err := server.Client().Get(server.URL + "/no/such/path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// The Routing and Remote surfaces were removed with the features behind them.
// They must be gone rather than lingering as dead pages the nav no longer links.
func TestRemovedSurfacesAreGone(t *testing.T) {
	server := newTestServer(t)

	for _, path := range []string{
		"/routing", "/routing-panel", "/routing/config",
		"/remote-panel", "/remote/config", "/remote/status", "/tunnel/status",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := server.Client().Get(server.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404 for a removed surface", resp.StatusCode)
			}
		})
	}
}

// The dashboard must not reach for any third-party host: the whole UI ships
// inside the binary, so it has to render offline.
func TestDashboardHasNoExternalAssetReferences(t *testing.T) {
	server := newTestServer(t)

	resp, err := server.Client().Get(server.URL + "/quota")
	if err != nil {
		t.Fatalf("GET /quota: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	page := string(body)

	for _, host := range []string{
		"cdn.tailwindcss.com", "cdn.jsdelivr.net",
		"fonts.googleapis.com", "fonts.gstatic.com", "gstatic.com",
	} {
		if strings.Contains(page, host) {
			t.Errorf("dashboard still references %s", host)
		}
	}
}

// Every asset the dashboard asks for must actually be served, with a sane
// content type.
func TestVendoredAssetsAreServed(t *testing.T) {
	server := newTestServer(t)

	cases := []struct{ path, wantType string }{
		{"/vendor/app.css", "text/css"},
		{"/vendor/chart.umd.min.js", "application/javascript"},
		{"/vendor/comfortaa.woff2", "font/woff2"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := server.Client().Get(server.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, tc.wantType) {
				t.Errorf("Content-Type = %q, want %s", ct, tc.wantType)
			}
			if resp.ContentLength == 0 {
				t.Error("asset is empty")
			}
		})
	}
}

// The asset route reads from an embedded FS; a traversal attempt must not escape
// it, while ordinary dotted filenames must still resolve.
func TestVendorAssetRejectsTraversal(t *testing.T) {
	server := newTestServer(t)

	for _, path := range []string{
		"/vendor/..%2f..%2fquota.html",
		"/vendor/../quota.html",
	} {
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			continue // the client may refuse to send it at all, which is fine
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "<html") {
			t.Errorf("%s escaped the vendor directory", path)
		}
	}
}

// The update endpoint must answer the shape the dashboard reads, or the Settings
// tab shows a red "Update failed: HTTP 200".
func TestUpdatesCheckMatchesDashboardContract(t *testing.T) {
	server := newTestServer(t)

	body := getJSON(t, server, "/updates/check")
	if body["success"] != true {
		t.Errorf("success = %v, want true (the dashboard throws otherwise)", body["success"])
	}
	for _, key := range []string{"currentVersion", "latestVersion", "canApply", "status"} {
		if _, present := body[key]; !present {
			t.Errorf("response is missing %q", key)
		}
	}
}

// Regression: the vendor assets were served "immutable" with a one-year
// max-age, but their URLs carry no version or content hash. A browser that
// fetched /vendor/app.css once would keep the stale copy for a year and never
// see a rebuilt stylesheet — every future frontend change would be invisible
// until the user cleared their cache.
func TestVendorAssetsAreRevalidatedNotFrozen(t *testing.T) {
	server := newTestServer(t)

	resp, err := server.Client().Get(server.URL + "/vendor/app.css")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	cc := resp.Header.Get("Cache-Control")
	if strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q; an unversioned URL must never be immutable", cc)
	}
	// A long max-age has the same effect as immutable: no revalidation.
	if strings.Contains(cc, "max-age=") && !strings.Contains(cc, "max-age=0") {
		t.Errorf("Cache-Control = %q; a long max-age on an unversioned URL "+
			"leaves stale assets in place", cc)
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag; without one the browser must refetch the whole asset every load")
	}

	// The point of the ETag: an unchanged asset costs a 304, not a re-download.
	req, err := http.NewRequest(http.MethodGet, server.URL+"/vendor/app.css", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("If-None-Match", etag)

	second, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	defer second.Body.Close()

	if second.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET status = %d, want 304", second.StatusCode)
	}
}

// Different assets must not share an ETag, or updating one would leave the
// others being served from a stale cache entry.
func TestVendorETagsAreContentDerived(t *testing.T) {
	server := newTestServer(t)

	tags := map[string]string{}
	for _, name := range []string{"app.css", "chart.umd.min.js", "comfortaa.woff2"} {
		resp, err := server.Client().Get(server.URL + "/vendor/" + name)
		if err != nil {
			t.Fatalf("GET %s: %v", name, err)
		}
		resp.Body.Close()

		tag := resp.Header.Get("ETag")
		if tag == "" {
			t.Fatalf("%s: no ETag", name)
		}
		if prev, clash := tags[tag]; clash {
			t.Errorf("%s and %s share ETag %s", name, prev, tag)
		}
		tags[tag] = name
	}
}

// The dashboard must point at versioned asset URLs. Correct cache headers alone
// cannot rescue a browser that pinned an asset under the old "immutable"
// response — it stops sending requests entirely — so the URL has to move.
func TestDashboardReferencesVersionedAssets(t *testing.T) {
	server := newTestServer(t)

	resp, err := server.Client().Get(server.URL + "/quota")
	if err != nil {
		t.Fatalf("GET /quota: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	for _, name := range []string{"app.css", "chart.umd.min.js"} {
		bare := "/vendor/" + name + `"`
		if strings.Contains(page, bare) {
			t.Errorf("%s is referenced without a version; a pinned cache entry "+
				"would never be replaced", name)
		}
		if !strings.Contains(page, "/vendor/"+name+"?v=") {
			t.Errorf("%s is missing its ?v= content hash", name)
		}
	}

	// A versioned URL must still serve the asset.
	start := strings.Index(page, "/vendor/app.css?v=")
	if start < 0 {
		t.Fatal("no versioned app.css reference to follow")
	}
	end := start
	for end < len(page) && page[end] != '"' && page[end] != '\'' {
		end++
	}
	assetResp, err := server.Client().Get(server.URL + page[start:end])
	if err != nil {
		t.Fatalf("GET %s: %v", page[start:end], err)
	}
	defer assetResp.Body.Close()
	if assetResp.StatusCode != http.StatusOK {
		t.Errorf("versioned asset status = %d, want 200", assetResp.StatusCode)
	}
}

// The version must track the content: two different assets cannot share one.
func TestAssetVersionsDifferPerAsset(t *testing.T) {
	server := newTestServer(t)

	resp, err := server.Client().Get(server.URL + "/quota")
	if err != nil {
		t.Fatalf("GET /quota: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	versions := map[string]string{}
	for _, m := range regexp.MustCompile(`/vendor/([A-Za-z0-9._-]+)\?v=([a-f0-9]+)`).
		FindAllStringSubmatch(string(body), -1) {
		if prev, ok := versions[m[2]]; ok && prev != m[1] {
			t.Errorf("%s and %s share version %s", m[1], prev, m[2])
		}
		versions[m[2]] = m[1]
	}
	if len(versions) < 2 {
		t.Errorf("found %d versioned assets, want at least 2", len(versions))
	}
}

// recordingWriter captures SSE output and supports Flush, standing in for a real
// streaming response.
type recordingWriter struct {
	mu      sync.Mutex
	header  http.Header
	written strings.Builder
}

func (rw *recordingWriter) Header() http.Header {
	if rw.header == nil {
		rw.header = http.Header{}
	}
	return rw.header
}

func (rw *recordingWriter) Write(b []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.written.Write(b)
}

func (rw *recordingWriter) WriteHeader(int) {}
func (rw *recordingWriter) Flush()          {}

func (rw *recordingWriter) body() string {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.written.String()
}

// A reasoning model can stay silent for minutes before its first token, and an
// idle connection is what proxies and clients drop. The keep-alive must emit
// comment frames during that silence and stop as soon as the request ends.
func TestKeepAliveEmitsDuringSilenceAndStops(t *testing.T) {
	original := keepAliveInterval
	keepAliveInterval = 15 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = original })

	rw := &recordingWriter{}
	stream := newSSEWriter(rw)

	stop := startKeepAlive(context.Background(), stream)
	time.Sleep(90 * time.Millisecond)
	stop()

	got := rw.body()
	if n := strings.Count(got, ": ping\n\n"); n < 2 {
		t.Fatalf("got %d ping frames in 90ms at a 15ms interval, want at least 2 "+
			"(body=%q)", n, got)
	}

	// After stopping, nothing more may be written — the response is finished and
	// a late frame would corrupt it.
	settled := rw.body()
	time.Sleep(60 * time.Millisecond)
	if after := rw.body(); after != settled {
		t.Errorf("keep-alive kept writing after stop: %d extra bytes",
			len(after)-len(settled))
	}
}

// Both streaming endpoints must install the keep-alive. Only the Anthropic path
// had it, so a slow request on the OpenAI endpoint could be dropped mid-think.
func TestBothStreamingEndpointsInstallKeepAlive(t *testing.T) {
	for _, file := range []string{"messages.go", "openai.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if !strings.Contains(string(src), "startKeepAlive(r.Context(), stream)") {
			t.Errorf("%s does not install the keep-alive; a slow model would "+
				"leave the connection idle until something times it out", file)
		}
	}
}

// The dashboard switch posts to this endpoint; it has to validate its input and
// report the state back so the UI can trust what it renders.
func TestAccountEnabledEndpoint(t *testing.T) {
	server := newTestServer(t)

	post := func(path, body string) (int, map[string]any) {
		t.Helper()
		resp, err := server.Client().Post(server.URL+path, "application/json",
			strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		var decoded map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&decoded)
		return resp.StatusCode, decoded
	}

	t.Run("unknown account is a 404, not a silent success", func(t *testing.T) {
		status, body := post("/accounts/nobody@example.com/enabled", `{"enabled":false}`)
		if status != http.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
		if body["success"] != false {
			t.Errorf("success = %v, want false", body["success"])
		}
	})

	t.Run("missing enabled field is rejected", func(t *testing.T) {
		// An empty body must not be read as "disable": the switch would flip
		// from a malformed request.
		status, body := post("/accounts/someone@example.com/enabled", `{}`)
		if status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
		if body["success"] != false {
			t.Errorf("success = %v, want false", body["success"])
		}
	})

	t.Run("malformed json is rejected", func(t *testing.T) {
		status, _ := post("/accounts/someone@example.com/enabled", `not json`)
		if status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})
}

// Regression: the message-count ceiling was 1000, inherited from the TypeScript
// build. Every tool call and its result is a message, so a real agentic session
// passes that quickly — a 669k-token opencode session was rejected outright,
// even though upstream accepts it (verified live at 3001 messages). The genuine
// limits are the context window and the body cap, both enforced elsewhere.
func TestLongConversationsAreNotRejectedOnMessageCount(t *testing.T) {
	if maxMessagesPerRequest <= 1000 {
		t.Fatalf("maxMessagesPerRequest = %d; a long agentic session exceeds this "+
			"and would be refused before it ever reaches upstream", maxMessagesPerRequest)
	}

	server := newTestServer(t)

	// 3001 turns: past the old ceiling, well inside the new one.
	var b strings.Builder
	b.WriteString(`{"model":"gemini-3.8-flash-high","max_tokens":64,"messages":[`)
	for i := 0; i < 1500; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"role":"user","content":"step %d"},`, i)
		fmt.Fprintf(&b, `{"role":"assistant","content":"ack %d"}`, i)
	}
	b.WriteString(`,{"role":"user","content":"done"}]}`)

	resp, err := server.Client().Post(server.URL+"/v1/messages",
		"application/json", strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	// The request has no upstream to reach in this test, so anything except a
	// message-count rejection means validation let it through.
	if resp.StatusCode == http.StatusBadRequest &&
		strings.Contains(string(body), "Too many messages") {
		t.Errorf("3001 messages were rejected on count: %s", body)
	}
}

// Regression: the tool ceiling was 100, inherited from the TypeScript build. A
// single client declares 15-20 tools, but the limit applies to the whole set, so
// stacking MCP servers reached it — while upstream happily takes far more
// (verified live at 1000 declarations, still selecting the right tool).
func TestLargeToolsetsAreNotRejectedOnCount(t *testing.T) {
	if maxToolsPerRequest <= 100 {
		t.Fatalf("maxToolsPerRequest = %d; several MCP servers together exceed "+
			"this and would be refused before reaching upstream", maxToolsPerRequest)
	}

	server := newTestServer(t)

	var b strings.Builder
	b.WriteString(`{"model":"gemini-3.8-flash-high","max_tokens":64,`)
	b.WriteString(`"messages":[{"role":"user","content":"hi"}],"tools":[`)
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":"tool_%d","input_schema":{"type":"object"}}`, i)
	}
	b.WriteString(`]}`)

	resp, err := server.Client().Post(server.URL+"/v1/messages",
		"application/json", strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusBadRequest &&
		strings.Contains(string(body), "Too many tools") {
		t.Errorf("500 tools were rejected on count: %s", body)
	}
}
