package antigravity

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"gravity-go/internal/appstate"
)

// OAuth client credentials.
//
// These are the public "installed application" credentials shipped inside the
// upstream client, not confidential secrets. They are required for the loopback
// OAuth flow and secret-scanning alerts on them are expected — see the project
// notes. Do not rotate or remove.
const (
	oauthClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	oauthClientSecret = "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"
	oauthCallbackPort = 51121

	oauthAuthURL = "https://accounts.google.com/o/oauth2/v2/auth"
)

// Endpoint URLs are vars, not consts, so tests can point them at a local stub.
// They are never reassigned in production.
var (
	oauthTokenURL    = "https://oauth2.googleapis.com/token"
	oauthUserInfoURL = "https://www.googleapis.com/oauth2/v1/userinfo?alt=json"
	oauthProjectURL  = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
)

var oauthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

const defaultIDEVersion = "1.15.8"

// IDEVersion is the Antigravity client version we present upstream.
//
// The version number itself stays pinned to a known-good IDE release: it must
// match what Google has actually shipped, or behaviour-gating mis-routes the
// call. Differentiation lives in transport and timing, not here. Both the
// GRAVITY_ and ANTIGRAVITY_ env names pin it; GRAVITY_ wins when both are set.
func IDEVersion() string {
	for _, key := range []string{"GRAVITY_IDE_VERSION", "ANTIGRAVITY_IDE_VERSION"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return defaultIDEVersion
}

// normalizeUpstreamPlatform maps Go runtime names onto the names the IDE
// reports. Only darwin differs (macos); everything else passes through
// lower-cased so exotic GOOS values cannot leak a build-machine fingerprint.
func normalizeUpstreamPlatform(goos string) string {
	if strings.EqualFold(strings.TrimSpace(goos), "darwin") {
		return "macos"
	}
	return strings.ToLower(strings.TrimSpace(goos))
}

// UserAgent is the client string sent upstream. Google keys behaviour off this,
// so it must look like the real IDE — never a custom product token.
//
// A full override is supported for debugging via GRAVITY_USER_AGENT (then
// ANTIGRAVITY_USER_AGENT); otherwise the value is derived from IDEVersion plus
// the normalized platform/arch. Header and body copies must always agree, so
// every caller goes through this function (see setUpstreamHeaders).
func UserAgent() string {
	for _, key := range []string{"GRAVITY_USER_AGENT", "ANTIGRAVITY_USER_AGENT"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	platform := normalizeUpstreamPlatform(runtime.GOOS)
	arch := strings.ToLower(strings.TrimSpace(runtime.GOARCH))
	return fmt.Sprintf("antigravity/%s %s/%s", IDEVersion(), platform, arch)
}

// Tokens is an OAuth token pair.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
}

// GenerateState returns a random CSRF state value.
func GenerateState() string {
	return randomHex(16)
}

// randomHex returns n cryptographically random bytes as hex.
//
// It fails closed. This backs the OAuth CSRF state parameter, and a predictable
// state defeats the protection entirely — an attacker who can guess it can forge
// the callback. crypto/rand.Read does not fail on any supported platform, so the
// panic is unreachable in practice; if entropy ever were unavailable, refusing to
// log in is the correct outcome, not issuing a guessable token.
func randomHex(n int) string {
	const hexdigits = "0123456789abcdef"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("gravity-go: crypto/rand unavailable, refusing to issue a guessable CSRF state: " + err.Error())
	}
	out := make([]byte, n*2)
	for i, b := range buf {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0x0f]
	}
	return string(out)
}

// GenerateAuthURL builds the Google consent URL.
func GenerateAuthURL(redirectURI, state string) string {
	params := url.Values{}
	params.Set("client_id", oauthClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", strings.Join(oauthScopes, " "))
	params.Set("access_type", "offline")
	params.Set("prompt", "consent")
	params.Set("state", state)
	return oauthAuthURL + "?" + params.Encode()
}

func postForm(ctx context.Context, endpoint string, form url.Values) (jsonResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return jsonResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Even the OAuth endpoints present the IDE agent: mixing a custom token
	// here with the IDE string on the data plane splits one client into two
	// easily correlated fingerprints.
	setUpstreamAgent(req)
	return doJSON(req)
}

// ExchangeCode swaps an authorization code for tokens.
func ExchangeCode(ctx context.Context, code, redirectURI string) (Tokens, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", oauthClientID)
	form.Set("client_secret", oauthClientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")

	resp, err := postForm(ctx, oauthTokenURL, form)
	if err != nil {
		return Tokens{}, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return Tokens{}, fmt.Errorf("token exchange failed: %d %s", resp.Status, resp.Body)
	}

	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &data); err != nil {
		return Tokens{}, fmt.Errorf("token exchange returned unreadable JSON: %w", err)
	}
	return Tokens{
		AccessToken:  data.AccessToken,
		RefreshToken: data.RefreshToken,
		ExpiresIn:    data.ExpiresIn,
	}, nil
}

// RefreshAccessToken exchanges a refresh token for a fresh access token.
func RefreshAccessToken(ctx context.Context, refreshToken string) (Tokens, error) {
	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("client_secret", oauthClientSecret)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")

	resp, err := postForm(ctx, oauthTokenURL, form)
	if err != nil {
		return Tokens{}, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return Tokens{}, fmt.Errorf("token refresh failed: %d %s", resp.Status, resp.Body)
	}

	var data struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &data); err != nil {
		return Tokens{}, fmt.Errorf("token refresh returned unreadable JSON: %w", err)
	}
	return Tokens{AccessToken: data.AccessToken, ExpiresIn: data.ExpiresIn}, nil
}

// FetchUserInfo reads the signed-in account's email.
func FetchUserInfo(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oauthUserInfoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	setUpstreamAgent(req)

	resp, err := doJSON(req)
	if err != nil {
		return "", err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return "", fmt.Errorf("failed to get user info: %d", resp.Status)
	}

	var data struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &data); err != nil {
		return "", err
	}
	return data.Email, nil
}

// GetProjectID resolves the cloudaicompanion project for an account. It returns
// an empty string rather than an error: callers fall back to a generated id.
func GetProjectID(ctx context.Context, accessToken string) string {
	payload := `{"metadata":{"ideType":"ANTIGRAVITY"}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthProjectURL,
		strings.NewReader(payload))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	setUpstreamAgent(req)
	setUpstreamLocale(req)

	resp, err := doJSON(req)
	if err != nil || resp.Status < 200 || resp.Status >= 300 {
		return ""
	}

	var data struct {
		CloudaicompanionProject string `json:"cloudaicompanionProject"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &data); err != nil {
		return ""
	}
	return data.CloudaicompanionProject
}

// tokenRefreshMu serializes refreshes of the single process-level token so a
// burst of requests cannot fire concurrent refreshes for the same credential.
var tokenRefreshMu sync.Mutex

// GetAccessToken returns the process-level token, refreshing it when it is
// within five minutes of expiry. This is the single-account path used when no
// managed accounts exist.
func GetAccessToken(ctx context.Context) (string, error) {
	auth := appstate.GetAuth()
	if auth.AccessToken == "" {
		return "", fmt.Errorf("not authenticated: run `gravity-go login` first")
	}

	nowMs := time.Now().UnixMilli()
	needsRefresh := auth.TokenExpiresAt > 0 && nowMs > auth.TokenExpiresAt-5*60*1000
	if !needsRefresh || auth.RefreshToken == "" {
		return auth.AccessToken, nil
	}

	tokenRefreshMu.Lock()
	defer tokenRefreshMu.Unlock()

	// Another goroutine may have refreshed while we waited for the lock.
	auth = appstate.GetAuth()
	nowMs = time.Now().UnixMilli()
	if !(auth.TokenExpiresAt > 0 && nowMs > auth.TokenExpiresAt-5*60*1000) {
		return auth.AccessToken, nil
	}

	tokens, err := RefreshAccessToken(ctx, auth.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("token expired and refresh failed, please re-login: %w", err)
	}
	appstate.SetTokens(tokens.AccessToken, nowMs+tokens.ExpiresIn*1000)
	SaveAuth()
	return tokens.AccessToken, nil
}

// mock project ids stand in when loadCodeAssist returns nothing.
var (
	mockAdjectives = []string{"useful", "bright", "swift", "calm", "bold"}
	mockNouns      = []string{"fuze", "wave", "spark", "flow", "core"}
)

// GenerateMockProjectID builds a plausible fallback project id.
func GenerateMockProjectID() string {
	const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
	pick := func(list []string) string {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
		if err != nil {
			return list[0]
		}
		return list[v.Int64()]
	}
	suffix := make([]byte, 5)
	for i := range suffix {
		v, err := rand.Int(rand.Reader, big.NewInt(36))
		if err != nil {
			suffix[i] = base36[i%36]
			continue
		}
		suffix[i] = base36[v.Int64()]
	}
	return fmt.Sprintf("%s-%s-%s", pick(mockAdjectives), pick(mockNouns), string(suffix))
}
