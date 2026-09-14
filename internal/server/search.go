package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"gravity-go/internal/antigravity"
	"gravity-go/internal/apperr"
	"gravity-go/internal/logx"
)

const maxSearchQueryLength = 2000

func requiredSearchToken() string {
	for _, k := range []string{"GRAVITY_SEARCH_TOKEN", "ANTI_API_SEARCH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func isSearchAuthorized(r *http.Request, queryToken string) bool {
	expected := requiredSearchToken()
	if expected == "" {
		return true
	}
	if queryToken != "" && queryToken == expected {
		return true
	}
	if t := r.URL.Query().Get("token"); t == expected {
		return true
	}
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h != "" {
		if len(h) > 6 && strings.EqualFold(h[:6], "Bearer") {
			if token := strings.TrimSpace(h[6:]); token == expected {
				return true
			}
		} else if h == expected {
			return true
		}
	}
	return false
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		handleSearchGet(w, r)
		return
	}
	if r.Method == http.MethodPost {
		handleSearchPost(w, r)
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
		"error": map[string]any{"type": "invalid_request_error", "message": "Method not allowed"},
	})
}

func handleSearchGet(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		query = strings.TrimSpace(r.URL.Query().Get("query"))
	}
	if !isSearchAuthorized(r, r.URL.Query().Get("token")) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"type": "unauthorized", "message": "Invalid or missing search token"},
		})
		return
	}
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"type": "invalid_request_error", "message": "Missing required query parameter: q"},
		})
		return
	}
	if len(query) > maxSearchQueryLength {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"type": "invalid_request_error", "message": "Query too long (max 2000 characters)"},
		})
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "json"
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))

	result, err := antigravity.PerformWebSearch(r.Context(), query, model)
	if err != nil {
		writeSearchError(w, err)
		return
	}
	// Log success like TS: [time] 200 search•model•sources•elapsed
	logx.Raw("[" + logx.FormatTime() + "] 200 search•" + result.Model + "•" + itoa(len(result.Sources)) + " sources•" + formatElapsed(result.ElapsedMs))

	if format == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(formatSearchResultAsText(result)))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func handleSearchPost(w http.ResponseWriter, r *http.Request) {
	// Authorize via query token or header; POST also allows ?token=
	if !isSearchAuthorized(r, r.URL.Query().Get("token")) {
		// also check body? original checks query/header only for POST as well
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"type": "unauthorized", "message": "Invalid or missing search token"},
		})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"type": "invalid_request_error", "message": "Body must be JSON"},
		})
		return
	}
	var payload struct {
		Q      string `json:"q"`
		Query  string `json:"query"`
		Model  string `json:"model"`
		Format string `json:"format"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"type": "invalid_request_error", "message": "Body must be JSON"},
			})
			return
		}
	}
	query := strings.TrimSpace(payload.Q)
	if query == "" {
		query = strings.TrimSpace(payload.Query)
	}
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"type": "invalid_request_error", "message": "Missing required field: q"},
		})
		return
	}
	if len(query) > maxSearchQueryLength {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"type": "invalid_request_error", "message": "Query too long (max 2000 characters)"},
		})
		return
	}

	result, err := antigravity.PerformWebSearch(r.Context(), query, payload.Model)
	if err != nil {
		writeSearchError(w, err)
		return
	}
	logx.Raw("[" + logx.FormatTime() + "] 200 search•" + result.Model + "•" + itoa(len(result.Sources)) + " sources•" + formatElapsed(result.ElapsedMs))

	if strings.ToLower(strings.TrimSpace(payload.Format)) == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(formatSearchResultAsText(result)))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeSearchError(w http.ResponseWriter, err error) {
	var upstream *apperr.UpstreamError
	if errors.As(err, &upstream) {
		message, reason := apperr.SummarizeUpstream(upstream)
		w.Header().Set("X-Log-Reason", "search failed")
		status := upstream.Status
		if status < 400 || status > 599 {
			status = 502
		}
		payload := map[string]any{
			"error": map[string]any{
				"type":     "upstream_error",
				"message":  message,
				"provider": upstream.Provider,
			},
		}
		if reason != "" {
			payload["error"].(map[string]any)["reason"] = reason
		}
		writeJSON(w, status, payload)
		return
	}
	w.Header().Set("X-Log-Reason", "search failed")
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": map[string]any{"type": "error", "message": err.Error()},
	})
}

func formatSearchResultAsText(result antigravity.SearchResult) string {
	var b strings.Builder
	if result.Answer != "" {
		b.WriteString(result.Answer)
	} else {
		b.WriteString("(no answer)")
	}
	if len(result.Sources) > 0 {
		b.WriteString("\n\nSources:\n")
		for i, s := range result.Sources {
			b.WriteString("  [" + itoa(i+1) + "] " + s.Title + " - " + s.URL + "\n")
		}
		// trim trailing newline added in loop, but keep one
	}
	// original does: if searched.length>0 push "", Searched: joined
	if len(result.Searched) > 0 {
		// ensure we have a blank line before Searched unless already
		if len(result.Sources) == 0 {
			b.WriteString("\n\n")
		} else {
			// sources already added newline; add one more blank line? original pushes "" then line
			// we already have Sources block ending with newline; add newline
			b.WriteString("\n")
		}
		b.WriteString("Searched: " + strings.Join(result.Searched, " | "))
	}
	return strings.TrimRight(b.String(), "\n")
}

func itoa(n int) string {
	// avoid importing strconv in this file already imported? we have not; use simple
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

func formatElapsed(ms int64) string {
	return fmt.Sprintf("%.1f", float64(ms)/1000)
}
