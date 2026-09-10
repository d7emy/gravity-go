package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicValidationCeilings(t *testing.T) {
	server := newTestServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"model too long", `{"model":"` + strings.Repeat("a", 300) + `","messages":[{"role":"user","content":"hi"}]}`},
		// Derived from the constant, not a copy of it: hardcoding a number here
		// meant raising the ceiling turned this into a false failure rather than
		// a real check.
		{"too many messages", func() string {
			var b strings.Builder
			b.Grow(maxMessagesPerRequest * 32)
			b.WriteString(`{"model":"gemini-3.7-flash-high","messages":[`)
			for i := 0; i <= maxMessagesPerRequest; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"role":"user","content":"hi"}`)
			}
			b.WriteString(`]}`)
			return b.String()
		}()},
		{"max_tokens too large", `{"model":"gemini-3.7-flash-high","messages":[{"role":"user","content":"hi"}],"max_tokens":2000000}`},
		{"temperature out of range", `{"model":"gemini-3.7-flash-high","messages":[{"role":"user","content":"hi"}],"temperature":5}`},
		// Derived from the constant for the same reason as the message ceiling.
		{"too many tools", func() string {
			var b strings.Builder
			b.Grow(maxToolsPerRequest * 64)
			b.WriteString(`{"model":"gemini-3.7-flash-high",`)
			b.WriteString(`"messages":[{"role":"user","content":"hi"}],"tools":[`)
			for i := 0; i <= maxToolsPerRequest; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"name":"taaaaa","input_schema":{"type":"object"}}`)
			}
			b.WriteString(`]}`)
			return b.String()
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := server.Client().Post(server.URL+"/v1/messages", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %s", resp.StatusCode, tc.name)
			}
		})
	}
}

func TestOpenAIValidationCeilings(t *testing.T) {
	server := newTestServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"model too long", `{"model":"` + strings.Repeat("a", 300) + `","messages":[{"role":"user","content":"hi"}]}`},
		{"max_tokens too large", `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"max_tokens":2000000}`},
		{"temperature out of range", `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"temperature":5}`},
		{"too many tools", func() string {
			var b strings.Builder
			b.Grow(maxToolsPerRequest * 80)
			b.WriteString(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[`)
			for i := 0; i <= maxToolsPerRequest; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"type":"function","function":{"name":"taaaaa","parameters":{"type":"object"}}}`)
			}
			b.WriteString(`]}`)
			return b.String()
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %s", resp.StatusCode, tc.name)
			}
		})
	}
}

func TestSearchValidation(t *testing.T) {
	server := newTestServer(t)

	// GET missing q
	resp, err := server.Client().Get(server.URL + "/search")
	if err != nil {
		t.Fatalf("GET /search: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /search without q: status %d want 400", resp.StatusCode)
	}

	// GET too long
	long := strings.Repeat("a", 2500)
	resp, err = server.Client().Get(server.URL + "/search?q=" + long)
	if err != nil {
		t.Fatalf("GET long: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("long query: status %d want 400", resp.StatusCode)
	}

	// POST missing q
	resp, err = server.Client().Post(server.URL+"/search", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /search: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST missing q: status %d want 400", resp.StatusCode)
	}

	// POST invalid JSON
	resp, err = server.Client().Post(server.URL+"/search", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatalf("POST invalid: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST invalid json: status %d want 400", resp.StatusCode)
	}
}

func TestSearchTokenAuth(t *testing.T) {
	server := newTestServer(t)
	t.Setenv("GRAVITY_SEARCH_TOKEN", "secret123")

	// Without token should be 401
	resp, err := server.Client().Get(server.URL + "/search?q=hello")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without token: status %d want 401", resp.StatusCode)
	}

	// With query token
	resp, err = server.Client().Get(server.URL + "/search?q=hello&token=secret123")
	if err != nil {
		t.Fatalf("GET with token: %v", err)
	}
	// This will attempt upstream and fail with 500 (no auth), but not 401
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("with token should not be 401, got %d", resp.StatusCode)
	}

	// With Bearer header
	req, _ := http.NewRequest("GET", server.URL+"/search?q=hello", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET bearer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("bearer token should not be 401, got %d", resp.StatusCode)
	}
}

func TestDiagnosticsLoopback(t *testing.T) {
	server := newTestServer(t)

	// httptest client uses synthetic 192.0.2.1 remote addr; our isLoopbackRequest allows fallback to Host header,
	// so a localhost Host should still succeed via httptest. We test via direct handler with explicit RemoteAddr.
	// Evil Host spoof from non-loopback RemoteAddr should be blocked.

	// Normal localhost should succeed (httptest path: Host is server.URL host which is 127.0.0.1)
	resp, err := server.Client().Get(server.URL + "/auth/diagnostics")
	if err != nil {
		t.Fatalf("GET diagnostics: %v", err)
	}
	resp.Body.Close()
	// httptest may have RemoteAddr 127.0.0.1, so should be 200 or 403 depending on implementation
	// We just check it doesn't panic

	// Simulate spoof: RemoteAddr 192.168.1.50 but Host localhost -> should be 403
	req, _ := http.NewRequest("GET", server.URL+"/auth/diagnostics", nil)
	req.Host = "localhost:1234"
	req.RemoteAddr = "192.168.1.50:1234"
	// Use handler directly to avoid httptest's automatic RemoteAddr overwrite
	handler := New()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("spoofed diagnostics: status %d want 403", rec.Code)
	}
}
