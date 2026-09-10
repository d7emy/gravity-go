package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"gravity-go/internal/antigravity"
	"gravity-go/internal/appstate"
	"gravity-go/internal/authstore"
	"gravity-go/internal/idedb"
)

func handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	email, name := appstate.UserInfo()
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": appstate.IsAuthenticated(),
		"email":         nullableString(email),
		"name":          nullableString(name),
	})
}

// nullableString renders "" as JSON null, which is what the dashboard expects.
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func handleAuthAccounts(w http.ResponseWriter, r *http.Request) {
	antigravity.Accounts.Load()
	summaries := authstore.Summaries()
	if summaries == nil {
		summaries = []authstore.Summary{}
	}
	// The dashboard iterates a provider-keyed map. This build populates only
	// antigravity; the other keys stay present but empty so its rendering code
	// needs no change.
	empty := []authstore.Summary{}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": map[string]any{
			"antigravity": summaries,
			"codex":       empty,
			"copilot":     empty,
			"zed":         empty,
			"kiro":        empty,
			"grok":        empty,
		},
	})
}

// authLoginRequest is the /auth/login body. An empty body starts the
// interactive OAuth flow; a body with tokens registers them directly.
type authLoginRequest struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
}

func handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req authLoginRequest
	if body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}

	if req.Provider != "" && req.Provider != "antigravity" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "This build supports the antigravity provider only.",
		})
		return
	}

	// Token supplied directly: register it without a browser round trip.
	if req.AccessToken != "" {
		antigravity.SetAuth(req.AccessToken, req.RefreshToken, req.Email, req.Name)
		antigravity.Accounts.Load()

		id := req.Email
		if id == "" {
			id = "account-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
		}
		email := req.Email
		if email == "" {
			email = "unknown"
		}
		auth := appstate.GetAuth()
		antigravity.Accounts.Add(antigravity.Account{
			ID:           id,
			Email:        email,
			AccessToken:  req.AccessToken,
			RefreshToken: req.RefreshToken,
			ExpiresAt:    auth.TokenExpiresAt,
			ProjectID:    auth.ProjectID,
		})

		writeJSON(w, http.StatusOK, map[string]any{
			"success":       true,
			"authenticated": true,
			"provider":      "antigravity",
			"email":         req.Email,
			"name":          req.Name,
		})
		return
	}

	// Interactive OAuth. The flow waits on the user's browser, so it is bounded
	// by its own five-minute timeout rather than the request context.
	// Detach from the HTTP request context so a client disconnect doesn't abort
	// the 5-minute browser flow.
	ctx := r.Context()
	// Go 1.20+: WithoutCancel; fallback to Background for older.
	// Use context.WithoutCancel equivalent via new context that ignores cancel.
	detached := context.WithoutCancel(ctx)
	result := antigravity.StartOAuthLogin(detached)
	if !result.Success {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   result.Error,
		})
		return
	}

	antigravity.Accounts.Load()
	auth := appstate.GetAuth()
	if auth.AccessToken != "" && auth.RefreshToken != "" {
		id := auth.UserEmail
		if id == "" {
			id = "account-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
		}
		email := auth.UserEmail
		if email == "" {
			email = "unknown"
		}
		antigravity.Accounts.Add(antigravity.Account{
			ID:           id,
			Email:        email,
			AccessToken:  auth.AccessToken,
			RefreshToken: auth.RefreshToken,
			ExpiresAt:    auth.TokenExpiresAt,
			ProjectID:    auth.ProjectID,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"authenticated": true,
		"provider":      "antigravity",
		"email":         result.Email,
	})
}

func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	antigravity.ClearAuth()
	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"authenticated": false,
	})
}

// handleAuthDiagnostics reports account health. It is loopback-only because the
// report names accounts and token expiry.
func handleAuthDiagnostics(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"success": false,
			"error":   "Diagnostics is only available from localhost.",
		})
		return
	}

	antigravity.Accounts.Load()
	accounts := authstore.List()
	now := time.Now().UnixMilli()

	reports := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		report := map[string]any{
			"id":              account.ID,
			"provider":        "antigravity",
			"displayName":     displayName(account),
			"hasRefreshToken": account.RefreshToken != "",
			"hasProjectId":    account.ProjectID != "",
			"rateLimited":     antigravity.Accounts.IsRateLimited(account.ID),
		}
		if account.ExpiresAt > 0 {
			report["expiresAt"] = account.ExpiresAt
			report["expired"] = account.ExpiresAt <= now
			report["expiresInSeconds"] = (account.ExpiresAt - now) / 1000
		}
		reports = append(reports, report)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
		"accounts":    reports,
		"ide":         idedb.Status(),
	})
}

func displayName(account authstore.Account) string {
	if account.Label != "" {
		return account.Label
	}
	if account.Email != "" {
		return account.Email
	}
	if account.Login != "" {
		return account.Login
	}
	return account.ID
}

func handleIDEStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, idedb.Status())
}

func handleIDELogout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, idedb.Logout())
}

// handleProviderUnsupported answers the panel's polls for providers this build
// does not ship, so its UI settles instead of retrying forever.
func handleProviderUnsupported(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"status":  "error",
			"message": "The " + provider + " provider is not available in this build.",
		})
	}
}

func handleBundleGone(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusGone, map[string]any{
		"success": false,
		"error":   "Credential bundle export/import has been removed.",
	})
}

// handleAccountPing measures round-trip latency for one account by issuing a
// deliberately tiny completion.
func handleAccountPing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider  string `json:"provider"`
		AccountID string `json:"accountId"`
		ModelID   string `json:"modelId"`
	}
	if raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}

	if body.AccountID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "provider and accountId are required",
		})
		return
	}
	if body.Provider != "" && body.Provider != "antigravity" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Unsupported provider",
		})
		return
	}

	modelID := body.ModelID
	if modelID == "" {
		modelID = "gemini-3.7-flash-high"
	}

	start := time.Now()
	_, err := antigravity.CreateChatCompletion(r.Context(), antigravity.ChatRequest{
		Model: modelID,
		Messages: []antigravity.Message{{
			Role:    "user",
			Content: antigravity.MessageContent{IsString: true, Text: "ping"},
		}},
		MaxTokens: 1,
	}, antigravity.CallOptions{AccountID: body.AccountID})

	if err != nil {
		writeJSON(w, http.StatusOK, pingFailure(body.AccountID, modelID, err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"provider":  "antigravity",
		"accountId": body.AccountID,
		"modelId":   modelID,
		"latencyMs": time.Since(start).Milliseconds(),
	})
}

func handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if accountID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Account id is required",
		})
		return
	}

	removed := antigravity.Accounts.Remove(accountID)
	if !removed {
		// The account may exist only in the auth store (added out of band, or
		// with an expired token the manager never loaded).
		removed = authstore.Delete(accountID)
	}

	if !removed {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "error": "Account not found",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Account " + accountID + " removed",
	})
}

// handleAccountEnabled switches one account in or out of rotation.
//
// POST /accounts/{id}/enabled  {"enabled": false}
//
// This is a pause, not a delete: the credentials stay, so the account can be
// switched back on without signing in again.
func handleAccountEnabled(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if accountID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Account id is required",
		})
		return
	}

	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": `Body must be {"enabled": true|false}`,
		})
		return
	}

	if !antigravity.Accounts.SetEnabled(accountID, *body.Enabled) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "error": "Account not found",
		})
		return
	}

	state := "disabled"
	if *body.Enabled {
		state = "enabled"
	}
	// Report how many are left in rotation so the dashboard can warn when the
	// last one has just been switched off and requests will now fail.
	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"enabled":      *body.Enabled,
		"accountId":    accountID,
		"enabledCount": antigravity.Accounts.EnabledCount(),
		"message":      "Account " + accountID + " " + state,
	})
}

// handleAuthLoginPending exposes the sign-in URL of an in-flight OAuth attempt.
//
// POST /auth/login blocks until the user finishes in the browser, so it cannot
// carry the URL back itself. The dashboard polls this while it waits, and shows
// the link when the browser launch was suppressed or failed.
func handleAuthLoginPending(w http.ResponseWriter, r *http.Request) {
	url, opened, active := antigravity.PendingAuthURL()
	writeJSON(w, http.StatusOK, map[string]any{
		"active":        active,
		"url":           url,
		"browserOpened": opened,
	})
}
