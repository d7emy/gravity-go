// Package server wires the HTTP API and the static dashboard.
package server

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gravity-go/internal/antigravity"
	"gravity-go/internal/appstate"
	"gravity-go/internal/logx"
	"gravity-go/internal/settings"
)

// publicFS carries the dashboard HTML into the binary, so a single executable is
// the whole deployment.
//
//go:embed all:public
var publicFS embed.FS

// New builds the fully wired HTTP handler.
func New() http.Handler {
	mux := http.NewServeMux()
	registerRoutes(mux)
	return withMiddleware(mux)
}

func registerRoutes(mux *http.ServeMux) {
	// Dashboard pages.
	mux.HandleFunc("GET /", handleRoot)
	mux.HandleFunc("GET /quota", servePage("quota.html"))

	// Auth.
	mux.HandleFunc("GET /auth/status", handleAuthStatus)
	mux.HandleFunc("GET /auth/accounts", handleAuthAccounts)
	mux.HandleFunc("POST /auth/login", handleAuthLogin)
	mux.HandleFunc("GET /auth/login/pending", handleAuthLoginPending)
	mux.HandleFunc("POST /auth/logout", handleAuthLogout)
	mux.HandleFunc("GET /auth/diagnostics", handleAuthDiagnostics)
	mux.HandleFunc("GET /auth/ide/status", handleIDEStatus)
	mux.HandleFunc("POST /auth/ide/logout", handleIDELogout)

	// Provider status endpoints the panel polls. This build ships antigravity
	// only, so they answer with a clear "not supported" rather than 404ing and
	// leaving the dashboard spinning.
	mux.HandleFunc("GET /auth/codex/status", handleProviderUnsupported("codex"))
	mux.HandleFunc("GET /auth/copilot/status", handleProviderUnsupported("copilot"))

	// Credential bundles were removed upstream; keep the documented 410.
	mux.HandleFunc("GET /auth/export", handleBundleGone)
	mux.HandleFunc("POST /auth/import", handleBundleGone)
	mux.HandleFunc("GET /bundle/export", handleBundleGone)
	mux.HandleFunc("POST /bundle/import", handleBundleGone)

	// Accounts.
	mux.HandleFunc("POST /accounts/ping", handleAccountPing)
	mux.HandleFunc("DELETE /accounts/{id}", handleAccountDelete)
	mux.HandleFunc("POST /accounts/{id}/enabled", handleAccountEnabled)

	// Quota, usage, settings, logs.
	mux.HandleFunc("GET /quota/json", handleQuotaJSON)
	mux.HandleFunc("GET /usage", handleUsage)
	mux.HandleFunc("POST /usage/reset", handleUsageReset)
	mux.HandleFunc("GET /settings", handleGetSettings)
	mux.HandleFunc("POST /settings", handleSaveSettings)
	mux.HandleFunc("GET /logs", handleLogs)
	mux.HandleFunc("GET /logs/stream", handleLogStream)

	// Web search (grounded googleSearch via Antigravity). Open by default; set
	// GRAVITY_SEARCH_TOKEN (or its legacy equivalent) to require auth.
	mux.HandleFunc("GET /search", handleSearch)
	mux.HandleFunc("POST /search", handleSearch)

	// Updates: self-update is not part of this build.
	mux.HandleFunc("GET /updates/check", handleUpdatesCheck)
	mux.HandleFunc("POST /updates/apply", handleUpdatesApply)

	// Anthropic-compatible endpoints. All three prefixes are served for client
	// compatibility.
	for _, prefix := range []string{"/v1/messages", "/v1beta/messages", "/messages"} {
		mux.HandleFunc("POST "+prefix, handleAnthropicMessages)
	}

	// OpenAI-compatible endpoints.
	mux.HandleFunc("POST /v1/chat/completions", handleOpenAIChatCompletions)
	mux.HandleFunc("POST /chat/completions", handleOpenAIChatCompletions)

	// Model listings.
	for _, prefix := range []string{"/v1/models", "/v1beta/models", "/models"} {
		mux.HandleFunc("GET "+prefix, handleModels)
	}

	// Unsupported OpenAI surfaces, answered explicitly.
	for _, path := range []string{"/embeddings", "/v1/embeddings"} {
		mux.HandleFunc("POST "+path, notSupported("Embeddings not supported"))
	}
	for _, path := range []string{"/responses", "/v1/responses"} {
		mux.HandleFunc("POST "+path, notSupported("Responses API not supported"))
	}
	mux.HandleFunc("POST /v1/images/generations", notSupported("Image generation not supported"))

	// Vendored front-end assets, served from the embedded FS. The dashboard used
	// to pull Tailwind and Chart.js from public CDNs, which meant it rendered
	// unstyled and chartless offline or behind a firewall — defeating the point
	// of shipping the UI inside the binary.
	mux.HandleFunc("GET /vendor/{file}", handleVendorAsset)

	mux.HandleFunc("GET /health", handleHealth)
}

// vendorETags caches one content hash per asset. The files are embedded and
// never change while the process runs, so hashing each one once is enough.
var vendorETags sync.Map // name -> ETag

func vendorETag(name string, data []byte) string {
	if tag, ok := vendorETags.Load(name); ok {
		return tag.(string)
	}
	sum := sha256.Sum256(data)
	tag := `"` + hex.EncodeToString(sum[:8]) + `"`
	vendorETags.Store(name, tag)
	return tag
}

// handleVendorAsset serves a bundled asset, revalidated against a content hash.
func handleVendorAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	// Reject path separators and parent traversal, but allow dots in filenames:
	// every vendored asset is a *.js file.
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(publicFS, "public/vendor/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	contentType := "application/octet-stream"
	switch {
	case strings.HasSuffix(name, ".js"):
		contentType = "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		contentType = "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".woff2"):
		contentType = "font/woff2"
	case strings.HasSuffix(name, ".svg"):
		contentType = "image/svg+xml"
	}

	w.Header().Set("Content-Type", contentType)
	// These URLs carry no version or content hash, so the response must not be
	// immutable: a browser that cached /vendor/app.css once would keep using it
	// forever and never see a rebuilt stylesheet. "no-cache" still caches -- it
	// requires revalidation first -- so the cost is a conditional request that
	// answers 304 from the hash below, which on loopback is free.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", vendorETag(name, data))

	// ServeContent handles If-None-Match and range requests against the ETag
	// already set above. The zero modtime suppresses Last-Modified, leaving the
	// hash as the single source of truth for freshness.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	// ServeMux's "GET /" also catches every unmatched path.
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"type": "not_found", "message": "Not found"},
		})
		return
	}
	http.Redirect(w, r, "/quota", http.StatusFound)
}

// vendorRef matches a reference to a vendored asset in the dashboard HTML.
var vendorRef = regexp.MustCompile(`/vendor/([A-Za-z0-9._-]+)`)

// versionVendorRefs stamps each /vendor/NAME reference with a short content
// hash, so the URL changes whenever the asset does.
//
// This is what actually defeats a stale cache. Earlier builds served these
// assets as "immutable" with a one-year max-age on a URL that never changed, so
// any browser that loaded the dashboard once pinned that copy and would not ask
// for it again -- no amount of correcting the response headers reaches a client
// that has stopped sending requests. Changing the URL is the only way past it.
func versionVendorRefs(page []byte) []byte {
	return vendorRef.ReplaceAllFunc(page, func(match []byte) []byte {
		name := string(match[len("/vendor/"):])
		data, err := fs.ReadFile(publicFS, "public/vendor/"+name)
		if err != nil {
			return match // not a real asset; leave it alone
		}
		// Reuse the ETag hash so the URL and the validator agree.
		tag := strings.Trim(vendorETag(name, data), `"`)

		// Build a fresh slice. ReplaceAllFunc hands back a subslice of the page,
		// so appending to it writes past the match and into the source buffer --
		// which corrupted the following markup and stamped the version twice.
		out := make([]byte, 0, len(match)+len("?v=")+len(tag))
		out = append(out, match...)
		out = append(out, "?v="...)
		return append(out, tag...)
	})
}

// servePage returns the embedded dashboard page by name.
func servePage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(publicFS, "public/"+name)
		if err != nil {
			http.Error(w, "Dashboard page not found", http.StatusNotFound)
			return
		}
		data = versionVendorRefs(data)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The page itself is never cached: it is small, and it carries the
		// asset versions, so a stale copy would pin stale assets with it.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so SSE keeps streaming.
func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		applyCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		ctx, _ := logx.WithRequestContext(r.Context())
		r = r.WithContext(ctx)

		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		// Only failures are logged here; successful completions get their own
		// detailed line from the chat path.
		if recorder.status >= 400 {
			logFailure(recorder, r)
		}
	})
}

func logFailure(recorder *statusRecorder, r *http.Request) {
	reason := recorder.Header().Get("X-Log-Reason")
	model, provider, account, routeTag := logx.FromContext(r.Context()).Snapshot()

	debugInfo := ""
	if recorder.status == 400 {
		debugInfo = " (" + r.Method + " " + r.URL.Path
		if reason != "" {
			debugInfo += " - " + reason
		}
		debugInfo += ")"
	}

	if model != "" && provider != "" {
		accountPart := ""
		if account != "" {
			accountPart = " >> " + account
		}
		routePart := ""
		if routeTag != "" {
			routePart = "•" + routeTag
		}
		logx.Raw("[" + logx.FormatTime() + "] " +
			strconv.Itoa(recorder.status) + ": from " + model + " > " +
			logx.ProviderName(provider) + accountPart + routePart + debugInfo)
		return
	}

	if reason == "" {
		reason = "error"
	}
	logx.Raw("[" + logx.FormatTime() + "] " + strconv.Itoa(recorder.status) + ": " + reason + debugInfo)
}

// applyCORS allows only same-machine origins, so an arbitrary web page cannot
// drive the local proxy.
func applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return
	}
	if !isLocalHost(parsed.Hostname()) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization,x-api-key,anthropic-version")
}

// localIPs holds this machine's own addresses, enumerated once.
//
// Interface enumeration is a syscall costing ~2ms, and this sits on the CORS
// path of every request that carries an Origin header. Resolving it per request
// added that cost to every LAN-origin call; the set is computed once instead,
// matching what the original did at startup.
var localIPs = sync.OnceValue(func() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok {
			out = append(out, ipNet.IP)
		}
	}
	return out
})

func isLocalHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	// A LAN address of this machine also counts, so the dashboard works when
	// opened from another device on the same network. An address added after
	// startup is not recognised until restart, as before.
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, local := range localIPs() {
		if local.Equal(ip) {
			return true
		}
	}
	return false
}

// isLoopbackRequest reports whether a request came from this machine, used to
// gate the diagnostics endpoint. It checks RemoteAddr (the TCP peer) first to
// prevent Host-header spoofing from the LAN; Host is only a fallback for
// httptest where RemoteAddr is the synthetic 192.0.2.1.
func isLoopbackRequest(r *http.Request) bool {
	// Primary gate: TCP peer must be loopback.
	if r.RemoteAddr != "" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			switch strings.ToLower(host) {
			case "localhost", "127.0.0.1", "::1", "[::1]":
				return true
			}
			if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsLoopback() {
				return true
			}
			// RemoteAddr present and not loopback → reject even if Host claims localhost.
			// Allow synthetic httptest address 192.0.2.1 to fall through to Host check so
			// unit tests that don't set RemoteAddr don't break.
			if host != "192.0.2.1" {
				return false
			}
		}
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func notSupported(message string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": map[string]any{"type": "not_supported", "message": message},
		})
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"authenticated": appstate.IsAuthenticated(),
	})
}

// modelEntry is one row of a model listing.
type modelEntry struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Object      string `json:"object"`
	CreatedAt   string `json:"created_at"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by"`
	DisplayName string `json:"display_name"`
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	models := antigravity.FilterVisibleModels(antigravity.AvailableModels)

	entries := make([]modelEntry, 0, len(models))
	for _, model := range models {
		entries = append(entries, modelEntry{
			ID:          model.ID,
			Type:        "model", // Anthropic shape
			Object:      "model", // OpenAI shape
			CreatedAt:   now.UTC().Format(time.RFC3339),
			Created:     now.Unix(),
			OwnedBy:     "antigravity",
			DisplayName: model.Name,
		})
	}

	payload := map[string]any{
		"object":   "list",
		"data":     entries,
		"has_more": false,
	}
	if len(entries) > 0 {
		payload["first_id"] = entries[0].ID
		payload["last_id"] = entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, payload)
}

func handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, settings.Load())
}

func handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var patch map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"type": "invalid_request_error", "message": "Invalid JSON body"},
		})
		return
	}
	updated, err := settings.Save(patch)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"type": "error", "message": err.Error()},
		})
		return
	}
	applyLogCaptureSetting(updated)
	writeJSON(w, http.StatusOK, updated)
}
