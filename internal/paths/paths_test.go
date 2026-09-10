package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// pointHomeAt redirects homeDir() at a temp dir on every platform.
func pointHomeAt(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("GRAVITY_DATA_DIR", "")
	t.Setenv("ANTI_API_DATA_DIR", "")
}

func TestDataDirDefaultsToGravityGo(t *testing.T) {
	pointHomeAt(t, t.TempDir())

	if got, want := DataDir(), filepath.Join(os.Getenv("HOME"), ".gravity-go"); got != want {
		t.Errorf("DataDir() = %q, want fresh installs to use %q", got, want)
	}
}

func TestDataDirFallsBackToLegacyAntiAPI(t *testing.T) {
	home := t.TempDir()
	pointHomeAt(t, home)

	// An existing legacy install is picked up as-is: no migration step, no
	// lost accounts.
	if err := os.MkdirAll(filepath.Join(home, ".anti-api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := DataDir(), filepath.Join(home, ".anti-api"); got != want {
		t.Errorf("DataDir() = %q, want legacy %q", got, want)
	}

	// Once ~/.gravity-go exists it always wins.
	if err := os.MkdirAll(filepath.Join(home, ".gravity-go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := DataDir(), filepath.Join(home, ".gravity-go"); got != want {
		t.Errorf("DataDir() = %q, want %q once it exists", got, want)
	}
}

func TestDataDirEnvOverrideWins(t *testing.T) {
	home := t.TempDir()
	pointHomeAt(t, home)
	if err := os.MkdirAll(filepath.Join(home, ".anti-api"), 0o755); err != nil {
		t.Fatal(err)
	}

	custom := filepath.Join(home, "custom-data")
	t.Setenv("GRAVITY_DATA_DIR", custom)
	if got := DataDir(); got != custom {
		t.Errorf("DataDir() = %q, want env override %q", got, custom)
	}
}
