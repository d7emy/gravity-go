package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// wirePartsFor sends one user message and returns the parts that reached the
// upstream payload.
func wirePartsFor(t *testing.T, blocksJSON string) []any {
	t.Helper()
	useSingleTokenAuth(t)
	stub := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseChunks(textChunk("ok")))
	})

	var blocks []ContentBlock
	if err := json.Unmarshal([]byte(blocksJSON), &blocks); err != nil {
		t.Fatalf("unmarshal blocks: %v", err)
	}
	if _, err := CreateChatCompletion(context.Background(), ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []Message{{Role: "user", Content: MessageContent{Blocks: blocks}}},
	}, CallOptions{}); err != nil {
		t.Fatalf("request: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(<-stub.bodies), &sent); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	find := func(m map[string]any) []any {
		if c, ok := m["contents"].([]any); ok && len(c) > 0 {
			if first, ok := c[0].(map[string]any); ok {
				if p, ok := first["parts"].([]any); ok {
					return p
				}
			}
		}
		return nil
	}
	if p := find(sent); p != nil {
		return p
	}
	for _, v := range sent {
		if nested, ok := v.(map[string]any); ok {
			if p := find(nested); p != nil {
				return p
			}
		}
	}
	t.Fatal("no contents[0].parts in payload")
	return nil
}

// A base64 image must arrive as Gemini inlineData with its media type intact.
func TestBase64ImageBecomesInlineData(t *testing.T) {
	parts := wirePartsFor(t, `[
      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}},
      {"type":"text","text":"what is this?"}
    ]`)

	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2 (image + text)", len(parts))
	}
	first, _ := parts[0].(map[string]any)
	inline, ok := first["inlineData"].(map[string]any)
	if !ok {
		t.Fatalf("first part is not inlineData: %v", first)
	}
	if inline["mimeType"] != "image/png" {
		t.Errorf("mimeType = %v, want image/png", inline["mimeType"])
	}
	if inline["data"] != "iVBORw0KGgo=" {
		t.Errorf("data = %v, want the base64 payload", inline["data"])
	}
}

// Regression: an image the proxy cannot inline used to be dropped silently. The
// model then received a bare question about a picture it was never sent, and the
// client saw "I can't see an image" with nothing in the logs to explain it. Every
// unsupported shape must leave a visible marker instead.
func TestUnsupportedImageSourcesAreReportedNotDropped(t *testing.T) {
	cases := []struct {
		name    string
		blocks  string
		wantHas string
	}{
		{
			name: "url source",
			blocks: `[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},
			          {"type":"text","text":"describe it"}]`,
			wantHas: "https://example.com/a.png",
		},
		{
			name: "unknown source type",
			blocks: `[{"type":"image","source":{"type":"file_id","data":""}},
			          {"type":"text","text":"describe it"}]`,
			wantHas: "file_id",
		},
		{
			name:    "missing source",
			blocks:  `[{"type":"image"},{"type":"text","text":"describe it"}]`,
			wantHas: "no source",
		},
		{
			name: "base64 with empty data",
			blocks: `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":""}},
			          {"type":"text","text":"describe it"}]`,
			wantHas: "no data",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := wirePartsFor(t, tc.blocks)

			if len(parts) != 2 {
				t.Fatalf("got %d parts, want 2 — the image must leave a marker, "+
					"not disappear", len(parts))
			}
			raw, _ := json.Marshal(parts)
			if strings.Contains(string(raw), "inlineData") {
				t.Errorf("an unsupported source was sent as inlineData: %s", raw)
			}
			if !strings.Contains(string(raw), tc.wantHas) {
				t.Errorf("marker does not mention %q: %s", tc.wantHas, raw)
			}
			if !strings.Contains(string(raw), "image omitted") {
				t.Errorf("marker does not say the image was omitted: %s", raw)
			}
		})
	}
}
