package idedb

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"gravity-go/internal/apperr"
)

// writeTestDB builds a VS Code-shaped state.vscdb holding the given key/value
// pairs, so the reader can be exercised without a real Antigravity install.
func writeTestDB(t *testing.T, items map[string]string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for key, value := range items {
		if _, err := db.Exec(`INSERT INTO ItemTable (key, value) VALUES (?, ?)`, key, value); err != nil {
			t.Fatalf("insert %q: %v", key, err)
		}
	}
	return path
}

func TestReadTokenParsesAuthStatus(t *testing.T) {
	path := writeTestDB(t, map[string]string{
		"antigravityAuthStatus": `{"name":"Test User","apiKey":"ya29.test-token","email":"test@example.com"}`,
	})
	t.Setenv("GRAVITY_IDE_DB_PATH", path)

	token, err := ReadToken()
	if err != nil {
		t.Fatalf("ReadToken() error = %v", err)
	}
	if token.APIKey != "ya29.test-token" {
		t.Errorf("APIKey = %q", token.APIKey)
	}
	if token.Email != "test@example.com" {
		t.Errorf("Email = %q", token.Email)
	}
	if token.Name != "Test User" {
		t.Errorf("Name = %q", token.Name)
	}
}

func TestReadTokenMissingDatabase(t *testing.T) {
	t.Setenv("GRAVITY_IDE_DB_PATH", filepath.Join(t.TempDir(), "absent.vscdb"))

	_, err := ReadToken()
	if err == nil {
		t.Fatal("ReadToken() should fail when the database is absent")
	}

	var appErr *apperr.AntigravityError
	if !errors.As(err, &appErr) {
		t.Fatalf("error type = %T, want *apperr.AntigravityError", err)
	}
	if appErr.Code != "db_not_found" {
		t.Errorf("code = %q, want db_not_found", appErr.Code)
	}
}

func TestReadTokenMissingAuthRow(t *testing.T) {
	path := writeTestDB(t, map[string]string{"someOtherKey": "{}"})
	t.Setenv("GRAVITY_IDE_DB_PATH", path)

	_, err := ReadToken()
	if err == nil {
		t.Fatal("ReadToken() should fail when the auth row is absent")
	}

	var appErr *apperr.AntigravityError
	if !errors.As(err, &appErr) {
		t.Fatalf("error type = %T, want *apperr.AntigravityError", err)
	}
	if appErr.Code != "auth_not_found" {
		t.Errorf("code = %q, want auth_not_found", appErr.Code)
	}
}

// A signed-out IDE leaves the row in place with an empty apiKey.
func TestReadTokenEmptyAPIKey(t *testing.T) {
	path := writeTestDB(t, map[string]string{
		"antigravityAuthStatus": `{"name":"x","apiKey":"","email":"x@y.z"}`,
	})
	t.Setenv("GRAVITY_IDE_DB_PATH", path)

	_, err := ReadToken()
	if err == nil {
		t.Fatal("ReadToken() should fail on an empty apiKey")
	}

	var appErr *apperr.AntigravityError
	if !errors.As(err, &appErr) {
		t.Fatalf("error type = %T, want *apperr.AntigravityError", err)
	}
	if appErr.Code != "invalid_token" {
		t.Errorf("code = %q, want invalid_token", appErr.Code)
	}
}

func TestStatusReportsSignedIn(t *testing.T) {
	path := writeTestDB(t, map[string]string{
		"antigravityAuthStatus": `{"name":"Test User","apiKey":"tok","email":"test@example.com"}`,
	})
	t.Setenv("GRAVITY_IDE_DB_PATH", path)

	status := Status()
	if !status.LoggedIn {
		t.Fatal("LoggedIn = false, want true")
	}
	if status.Email == nil || *status.Email != "test@example.com" {
		t.Errorf("Email = %v", status.Email)
	}
	if status.Name == nil || *status.Name != "Test User" {
		t.Errorf("Name = %v", status.Name)
	}
}

func TestStatusReportsSignedOut(t *testing.T) {
	t.Setenv("GRAVITY_IDE_DB_PATH", filepath.Join(t.TempDir(), "absent.vscdb"))

	status := Status()
	if status.LoggedIn {
		t.Error("LoggedIn = true, want false with no database")
	}
	if status.Email != nil || status.Name != nil {
		t.Error("Email/Name should be nil when signed out")
	}
}

// The reader must not take a write lock: the IDE is normally running and holding
// the same file open.
func TestReadTokenWorksWhileDatabaseIsOpenElsewhere(t *testing.T) {
	path := writeTestDB(t, map[string]string{
		"antigravityAuthStatus": `{"name":"n","apiKey":"tok","email":"e@x.y"}`,
	})
	t.Setenv("GRAVITY_IDE_DB_PATH", path)

	// Hold a second connection open for the duration of the read.
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()
	if err := holder.Ping(); err != nil {
		t.Fatalf("ping holder: %v", err)
	}

	if _, err := ReadToken(); err != nil {
		t.Errorf("ReadToken() failed while another connection was open: %v", err)
	}
}

// Logout must clear every session key, not just the primary auth row: the
// extension restores its session from the unified-state-sync keys at launch.
func TestLogoutClearsAllSessionKeys(t *testing.T) {
	path := writeTestDB(t, map[string]string{
		"antigravityAuthStatus":                  `{"name":"n","apiKey":"tok","email":"e@x.y"}`,
		"antigravityUnifiedStateSync.oauthToken": `"token"`,
		"antigravityUnifiedStateSync.userStatus": `"status"`,
		// Removing this would relaunch the first-run wizard, so it must survive.
		"antigravityOnboarding": `{"done":true}`,
	})
	t.Setenv("GRAVITY_IDE_DB_PATH", path)

	result := Logout()
	if !result.Success {
		t.Fatalf("Logout() failed: %s", result.Error)
	}
	if result.PreviousEmail == nil || *result.PreviousEmail != "e@x.y" {
		t.Errorf("PreviousEmail = %v, want the signed-in address", result.PreviousEmail)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	for _, key := range ideAuthKeys {
		var value string
		err := db.QueryRow("SELECT value FROM ItemTable WHERE key = ?", key).Scan(&value)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("key %q survived logout", key)
		}
	}

	var onboarding string
	if err := db.QueryRow(
		"SELECT value FROM ItemTable WHERE key = ?", "antigravityOnboarding",
	).Scan(&onboarding); err != nil {
		t.Errorf("antigravityOnboarding was removed; it must survive logout: %v", err)
	}
}

func TestLogoutReportsMissingDatabase(t *testing.T) {
	t.Setenv("GRAVITY_IDE_DB_PATH", filepath.Join(t.TempDir(), "absent.vscdb"))

	result := Logout()
	if result.Success {
		t.Error("Logout() = success, want failure with no database")
	}
	if result.Error == "" {
		t.Error("Logout() should explain why it failed")
	}
}
