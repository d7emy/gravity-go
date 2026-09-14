// Command gravity-go proxies Antigravity's built-in models as an
// Anthropic-compatible (and OpenAI-compatible) local API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gravity-go/internal/antigravity"
	"gravity-go/internal/appstate"
	"gravity-go/internal/idedb"
	"gravity-go/internal/logbuf"
	"gravity-go/internal/logx"
	"gravity-go/internal/server"
	"gravity-go/internal/settings"
	"gravity-go/internal/usage"
)

const defaultPort = 8964

func main() {
	// Running with no arguments starts the server. Serving is what this binary
	// is for, and it is what `go run .` passes -- printing usage and exiting 1
	// made the most obvious way to run the project fail. Every named command
	// still behaves exactly as before.
	command, args := "start", []string{}
	if len(os.Args) > 1 {
		command, args = os.Args[1], os.Args[2:]
	}

	switch command {
	case "start":
		os.Exit(cmdStart(args))
	case "login", "add-account":
		os.Exit(cmdLogin(args))
	case "accounts":
		os.Exit(cmdAccounts())
	case "logout-ide":
		os.Exit(cmdLogoutIDE())
	case "version", "--version", "-v":
		fmt.Println("gravity-go " + server.Version)
	case "help", "--help", "-h":
		usageText()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		usageText()
		os.Exit(1)
	}
}

func usageText() {
	fmt.Print(`gravity-go - Antigravity API proxy

Usage:
  gravity-go                        Start the proxy server (same as "start")
  gravity-go start [-p PORT] [-v]   Start the proxy server
  gravity-go login                  Add a Google account via browser OAuth
  gravity-go accounts               List configured accounts
  gravity-go logout-ide             Sign the local Antigravity IDE out
  gravity-go version                Print the version

Environment:
  GRAVITY_DATA_DIR                  Override the data directory (~/.gravity-go)
  GRAVITY_IDE_DB_PATH               Override the Antigravity state.vscdb path
  GRAVITY_HOST                      Bind address (default 127.0.0.1)
  GRAVITY_ACCOUNT_CONCURRENCY       Requests in flight per account (default 1)
  GRAVITY_INSECURE_TLS=1            Skip TLS verification (TLS-inspecting proxies)
  GRAVITY_NO_OPEN=1                 Do not auto-open the dashboard at startup
  GRAVITY_OAUTH_NO_OPEN=1           Do not open a browser for sign-in
`)
}

// bootstrap loads credentials and refreshes the project id. Shared by the
// commands that need a working account.
func bootstrap(ctx context.Context) {
	antigravity.InitAuth()

	// No saved OAuth session: fall back to the token the local IDE holds.
	if !appstate.IsAuthenticated() {
		logx.Debug("No saved OAuth session, trying the local Antigravity IDE...")
		token, err := idedb.ReadToken()
		if err != nil {
			logx.Debug("Failed to read token from IDE: %v", err)
		} else {
			appstate.SetIDEToken(token.APIKey, token.Email, token.Name)
			logx.Debug("Loaded token from Antigravity IDE (%s)", token.Email)
		}
	}

	// Keep the project id current so quota and billing attribute correctly.
	if appstate.IsAuthenticated() {
		if projectID := antigravity.GetProjectID(ctx, appstate.AccessToken()); projectID != "" &&
			projectID != appstate.ProjectID() {
			appstate.SetProjectID(projectID)
			antigravity.SaveAuth()
			logx.Debug("Project ID refreshed: %s", projectID)
		}
	}

	antigravity.Accounts.Load()
}

func cmdStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	port := fs.Int("port", defaultPort, "listen port")
	fs.IntVar(port, "p", defaultPort, "listen port (shorthand)")
	verbose := fs.Bool("verbose", false, "verbose logging")
	fs.BoolVar(verbose, "v", false, "verbose logging (shorthand)")
	_ = fs.Parse(args)

	if envPort := strings.TrimSpace(os.Getenv("GRAVITY_PORT")); envPort != "" && !flagPassed(fs, "port", "p") {
		if parsed, err := strconv.Atoi(envPort); err == nil {
			*port = parsed
		}
	}

	appstate.SetPort(*port)
	appstate.SetVerbose(*verbose)
	logx.SetVerbose(*verbose)

	usage.Load()
	appSettings := settings.Load()
	logbuf.SetEnabled(appSettings.CaptureLogs)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bootstrap(ctx)

	logx.LogStartup()

	host := strings.TrimSpace(os.Getenv("GRAVITY_HOST"))
	if host == "" {
		host = strings.TrimSpace(os.Getenv("ANTI_API_HOST"))
	}
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(*port))

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           server.New(),
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: a streaming completion can legitimately run for
		// many minutes and a deadline here would cut it off mid-response.
		IdleTimeout: 120 * time.Second,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		logx.Error("Failed to bind %s: %v", addr, err)
		return 1
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	logx.LogStartupSuccess(*port)

	// GRAVITY_NO_OPEN suppresses only this dashboard auto-open. Sign-in has its
	// own flag (GRAVITY_OAUTH_NO_OPEN) so a headless-ish setup can still log in.
	noOpen := os.Getenv("GRAVITY_NO_OPEN") == "1" || os.Getenv("ANTI_API_NO_OPEN") == "1"
	if appSettings.AutoOpenDashboard && !noOpen {
		antigravity.OpenBrowser(fmt.Sprintf("http://localhost:%d/quota", *port))
	}

	select {
	case err := <-serveErr:
		if err != nil {
			logx.Error("Server error: %v", err)
			return 1
		}
	case <-ctx.Done():
		logx.Info("Shutting down...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)

	// Flush both debounced stores: signatures so resumed sessions keep working,
	// usage so the last few seconds of tokens are not lost.
	antigravity.FlushToolCallSignatures()
	usage.Flush()
	return 0
}

// flagPassed reports whether any of the given flag names was set explicitly.
func flagPassed(fs *flag.FlagSet, names ...string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		for _, name := range names {
			if f.Name == name {
				seen = true
			}
		}
	})
	return seen
}

func cmdLogin(args []string) int {
	_ = args

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logx.SetVerbose(true)
	antigravity.InitAuth()
	antigravity.Accounts.Load()

	if existing := antigravity.Accounts.Emails(); len(existing) > 0 {
		logx.Info("Existing accounts (%d):", len(existing))
		for i, email := range existing {
			logx.Info("  %d. %s", i+1, email)
		}
	}
	logx.Info("Adding a new account. Multiple accounts rotate when quota runs out.")

	result := antigravity.StartOAuthLogin(ctx)
	if !result.Success {
		logx.Error("Failed to add account: %s", result.Error)
		return 1
	}

	auth := appstate.GetAuth()
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

	logx.Success("Account added: %s", result.Email)
	logx.Info("Now %d account(s) available for rotation", antigravity.Accounts.Count())
	return 0
}

func cmdAccounts() int {
	antigravity.InitAuth()
	antigravity.Accounts.Load()

	emails := antigravity.Accounts.Emails()
	if len(emails) == 0 {
		fmt.Println("No accounts added yet.")
		fmt.Println("Run `gravity-go login` to add one.")
		return 0
	}

	fmt.Printf("Accounts (%d):\n", len(emails))
	for i, email := range emails {
		fmt.Printf("  %d. %s\n", i+1, email)
	}
	return 0
}

func cmdLogoutIDE() int {
	logx.SetVerbose(true)

	current := idedb.Status()
	if current.LoggedIn && current.Email != nil {
		logx.Info("Current IDE account: %s", *current.Email)
	} else {
		logx.Info("IDE is not logged in")
	}

	result := idedb.Logout()
	if !result.Success {
		logx.Error("Logout failed: %s", result.Error)
		return 1
	}

	if result.PreviousEmail != nil {
		logx.Success("Logged out: %s", *result.PreviousEmail)
	} else {
		logx.Success("IDE session cleared")
	}
	logx.Info("Open Antigravity manually to sign in with a different account.")
	return 0
}
