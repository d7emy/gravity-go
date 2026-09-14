package logbuf

import (
	"sync"
	"testing"
	"time"
)

func TestAppendAndGet(t *testing.T) {
	reset()
	SetEnabled(true)

	Append("info", "first")
	Append("warn", "second")

	snapshot := Get(10, 0)
	if !snapshot.Enabled {
		t.Fatal("snapshot should report capture enabled")
	}
	if len(snapshot.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(snapshot.Entries))
	}
	if snapshot.Entries[0].Line != "first" || snapshot.Entries[1].Line != "second" {
		t.Errorf("entries = %+v", snapshot.Entries)
	}
	if snapshot.LastID != snapshot.Entries[1].ID {
		t.Errorf("lastID = %d, want %d", snapshot.LastID, snapshot.Entries[1].ID)
	}
}

func TestAppendIsNoOpWhileDisabled(t *testing.T) {
	reset()
	SetEnabled(false)

	Append("info", "dropped")

	snapshot := Get(10, 0)
	if snapshot.Enabled {
		t.Error("snapshot should report capture disabled")
	}
	if len(snapshot.Entries) != 0 {
		t.Errorf("got %d entries, want none while disabled", len(snapshot.Entries))
	}
}

func TestGetSinceIDReturnsOnlyNewer(t *testing.T) {
	reset()
	SetEnabled(true)

	Append("info", "a")
	Append("info", "b")
	Append("info", "c")

	first := Get(10, 0).Entries[0].ID
	newer := Get(10, first)

	if len(newer.Entries) != 2 {
		t.Fatalf("got %d entries, want 2 newer than id %d", len(newer.Entries), first)
	}
	if newer.Entries[0].Line != "b" {
		t.Errorf("first newer entry = %q, want b", newer.Entries[0].Line)
	}
}

func TestSubscribeReceivesNewEntries(t *testing.T) {
	reset()
	SetEnabled(true)

	updates, unsubscribe := Subscribe()
	defer unsubscribe()

	Append("info", "streamed")

	select {
	case entry := <-updates:
		if entry.Line != "streamed" {
			t.Errorf("entry = %q", entry.Line)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber never received the entry")
	}
}

// A stalled SSE client must not block the writer: the fan-out drops rather than
// waits once a subscriber's buffer is full.
func TestSlowSubscriberDoesNotBlockAppend(t *testing.T) {
	reset()
	SetEnabled(true)

	_, unsubscribe := Subscribe()
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		// Far more than the 256-entry buffer, with nothing draining it.
		for i := 0; i < 1000; i++ {
			Append("info", "flood")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Append blocked on a subscriber that was not draining")
	}
}

// Regression: Append copies the listener channels under the lock, releases it,
// and only then sends. A concurrent unsubscribe that closed those channels made
// the send panic with "send on closed channel", taking the whole server down.
// Log lines are emitted on every request and the dashboard opens and closes the
// log stream freely, so this was reachable in normal use.
func TestConcurrentUnsubscribeDoesNotPanicAppend(t *testing.T) {
	reset()
	SetEnabled(true)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers hammering the fan-out.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					Append("info", "line")
				}
			}
		}()
	}

	// Subscribers churning: subscribe, read a little, unsubscribe.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				updates, unsubscribe := Subscribe()
				select {
				case <-updates:
				default:
				}
				unsubscribe()
			}
		}()
	}

	// Let the churn run, then stop the writers.
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// After unsubscribing, a subscriber must stop receiving without the writer
// caring.
func TestUnsubscribeStopsDelivery(t *testing.T) {
	reset()
	SetEnabled(true)

	updates, unsubscribe := Subscribe()
	Append("info", "before")

	// Drain what was sent before unsubscribing.
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("did not receive the pre-unsubscribe entry")
	}

	unsubscribe()
	Append("info", "after")

	select {
	case entry := <-updates:
		t.Errorf("received %q after unsubscribing", entry.Line)
	case <-time.After(100 * time.Millisecond):
		// Silence is the expected outcome.
	}
}

func TestDisablingClearsTheBuffer(t *testing.T) {
	reset()
	SetEnabled(true)
	Append("info", "will be dropped")

	SetEnabled(false)
	SetEnabled(true)

	if entries := Get(10, 0).Entries; len(entries) != 0 {
		t.Errorf("got %d entries, want the buffer cleared on disable", len(entries))
	}
}

// reset returns the package to a clean state between tests.
func reset() {
	mu.Lock()
	defer mu.Unlock()
	buffer = nil
	nextID = 1
	enabled = false
	for id := range listeners {
		delete(listeners, id)
	}
}
