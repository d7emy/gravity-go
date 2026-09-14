package antigravity

import (
	"fmt"
	"testing"
	"time"
)

// fillSignatures loads the store to capacity with realistic-sized entries.
func fillSignatures(t testing.TB) {
	t.Helper()
	t.Setenv("GRAVITY_DATA_DIR", t.TempDir())
	ResetToolCallSignatures()

	// Real thought signatures are long opaque blobs; this matches the ~1.1MB
	// store observed in practice at the 4000-entry cap.
	blob := ""
	for len(blob) < 270 {
		blob += "AbCdEf0123456789"
	}
	for i := 0; i < maxSignatureEntries; i++ {
		RememberToolCallSignature(fmt.Sprintf("call_%06d", i), blob)
	}
}

// How long is the store's lock held by a save? GetToolCallSignature runs on the
// request path and blocks on that same lock.
func TestSaveLockHoldTime(t *testing.T) {
	fillSignatures(t)

	// Time only the portion that runs under the lock. GetToolCallSignature sits
	// on the request path waiting for this same lock, so this is the number that
	// determines whether a save stalls a live request.
	signatures.mu.Lock()
	start := time.Now()
	snapshot := signatures.snapshotLocked()
	held := time.Since(start)
	signatures.mu.Unlock()

	writeStart := time.Now()
	writeSignatures(snapshot)
	wrote := time.Since(writeStart)

	t.Logf("lock held %v; encode+write %v outside the lock (%d entries)",
		held.Round(time.Microsecond), wrote.Round(time.Microsecond),
		signatures.order.Len())
}

func BenchmarkSignatureSave(b *testing.B) {
	fillSignatures(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		signatures.mu.Lock()
		snapshot := signatures.snapshotLocked()
		signatures.mu.Unlock()
		writeSignatures(snapshot)
	}
}

func BenchmarkSignatureLookup(b *testing.B) {
	fillSignatures(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = GetToolCallSignature("call_002000")
	}
}

// The snapshot is the only part still holding the store lock, so its cost is
// the real blocking time a live request can hit.
func BenchmarkSignatureSnapshotUnderLock(b *testing.B) {
	fillSignatures(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		signatures.mu.Lock()
		_ = signatures.snapshotLocked()
		signatures.mu.Unlock()
	}
}

// And the encode+write that now happens with the lock released.
func BenchmarkSignatureEncodeAndWrite(b *testing.B) {
	fillSignatures(b)
	signatures.mu.Lock()
	snapshot := signatures.snapshotLocked()
	signatures.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writeSignatures(snapshot)
	}
}
