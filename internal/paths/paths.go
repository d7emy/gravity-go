// Package paths resolves the on-disk locations gravity-go reads and writes.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DataDir is where accounts, auth, settings and caches live.
// Override with GRAVITY_DATA_DIR (ANTI_API_DATA_DIR still honoured so an
// existing setup can be pointed at directly).
//
// Fresh installs use ~/.gravity-go. If that does not exist yet but a legacy
// ~/.anti-api directory does, the legacy one is used as-is so existing
// accounts keep working with no migration step. Once ~/.gravity-go exists it
// always wins; to migrate, move the files over.
func DataDir() string {
	for _, key := range []string{"GRAVITY_DATA_DIR", "ANTI_API_DATA_DIR"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	home := homeDir()
	gravityDir := filepath.Join(home, ".gravity-go")
	if _, err := os.Stat(gravityDir); err == nil {
		return gravityDir
	}
	if _, err := os.Stat(filepath.Join(home, ".anti-api")); err == nil {
		return filepath.Join(home, ".anti-api")
	}
	return gravityDir
}

// EnsureDataDir creates the data directory if it does not exist.
func EnsureDataDir() (string, error) {
	dir := DataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir, err
	}
	return dir, nil
}

// DataFile joins a filename onto the data directory.
func DataFile(name string) string {
	return filepath.Join(DataDir(), name)
}

// AuthDir holds one JSON file per provider account.
func AuthDir() string {
	return filepath.Join(DataDir(), "auth")
}

func homeDir() string {
	for _, key := range []string{"HOME", "USERPROFILE"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

// IDEUserDataDir is Antigravity's VS Code-style userData directory.
//
//	macOS   ~/Library/Application Support/Antigravity
//	Windows %APPDATA%\Antigravity
//	Linux   ~/.config/Antigravity
func IDEUserDataDir() string {
	home := homeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Antigravity")
	case "windows":
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			return filepath.Join(appData, "Antigravity")
		}
		return filepath.Join(home, "AppData", "Roaming", "Antigravity")
	default:
		return filepath.Join(home, ".config", "Antigravity")
	}
}

// IDEDBPath is Antigravity's globalStorage SQLite database.
// Override with GRAVITY_IDE_DB_PATH / ANTI_API_IDE_DB_PATH for portable installs.
func IDEDBPath() string {
	for _, key := range []string{"GRAVITY_IDE_DB_PATH", "ANTI_API_IDE_DB_PATH"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return filepath.Join(IDEUserDataDir(), "User", "globalStorage", "state.vscdb")
}
