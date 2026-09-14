// Package idedb reads and edits the local Antigravity IDE's credential store.
//
// Antigravity is VS Code-derived: global state lives in a SQLite file
// (state.vscdb) as a key/value ItemTable. We use a pure-Go driver so the binary
// stays static and cross-compiles without a C toolchain.
package idedb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"gravity-go/internal/apperr"
	"gravity-go/internal/logx"
	"gravity-go/internal/paths"
)

// authStatus is the JSON stored under the antigravityAuthStatus key.
type authStatus struct {
	Name   string `json:"name"`
	APIKey string `json:"apiKey"`
	Email  string `json:"email"`
}

// IDE session keys cleared on logout.
//
// Deliberately excludes antigravityOnboarding: removing that key relaunches the
// full first-run wizard. Clearing these three is enough for the extension to
// detect "no token" and show the Log in button.
var ideAuthKeys = []string{
	"antigravityAuthStatus",
	"antigravityUnifiedStateSync.oauthToken",
	"antigravityUnifiedStateSync.userStatus",
}

// openReadOnly opens state.vscdb without taking a write lock, so it works while
// the IDE is running.
func openReadOnly(path string) (*sql.DB, error) {
	// modernc's driver takes SQLite URI query params.
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(3000)", path)
	return sql.Open("sqlite", dsn)
}

func openReadWrite(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(3000)", path)
	return sql.Open("sqlite", dsn)
}

func readItem(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow("SELECT value FROM ItemTable WHERE key = ?", key).Scan(&value)
	return value, err
}

// Token is the credential read out of the IDE database.
type Token struct {
	APIKey string
	Email  string
	Name   string
}

// ReadToken pulls the OAuth token the Antigravity IDE stored locally. This is
// the fallback path when no gravity-go OAuth login has been performed.
func ReadToken() (Token, error) {
	dbPath := paths.IDEDBPath()
	if _, err := os.Stat(dbPath); err != nil {
		return Token{}, apperr.NewAntigravity(
			"Antigravity IDE data not found. Install and sign in to Antigravity first.",
			"db_not_found",
		)
	}

	db, err := openReadOnly(dbPath)
	if err != nil {
		return Token{}, apperr.NewAntigravity("Failed to open Antigravity database: "+err.Error(), "db_error")
	}
	defer db.Close()

	value, err := readItem(db, "antigravityAuthStatus")
	if err == sql.ErrNoRows {
		return Token{}, apperr.NewAntigravity(
			"Antigravity auth record not found. Sign in to the Antigravity app first.",
			"auth_not_found",
		)
	}
	if err != nil {
		return Token{}, apperr.NewAntigravity("Failed to read Antigravity token: "+err.Error(), "db_error")
	}

	var status authStatus
	if err := json.Unmarshal([]byte(value), &status); err != nil {
		return Token{}, apperr.NewAntigravity("Failed to parse Antigravity auth record: "+err.Error(), "db_error")
	}
	if status.APIKey == "" {
		return Token{}, apperr.NewAntigravity(
			"Antigravity token is empty. Sign in to the Antigravity app again.",
			"invalid_token",
		)
	}

	return Token{APIKey: status.APIKey, Email: status.Email, Name: status.Name}, nil
}

// AuthInfo is the IDE's current sign-in state.
type AuthInfo struct {
	LoggedIn bool    `json:"loggedIn"`
	Email    *string `json:"email"`
	Name     *string `json:"name"`
}

// Status reports whether the IDE currently holds a session.
func Status() AuthInfo {
	out := AuthInfo{}
	token, err := ReadToken()
	if err != nil {
		return out
	}
	out.LoggedIn = true
	if token.Email != "" {
		email := token.Email
		out.Email = &email
	}
	if token.Name != "" {
		name := token.Name
		out.Name = &name
	}
	return out
}

// LogoutResult reports the outcome of clearing the IDE session.
type LogoutResult struct {
	Success       bool    `json:"success"`
	PreviousEmail *string `json:"previousEmail"`
	IDEWasRunning bool    `json:"ideWasRunning"`
	Error         string  `json:"error,omitempty"`
}

// isIDERunning reports whether an Antigravity process is alive.
func isIDERunning() bool {
	switch runtime.GOOS {
	case "windows":
		// tasklist filters are matched case-insensitively on the image name.
		out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq Antigravity.exe", "/NH").Output()
		if err != nil {
			return false
		}
		return strings.Contains(strings.ToLower(string(out)), "antigravity.exe")
	case "darwin":
		out, err := exec.Command("pgrep", "-f", "Antigravity").Output()
		return err == nil && len(strings.TrimSpace(string(out))) > 0
	default:
		out, err := exec.Command("pgrep", "-f", "antigravity").Output()
		return err == nil && len(strings.TrimSpace(string(out))) > 0
	}
}

// closeIDE asks Antigravity to exit gracefully and waits for it.
//
// The IDE reads state.vscdb into memory at launch and writes it back on exit,
// so clearing the DB under a running IDE would just be overwritten.
func closeIDE() (closed bool, wasRunning bool, err error) {
	if !isIDERunning() {
		return true, false, nil
	}

	switch runtime.GOOS {
	case "windows":
		// No /F: let it shut down cleanly so it flushes state to the DB.
		_ = exec.Command("taskkill", "/IM", "Antigravity.exe").Run()
	case "darwin":
		_ = exec.Command("osascript", "-e", `quit app "Antigravity"`).Run()
	default:
		_ = exec.Command("pkill", "-TERM", "-f", "antigravity").Run()
	}

	// Poll for up to 15s, then give the DB a moment to flush.
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		if !isIDERunning() {
			time.Sleep(time.Second)
			return true, true, nil
		}
	}
	return false, true, fmt.Errorf("Antigravity IDE did not exit within 15 seconds. Try closing it manually.")
}

// Logout closes the IDE and clears its stored session keys.
func Logout() LogoutResult {
	dbPath := paths.IDEDBPath()
	if _, err := os.Stat(dbPath); err != nil {
		return LogoutResult{
			Error: "Antigravity IDE database not found. Is Antigravity installed?",
		}
	}

	// Capture the signed-in identity before we tear the session down.
	current := Status()

	closed, wasRunning, closeErr := closeIDE()
	if !closed {
		msg := "Failed to close Antigravity IDE"
		if closeErr != nil {
			msg = closeErr.Error()
		}
		return LogoutResult{PreviousEmail: current.Email, IDEWasRunning: wasRunning, Error: msg}
	}

	db, err := openReadWrite(dbPath)
	if err != nil {
		return LogoutResult{
			PreviousEmail: current.Email,
			IDEWasRunning: wasRunning,
			Error:         "Failed to open IDE database: " + err.Error(),
		}
	}
	defer db.Close()

	for _, key := range ideAuthKeys {
		if _, err := db.Exec("DELETE FROM ItemTable WHERE key = ?", key); err != nil {
			return LogoutResult{
				PreviousEmail: current.Email,
				IDEWasRunning: wasRunning,
				Error:         "Failed to clear IDE auth: " + err.Error(),
			}
		}
	}

	if current.Email != nil {
		logx.Success("IDE session logged out: %s", *current.Email)
	} else {
		logx.Success("IDE session cleared")
	}

	return LogoutResult{Success: true, PreviousEmail: current.Email, IDEWasRunning: wasRunning}
}
