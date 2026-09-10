package antigravity

import (
	"container/list"
	"fmt"
	"testing"
)

// useTempDataDir points the data directory at a per-test temp dir, so the
// signature store never touches the user's real ~/.anti-api.
func useTempDataDir(t *testing.T) {
	t.Helper()
	t.Setenv("GRAVITY_DATA_DIR", t.TempDir())
}

// reloadSignatureStore simulates a process restart against the same data dir.
func reloadSignatureStore() {
	signatures.mu.Lock()
	defer signatures.mu.Unlock()
	signatures.entries = map[string]*list.Element{}
	signatures.order = list.New()
	signatures.loaded = false
}

func TestRemembersAndReturnsSignature(t *testing.T) {
	useTempDataDir(t)
	ResetToolCallSignatures()

	RememberToolCallSignature("toolu_1", "sig-1")

	if got := GetToolCallSignature("toolu_1"); got != "sig-1" {
		t.Errorf("GetToolCallSignature() = %q, want sig-1", got)
	}
	if got := GetToolCallSignature("missing"); got != "" {
		t.Errorf("GetToolCallSignature(missing) = %q, want empty", got)
	}
}

func TestIgnoresEmptyIDsAndSignatures(t *testing.T) {
	useTempDataDir(t)
	ResetToolCallSignatures()

	RememberToolCallSignature("", "sig")
	RememberToolCallSignature("toolu_x", "")

	if got := ToolCallSignatureCount(); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
}

// Agent clients replay history across proxy restarts, so an in-memory-only store
// would break every resumed session.
func TestSignaturesSurviveRestart(t *testing.T) {
	useTempDataDir(t)
	ResetToolCallSignatures()

	RememberToolCallSignature("toolu_persist", "sig-persist")
	FlushToolCallSignatures()

	reloadSignatureStore()

	if got := GetToolCallSignature("toolu_persist"); got != "sig-persist" {
		t.Errorf("after restart GetToolCallSignature() = %q, want sig-persist", got)
	}
}

// The old behaviour wiped the whole map at the cap, which dropped signatures a
// live session still needed. Eviction must be least-recently-used instead.
func TestEvictsLeastRecentlyUsed(t *testing.T) {
	useTempDataDir(t)
	ResetToolCallSignatures()

	RememberToolCallSignature("keep-me", "sig-keep")
	for i := 0; i < 3900; i++ {
		RememberToolCallSignature(fmt.Sprintf("filler-%d", i), fmt.Sprintf("sig-%d", i))
	}

	// Reading refreshes recency, so an active session keeps its own history alive.
	if got := GetToolCallSignature("keep-me"); got != "sig-keep" {
		t.Fatalf("GetToolCallSignature(keep-me) = %q before the flood", got)
	}

	// Push past the 4000 cap.
	for i := 0; i < 500; i++ {
		RememberToolCallSignature(fmt.Sprintf("flood-%d", i), fmt.Sprintf("sig-flood-%d", i))
	}

	if got := GetToolCallSignature("keep-me"); got != "sig-keep" {
		t.Errorf("refreshed entry was evicted: got %q, want sig-keep", got)
	}
	if got := GetToolCallSignature("filler-0"); got != "" {
		t.Errorf("oldest filler survived: got %q, want it evicted", got)
	}
	if got := GetToolCallSignature("flood-499"); got != "sig-flood-499" {
		t.Errorf("newest entry missing: got %q", got)
	}
	if count := ToolCallSignatureCount(); count > maxSignatureEntries {
		t.Errorf("count = %d, want <= %d", count, maxSignatureEntries)
	}
}

func TestRememberOverwritesChangedSignature(t *testing.T) {
	useTempDataDir(t)
	ResetToolCallSignatures()

	RememberToolCallSignature("toolu_1", "old")
	RememberToolCallSignature("toolu_1", "new")

	if got := GetToolCallSignature("toolu_1"); got != "new" {
		t.Errorf("GetToolCallSignature() = %q, want new", got)
	}
	if got := ToolCallSignatureCount(); got != 1 {
		t.Errorf("count = %d, want 1 (no duplicate entry)", got)
	}
}
