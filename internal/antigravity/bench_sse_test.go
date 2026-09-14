package antigravity

import (
	"io"
	"strings"
	"testing"
)

// oneBigEvent builds a single SSE event of the given payload size. Long model
// output and large tool results arrive exactly like this: one event far bigger
// than the 8KB read chunk.
func oneBigEvent(payloadBytes int) string {
	var b strings.Builder
	b.WriteString("data: ")
	b.WriteString(strings.Repeat("x", payloadBytes))
	b.WriteString("\n\n")
	return b.String()
}

func benchSSERead(b *testing.B, payloadBytes int) {
	stream := oneBigEvent(payloadBytes)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		r := &sseReader{source: strings.NewReader(stream)}
		for {
			_, err := r.next()
			if err != nil {
				break
			}
		}
	}
}

func BenchmarkSSEReadEvent64KB(b *testing.B)  { benchSSERead(b, 64<<10) }
func BenchmarkSSEReadEvent256KB(b *testing.B) { benchSSERead(b, 256<<10) }
func BenchmarkSSEReadEvent1MB(b *testing.B)   { benchSSERead(b, 1<<20) }

// Correctness: events must come back whole and in order regardless of how they
// straddle the 8KB read boundary, including CRLF separators.
func TestSSEReaderSplitsEventsAcrossChunkBoundaries(t *testing.T) {
	for _, sep := range []string{"\n\n", "\r\n\r\n"} {
		// The payload starts at offset 6 ("data: "), so a separator beginning at
		// 6+size straddles the 8192-byte read boundary for sizes just under 8186.
		// Those are the only sizes that actually exercise the rescan overlap;
		// round numbers like 8192 do not.
		for _, size := range []int{1, 8182, 8183, 8184, 8185, 8186, 8192, 20000} {
			payload := strings.Repeat("y", size)
			stream := "data: " + payload + sep + "data: second" + sep
			r := &sseReader{source: strings.NewReader(stream)}

			first, err := r.next()
			if err != nil {
				t.Fatalf("sep=%q size=%d: first next: %v", sep, size, err)
			}
			if got := extractSSEEventData(first); got != payload {
				t.Errorf("sep=%q size=%d: first event len=%d, want %d",
					sep, size, len(got), len(payload))
			}

			second, err := r.next()
			if err != nil && err != io.EOF {
				t.Fatalf("sep=%q size=%d: second next: %v", sep, size, err)
			}
			if got := extractSSEEventData(second); got != "second" {
				t.Errorf("sep=%q size=%d: second event = %q, want %q",
					sep, size, got, "second")
			}
		}
	}
}
