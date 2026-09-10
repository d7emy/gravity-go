package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"gravity-go/internal/appstate"
	"gravity-go/internal/logx"
	"gravity-go/internal/paths"
)

// authData is the on-disk shape of auth.json, matching the TypeScript build so
// an existing ~/.anti-api/auth.json is picked up as-is.
type authData struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	UserEmail    string `json:"userEmail,omitempty"`
	UserName     string `json:"userName,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
	ProjectID    string `json:"projectId,omitempty"`
}

func authFile() string { return paths.DataFile("auth.json") }

// legacyAuthFile is the old in-project data directory.
func legacyAuthFile() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Join(cwd, "data", "auth.json")
}

var authFileMu sync.Mutex

// InitAuth loads saved credentials into process state at startup.
func InitAuth() {
	authFileMu.Lock()
	defer authFileMu.Unlock()

	source := authFile()
	if _, err := os.Stat(source); err != nil {
		legacy := legacyAuthFile()
		if legacy == "" {
			return
		}
		if _, err := os.Stat(legacy); err != nil {
			return
		}
		source = legacy
	}

	raw, err := os.ReadFile(source)
	if err != nil {
		logx.Warn("Failed to load saved auth: %v", err)
		return
	}
	var data authData
	if err := json.Unmarshal(raw, &data); err != nil {
		logx.Warn("Failed to parse saved auth: %v", err)
		return
	}
	if data.AccessToken == "" {
		return
	}

	appstate.SetAuth(appstate.Auth{
		AccessToken:    data.AccessToken,
		RefreshToken:   data.RefreshToken,
		TokenExpiresAt: data.ExpiresAt,
		UserEmail:      data.UserEmail,
		UserName:       data.UserName,
		ProjectID:      data.ProjectID,
	})

	// Migrate a legacy file forward on first sight.
	if source != authFile() {
		saveAuthLocked()
	}
	logx.Success("Loaded saved authentication")
}

// SaveAuth persists the current process credentials.
func SaveAuth() {
	authFileMu.Lock()
	defer authFileMu.Unlock()
	saveAuthLocked()
}

func saveAuthLocked() {
	if _, err := paths.EnsureDataDir(); err != nil {
		logx.Error("Failed to prepare data dir: %v", err)
		return
	}
	auth := appstate.GetAuth()
	payload, err := json.MarshalIndent(authData{
		AccessToken:  auth.AccessToken,
		RefreshToken: auth.RefreshToken,
		ExpiresAt:    auth.TokenExpiresAt,
		UserEmail:    auth.UserEmail,
		UserName:     auth.UserName,
		ProjectID:    auth.ProjectID,
	}, "", "  ")
	if err != nil {
		logx.Error("Failed to encode auth: %v", err)
		return
	}
	// 0600: this file holds a refresh token.
	if err := os.WriteFile(authFile(), payload, 0o600); err != nil {
		logx.Error("Failed to save auth: %v", err)
	}
}

// ClearAuth wipes credentials from memory and disk.
func ClearAuth() {
	appstate.ClearAuth()

	authFileMu.Lock()
	defer authFileMu.Unlock()
	for _, path := range []string{authFile(), legacyAuthFile()} {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			logx.Warn("Failed to clear auth file %s: %v", path, err)
		}
	}
}

// SetAuth stores an externally supplied token pair.
func SetAuth(accessToken, refreshToken, email, name string) {
	appstate.SetAuth(appstate.Auth{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		UserEmail:    email,
		UserName:     name,
		ProjectID:    appstate.ProjectID(),
	})
	SaveAuth()
}

// callbackResult is what the loopback handler captured.
type callbackResult struct {
	Code  string
	State string
	Err   string
}

// startCallbackServer binds a loopback listener for the OAuth redirect,
// scanning forward from the default port if it is taken.
func startCallbackServer() (srv *http.Server, port int, results <-chan callbackResult, err error) {
	var listener net.Listener
	for offset := 0; offset <= 10; offset++ {
		candidate := oauthCallbackPort + offset
		l, lerr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", candidate))
		if lerr == nil {
			listener = l
			port = candidate
			break
		}
		if offset == 10 {
			return nil, 0, nil, fmt.Errorf("no free callback port in %d-%d: %w",
				oauthCallbackPort, oauthCallbackPort+10, lerr)
		}
	}

	ch := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		select {
		case ch <- callbackResult{
			Code:  q.Get("code"),
			State: q.Get("state"),
			Err:   q.Get("error"),
		}:
		default:
		}
		http.Redirect(w, r, "https://antigravity.google/auth-success", http.StatusFound)
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			logx.Debug("OAuth callback server stopped: %v", serveErr)
		}
	}()
	return server, port, ch, nil
}

// browserCommand builds the platform command that opens target, without running
// it. Construction is split from execution so the policy can be tested without
// actually launching a browser.
//
// It deliberately consults no "no open" environment variable. Those flags belong
// to the caller: GRAVITY_NO_OPEN suppresses the dashboard auto-open at startup,
// GRAVITY_OAUTH_NO_OPEN suppresses the sign-in window. Checking the dashboard
// flag in here silently broke sign-in for anyone running with GRAVITY_NO_OPEN=1.
func browserCommand(target string) *exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target)
	case "windows":
		// The empty title argument stops `start` treating the URL as a title.
		return exec.Command("cmd", "/c", "start", "", target)
	default:
		return exec.Command("xdg-open", target)
	}
}

// OpenBrowser launches the system browser and reports whether the launch was
// handed off successfully.
func OpenBrowser(target string) bool {
	cmd := browserCommand(target)
	if err := cmd.Start(); err != nil {
		logx.Warn("Could not open a browser automatically: %v", err)
		return false
	}
	// Reap the child rather than leaving a zombie.
	go func() { _ = cmd.Wait() }()
	return true
}

// pendingAuth holds the sign-in URL of an in-flight OAuth attempt so the
// dashboard can show it. Browser launching is unreliable — wrong default
// browser, headless session, sandboxed shell — and without the URL on screen a
// failed launch leaves the user with nothing to act on.
var pendingAuth struct {
	mu        sync.Mutex
	url       string
	startedAt time.Time
	opened    bool
}

// PendingAuthURL returns the in-flight sign-in URL, whether the browser launch
// succeeded, and whether an attempt is active at all.
func PendingAuthURL() (url string, browserOpened bool, active bool) {
	pendingAuth.mu.Lock()
	defer pendingAuth.mu.Unlock()
	if pendingAuth.url == "" {
		return "", false, false
	}
	// The flow times out after five minutes; do not advertise a stale URL.
	if time.Since(pendingAuth.startedAt) > 5*time.Minute {
		return "", false, false
	}
	return pendingAuth.url, pendingAuth.opened, true
}

func setPendingAuth(url string, opened bool) {
	pendingAuth.mu.Lock()
	defer pendingAuth.mu.Unlock()
	pendingAuth.url = url
	pendingAuth.opened = opened
	pendingAuth.startedAt = time.Now()
}

func clearPendingAuth() {
	pendingAuth.mu.Lock()
	defer pendingAuth.mu.Unlock()
	pendingAuth.url = ""
	pendingAuth.opened = false
}

// LoginResult reports the outcome of an interactive login.
type LoginResult struct {
	Success bool
	Email   string
	Error   string
}

// StartOAuthLogin runs the full loopback OAuth flow and persists the result.
func StartOAuthLogin(ctx context.Context) LoginResult {
	server, port, results, err := startCallbackServer()
	if err != nil {
		return LoginResult{Error: err.Error()}
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	state := GenerateState()
	redirectURI := strings.TrimSpace(os.Getenv("GRAVITY_OAUTH_REDIRECT_URL"))
	if redirectURI == "" {
		redirectURI = fmt.Sprintf("http://localhost:%d/oauth-callback", port)
	}
	authURL := GenerateAuthURL(redirectURI, state)

	logx.Info("Open this URL to sign in: %s", authURL)

	opened := false
	if os.Getenv("GRAVITY_OAUTH_NO_OPEN") != "1" && os.Getenv("ANTI_API_OAUTH_NO_OPEN") != "1" {
		opened = OpenBrowser(authURL)
	}
	// Publish it either way, so the dashboard can offer the link when the
	// launch was suppressed or failed.
	setPendingAuth(authURL, opened)
	defer clearPendingAuth()

	// Google's consent screen is human-paced; five minutes matches the original.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var cb callbackResult
	select {
	case cb = <-results:
	case <-waitCtx.Done():
		return LoginResult{Error: "authentication timed out after 5 minutes"}
	}

	if cb.Err != "" {
		return LoginResult{Error: cb.Err}
	}
	if cb.Code == "" || cb.State == "" {
		return LoginResult{Error: "missing code or state in callback"}
	}
	if cb.State != state {
		return LoginResult{Error: "state mismatch - possible CSRF attack"}
	}

	tokens, err := ExchangeCode(ctx, cb.Code, redirectURI)
	if err != nil {
		return LoginResult{Error: err.Error()}
	}

	email, err := FetchUserInfo(ctx, tokens.AccessToken)
	if err != nil {
		return LoginResult{Error: err.Error()}
	}

	projectID := GetProjectID(ctx, tokens.AccessToken)
	if projectID == "" {
		projectID = GenerateMockProjectID()
		logx.Warn("No project ID returned, using fallback: %s", projectID)
	}

	name := email
	if idx := strings.Index(email, "@"); idx > 0 {
		name = email[:idx]
	}

	appstate.SetAuth(appstate.Auth{
		AccessToken:    tokens.AccessToken,
		RefreshToken:   tokens.RefreshToken,
		TokenExpiresAt: time.Now().UnixMilli() + tokens.ExpiresIn*1000,
		UserEmail:      email,
		UserName:       name,
		ProjectID:      projectID,
	})
	SaveAuth()

	logx.Success("✓ Login successful: %s", email)
	logx.Success("✓ Project ID: %s", projectID)
	return LoginResult{Success: true, Email: email}
}
