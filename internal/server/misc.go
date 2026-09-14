package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"gravity-go/internal/apperr"
	"gravity-go/internal/logbuf"
	"gravity-go/internal/quotaagg"
	"gravity-go/internal/settings"
	"gravity-go/internal/usage"
)

// pingFailure renders a failed account ping without turning it into an HTTP error:
// the dashboard wants the reason on the card, not an exception.
func pingFailure(accountID, modelID string, err error) map[string]any {
	out := map[string]any{
		"success":   false,
		"provider":  "antigravity",
		"accountId": accountID,
		"modelId":   modelID,
	}
	var upstream *apperr.UpstreamError
	if errors.As(err, &upstream) {
		message, reason := apperr.SummarizeUpstream(upstream)
		out["status"] = upstream.Status
		out["error"] = message
		if reason != "" {
			out["reason"] = reason
		} else {
			out["reason"] = nil
		}
		return out
	}
	out["error"] = err.Error()
	return out
}

func handleQuotaJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, quotaagg.Get(r.Context()))
}

func handleUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, usage.Get())
}

func handleUsageReset(w http.ResponseWriter, r *http.Request) {
	usage.Reset()
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// applyLogCaptureSetting keeps the log buffer in sync with the saved setting.
func applyLogCaptureSetting(s settings.AppSettings) {
	logbuf.SetEnabled(s.CaptureLogs)
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	sinceID, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	writeJSON(w, http.StatusOK, logbuf.Get(limit, sinceID))
}

func handleLogStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	stream := newSSEWriter(w)

	if !logbuf.IsEnabled() {
		_ = stream.write("event: disabled\ndata: Log capture disabled\n\n")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	sinceID, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)

	// Subscribe before replaying the backlog, so nothing that lands in between
	// is dropped.
	updates, unsubscribe := logbuf.Subscribe()
	defer unsubscribe()

	for _, entry := range logbuf.Get(limit, sinceID).Entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		if err := stream.write("event: log\ndata: " + string(encoded) + "\n\n"); err != nil {
			return
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case entry := <-updates:
			encoded, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			if err := stream.write("event: log\ndata: " + string(encoded) + "\n\n"); err != nil {
				return
			}
		}
	}
}

func handleUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	// The dashboard throws unless success is true, and reads currentVersion /
	// latestVersion / canApply / status. Returning a different shape surfaced as
	// a red "Update failed: HTTP 200" on the Settings tab.
	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"status":          "blocked",
		"updateAvailable": false,
		"canApply":        false,
		"currentVersion":  Version,
		"latestVersion":   Version,
		"message":         "Self-update is not available in this build.",
		"commandHint":     "rebuild with: go build -o gravity-go.exe .",
	})
}

func handleUpdatesApply(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": false,
		"error":   "Self-update is not available in this build. Rebuild with: go build -o gravity-go.exe .",
	})
}
