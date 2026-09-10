package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func floatPtr(v float64) *float64 { return &v }

// generationConfigFrom pulls the generationConfig out of an upstream payload,
// wherever it sits in the envelope.
func generationConfigFrom(t *testing.T, body string) map[string]any {
	t.Helper()
	var sent map[string]any
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("payload is not json: %v\n%s", err, body)
	}
	if gc, ok := sent["generationConfig"].(map[string]any); ok {
		return gc
	}
	for _, v := range sent {
		if nested, ok := v.(map[string]any); ok {
			if gc, ok := nested["generationConfig"].(map[string]any); ok {
				return gc
			}
		}
	}
	raw, _ := json.MarshalIndent(sent, "", " ")
	t.Fatalf("no generationConfig in payload:\n%s", raw)
	return nil
}

// The sampling parameters must reach the wire under Gemini's field names, and
// must be omitted entirely when the client sends none so the upstream default
// applies rather than a value we invented.
//
// Whether upstream then acts on them is a separate matter: it does not. The
// Antigravity endpoint reads maxOutputTokens from this same block but discards
// temperature, topP and topK — verified live, where temperature 0 with topK 1
// still returns a different completion on every call. This test pins the half
// that is ours to get right.
func TestSamplingParametersReachTheWire(t *testing.T) {
	cases := []struct {
		name     string
		req      ChatRequest
		wantKeys map[string]any
		absent   []string
	}{
		{
			name:   "none set",
			req:    ChatRequest{},
			absent: []string{"temperature", "topP", "topK"},
		},
		{
			name:     "temperature zero is sent, not treated as unset",
			req:      ChatRequest{Temperature: floatPtr(0)},
			wantKeys: map[string]any{"temperature": 0.0},
			absent:   []string{"topP", "topK"},
		},
		{
			name: "all three",
			req: ChatRequest{
				Temperature: floatPtr(1.5),
				TopP:        floatPtr(0.9),
				TopK:        floatPtr(40),
			},
			wantKeys: map[string]any{"temperature": 1.5, "topP": 0.9, "topK": 40.0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useSingleTokenAuth(t)
			stub := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, sseChunks(textChunk("ok")))
			})

			req := tc.req
			req.Model = "gemini-3.7-flash-high"
			req.Messages = []Message{userMessage("hi")}

			if _, err := CreateChatCompletion(context.Background(), req, CallOptions{}); err != nil {
				t.Fatalf("request: %v", err)
			}

			gc := generationConfigFrom(t, <-stub.bodies)
			for key, want := range tc.wantKeys {
				got, ok := gc[key]
				if !ok {
					t.Errorf("%s missing from generationConfig", key)
					continue
				}
				if got != want {
					t.Errorf("%s = %v, want %v", key, got, want)
				}
			}
			for _, key := range tc.absent {
				if got, ok := gc[key]; ok {
					t.Errorf("%s = %v sent when the client set none", key, got)
				}
			}

			// maxOutputTokens shares this block and is the one field upstream
			// does honour, so it must always be present.
			if _, ok := gc["maxOutputTokens"]; !ok {
				t.Error("maxOutputTokens missing from generationConfig")
			}
		})
	}
}
