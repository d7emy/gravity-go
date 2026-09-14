// Package logbuf keeps an in-memory ring of recent log lines so the dashboard
// can show them, and fans new lines out to /logs/stream subscribers.
package logbuf

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// Entry is one captured log line.
type Entry struct {
	ID    int64  `json:"id"`
	TS    string `json:"ts"`
	Level string `json:"level"`
	Line  string `json:"line"`
}

// Snapshot is the /logs payload.
type Snapshot struct {
	Entries  []Entry `json:"entries"`
	LastID   int64   `json:"lastId"`
	MaxLines int     `json:"maxLines"`
	Enabled  bool    `json:"enabled"`
}

const defaultMaxLines = 2000

var (
	mu        sync.RWMutex
	buffer    []Entry
	nextID    int64 = 1
	enabled   bool
	maxLines  = resolveMaxLines()
	listeners = map[int64]chan Entry{}
	listenerN int64
)

func resolveMaxLines() int {
	raw := os.Getenv("GRAVITY_LOG_LINES")
	if raw == "" {
		raw = os.Getenv("ANTI_API_LOG_LINES")
	}
	if n, err := strconv.Atoi(raw); err == nil && n >= 100 {
		return n
	}
	return defaultMaxLines
}

// SetEnabled turns capture on or off. Turning it off drops the buffer.
func SetEnabled(v bool) {
	mu.Lock()
	defer mu.Unlock()
	enabled = v
	if !v {
		buffer = nil
	}
}

// IsEnabled reports whether capture is on.
func IsEnabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return enabled
}

// Append records one line. It is a no-op while capture is off.
func Append(level, line string) {
	mu.Lock()
	if !enabled {
		mu.Unlock()
		return
	}
	entry := Entry{
		ID:    nextID,
		TS:    time.Now().UTC().Format(time.RFC3339Nano),
		Level: level,
		Line:  line,
	}
	nextID++
	buffer = append(buffer, entry)
	if len(buffer) > maxLines {
		buffer = buffer[len(buffer)-maxLines:]
	}
	// The fan-out happens under the lock on purpose. Copying the channels and
	// sending after unlocking races with unsubscribe: the channel could be
	// removed and closed in between, and the send would panic. Every send here
	// is non-blocking, so holding the lock cannot stall the writer.
	for _, ch := range listeners {
		select {
		case ch <- entry:
		default:
			// Stalled SSE client: drop rather than block the writer.
		}
	}
	mu.Unlock()
}

// Get returns recent entries. sinceID > 0 returns only newer ones.
func Get(limit int, sinceID int64) Snapshot {
	mu.RLock()
	defer mu.RUnlock()

	if !enabled {
		return Snapshot{Entries: []Entry{}, MaxLines: maxLines, Enabled: false}
	}
	if limit < 1 {
		limit = 500
	}
	if limit > maxLines {
		limit = maxLines
	}

	var entries []Entry
	if sinceID > 0 {
		for _, e := range buffer {
			if e.ID > sinceID {
				entries = append(entries, e)
			}
		}
	} else if len(buffer) > limit {
		entries = append(entries, buffer[len(buffer)-limit:]...)
	} else {
		entries = append(entries, buffer...)
	}
	if entries == nil {
		entries = []Entry{}
	}

	var lastID int64
	if len(buffer) > 0 {
		lastID = buffer[len(buffer)-1].ID
	}
	return Snapshot{Entries: entries, LastID: lastID, MaxLines: maxLines, Enabled: true}
}

// Subscribe returns a channel of new entries and a cancel func.
func Subscribe() (<-chan Entry, func()) {
	mu.Lock()
	defer mu.Unlock()
	listenerN++
	id := listenerN
	ch := make(chan Entry, 256)
	listeners[id] = ch
	// Unsubscribing only detaches the channel; it deliberately does not close
	// it. A closed channel would be a panic waiting to happen on any in-flight
	// send, and the consumer already exits on its request context instead.
	return ch, func() {
		mu.Lock()
		defer mu.Unlock()
		delete(listeners, id)
	}
}
