package antigravity

import (
	"container/list"
	"encoding/json"
	"os"
	"sync"
	"time"

	"gravity-go/internal/logx"
	"gravity-go/internal/paths"
)

// Gemini thought-signature store.
//
// Gemini 3 requires the thoughtSignature that came back with a functionCall to
// be echoed on every replay of that call in later turns. Missing it is a hard
// 400:
//
//	"Function call is missing a thought_signature in functionCall parts.
//	 This is required for tools to work correctly."
//
// Agent clients persist sessions and replay full history, so an in-process map
// alone loses every signature on restart and breaks resumed sessions. This store
// therefore persists to <dataDir>/tool-signatures.json (debounced) and evicts
// least-recently-used entries instead of wiping wholesale.

const (
	maxSignatureEntries   = 4000
	signatureSaveDebounce = 3 * time.Second
)

type signatureStore struct {
	mu      sync.Mutex
	entries map[string]*list.Element // toolCallID -> element in recency list
	order   *list.List               // front = least recent, back = most recent
	loaded  bool
	timer   *time.Timer
}

type signatureEntry struct {
	id        string
	signature string
}

var signatures = &signatureStore{
	entries: map[string]*list.Element{},
	order:   list.New(),
}

func signatureStorePath() string { return paths.DataFile("tool-signatures.json") }

func (s *signatureStore) ensureLoadedLocked() {
	if s.loaded {
		return
	}
	s.loaded = true

	data, err := os.ReadFile(signatureStorePath())
	if err != nil {
		return
	}
	var raw struct {
		Signatures map[string]string `json:"signatures"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		logx.Debug("[tool-signatures] load skipped: %v", err)
		return
	}
	for id, signature := range raw.Signatures {
		if id == "" || signature == "" {
			continue
		}
		element := s.order.PushBack(signatureEntry{id: id, signature: signature})
		s.entries[id] = element
	}
	s.trimLocked()
}

func (s *signatureStore) trimLocked() {
	for s.order.Len() > maxSignatureEntries {
		oldest := s.order.Front()
		if oldest == nil {
			return
		}
		entry := oldest.Value.(signatureEntry)
		s.order.Remove(oldest)
		delete(s.entries, entry.id)
	}
}

// snapshotLocked copies the entries into a slice. This is deliberately the only
// part of saving that runs under the lock, and it is a flat slice copy rather
// than a map build: the map's hashing is pure overhead to do while holding a
// lock that GetToolCallSignature needs on the request path. The map is assembled
// later, in writeSignatures, with the lock released.
func (s *signatureStore) snapshotLocked() []signatureEntry {
	out := make([]signatureEntry, 0, s.order.Len())
	for element := s.order.Front(); element != nil; element = element.Next() {
		out = append(out, element.Value.(signatureEntry))
	}
	return out
}

// writeSignatures encodes and persists a snapshot. It must be called WITHOUT the
// store lock held.
func writeSignatures(entries []signatureEntry) {
	snapshot := make(map[string]string, len(entries))
	for _, entry := range entries {
		snapshot[entry.id] = entry.signature
	}

	if _, err := paths.EnsureDataDir(); err != nil {
		logx.Debug("[tool-signatures] save skipped: %v", err)
		return
	}
	data, err := json.Marshal(struct {
		Signatures map[string]string `json:"signatures"`
	}{Signatures: snapshot})
	if err != nil {
		logx.Debug("[tool-signatures] save skipped: %v", err)
		return
	}

	// Write-then-rename so a crash cannot leave a truncated store behind.
	path := signatureStorePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		logx.Debug("[tool-signatures] save skipped: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logx.Debug("[tool-signatures] save skipped: %v", err)
	}
}

func (s *signatureStore) scheduleSaveLocked() {
	if s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(signatureSaveDebounce, func() {
		s.mu.Lock()
		s.timer = nil
		snapshot := s.snapshotLocked()
		s.mu.Unlock()

		writeSignatures(snapshot)
	})
}

// RememberToolCallSignature stores the signature that arrived with a functionCall.
func RememberToolCallSignature(toolCallID, signature string) {
	if toolCallID == "" || signature == "" {
		return
	}
	signatures.mu.Lock()
	defer signatures.mu.Unlock()
	signatures.ensureLoadedLocked()

	if element, ok := signatures.entries[toolCallID]; ok {
		if element.Value.(signatureEntry).signature == signature {
			// Already current; just refresh recency.
			signatures.order.MoveToBack(element)
			return
		}
		signatures.order.Remove(element)
	}
	element := signatures.order.PushBack(signatureEntry{id: toolCallID, signature: signature})
	signatures.entries[toolCallID] = element
	signatures.trimLocked()
	signatures.scheduleSaveLocked()
}

// GetToolCallSignature returns the stored signature for a tool call id.
func GetToolCallSignature(toolCallID string) string {
	if toolCallID == "" {
		return ""
	}
	signatures.mu.Lock()
	defer signatures.mu.Unlock()
	signatures.ensureLoadedLocked()

	element, ok := signatures.entries[toolCallID]
	if !ok {
		return ""
	}
	// Refresh recency so long-running sessions never evict their own history.
	signatures.order.MoveToBack(element)
	return element.Value.(signatureEntry).signature
}

// ToolCallSignatureCount reports how many signatures are held.
func ToolCallSignatureCount() int {
	signatures.mu.Lock()
	defer signatures.mu.Unlock()
	signatures.ensureLoadedLocked()
	return len(signatures.entries)
}

// FlushToolCallSignatures forces a synchronous write.
func FlushToolCallSignatures() {
	signatures.mu.Lock()
	if signatures.timer != nil {
		signatures.timer.Stop()
		signatures.timer = nil
	}
	snapshot := signatures.snapshotLocked()
	signatures.mu.Unlock()

	writeSignatures(snapshot)
}

// ResetToolCallSignatures drops everything, in memory and on disk.
func ResetToolCallSignatures() {
	signatures.mu.Lock()
	signatures.entries = map[string]*list.Element{}
	signatures.order = list.New()
	signatures.loaded = true
	if signatures.timer != nil {
		signatures.timer.Stop()
		signatures.timer = nil
	}
	snapshot := signatures.snapshotLocked()
	signatures.mu.Unlock()

	writeSignatures(snapshot)
}
