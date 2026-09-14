package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"
)

// realJPEG returns a genuine small JPEG, so magic-byte sniffing is exercised
// against real bytes rather than a hand-written stub.
func realJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{220, 30, 30, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func imagePartBlocks(t *testing.T, url string) []map[string]any {
	t.Helper()
	payload := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
			map[string]any{"type": "text", "text": "what colour?"},
		},
	}
	raw, err := json.Marshal(payload["content"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	content := translateContent(json.RawMessage(raw))

	out := make([]map[string]any, 0, len(content.Blocks))
	for _, b := range content.Blocks {
		m := map[string]any{"type": b.Type, "text": b.Text}
		if b.Source != nil {
			m["mediaType"] = b.Source.MediaType
			m["data"] = b.Source.Data
		}
		out = append(out, m)
	}
	return out
}

// Every spelling a client actually produces for the same JPEG must inline it,
// and must be labelled image/jpeg regardless of what the client claimed.
func TestDataURLVariantsAllInlineTheImage(t *testing.T) {
	raw := realJPEG(t)
	b64 := base64.StdEncoding.EncodeToString(raw)

	cases := []struct{ name, url string }{
		{"canonical", "data:image/jpeg;base64," + b64},
		{"image/jpg (not a real media type)", "data:image/jpg;base64," + b64},
		{"no media type", "data:;base64," + b64},
		{"uppercase scheme and marker", "DATA:IMAGE/JPEG;BASE64," + b64},
		{"extra parameter", "data:image/jpeg;charset=utf-8;base64," + b64},
		{"wrapped across lines", "data:image/jpeg;base64," + b64[:20] + "\n" + b64[20:]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := imagePartBlocks(t, tc.url)
			if len(blocks) != 2 {
				t.Fatalf("got %d blocks, want image + text: %v", len(blocks), blocks)
			}
			if blocks[0]["type"] != "image" {
				t.Fatalf("first block is %v, want an inlined image", blocks[0])
			}
			if got := blocks[0]["mediaType"]; got != "image/jpeg" {
				t.Errorf("mediaType = %v, want image/jpeg (sniffed from the bytes)", got)
			}
			decoded, err := base64.StdEncoding.DecodeString(blocks[0]["data"].(string))
			if err != nil {
				t.Fatalf("re-encoded data is not base64: %v", err)
			}
			if !bytes.Equal(decoded, raw) {
				t.Errorf("image bytes changed in transit (%d in, %d out)", len(raw), len(decoded))
			}
		})
	}
}

// PNG and WEBP must be sniffed correctly too, not forced to jpeg.
func TestMimeSniffedFromBytes(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBP"), 0, 0, 0, 0)

	for _, tc := range []struct {
		name, want string
		data       []byte
	}{
		{"png", "image/png", png},
		{"webp", "image/webp", webp},
		{"jpeg", "image/jpeg", realJPEG(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := "data:application/octet-stream;base64," +
				base64.StdEncoding.EncodeToString(tc.data)
			blocks := imagePartBlocks(t, url)
			if got := blocks[0]["mediaType"]; got != tc.want {
				t.Errorf("mediaType = %v, want %v", got, tc.want)
			}
		})
	}
}

// Regression: an image that could not be inlined was pasted back into the prompt
// as text. A file path fed the model the file NAME, and an unparsed data URL fed
// it megabytes of base64 — from which it confidently answered questions about a
// picture it had never seen. A wrong answer is worse than a missing one.
func TestUnusableImageIsNeverPastedIntoThePrompt(t *testing.T) {
	long := strings.Repeat("A", 5000)

	cases := []struct {
		name    string
		url     string
		mustNot string
	}{
		{"filesystem path leaks the name", "/home/user/a_red_square.jpg", "red"},
		{"file URL leaks the name", "file:///C:/pics/a_red_square.jpg", "red"},
		{"bare base64 floods the prompt", long, long[:200]},
		{"undecodable data URL floods the prompt", "data:image/jpeg;base64," + "!!!" + long, long[:200]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := imagePartBlocks(t, tc.url)
			if len(blocks) != 2 {
				t.Fatalf("got %d blocks, want a marker + the text", len(blocks))
			}
			if blocks[0]["type"] != "text" {
				t.Fatalf("unusable image became %v, want a text marker", blocks[0]["type"])
			}
			marker, _ := blocks[0]["text"].(string)
			if !strings.Contains(marker, "image omitted") {
				t.Errorf("marker does not say the image was omitted: %q", marker)
			}
			if strings.Contains(strings.ToLower(marker), tc.mustNot) {
				t.Errorf("marker leaks the original value into the prompt: %q", marker)
			}
			if len(marker) > 400 {
				t.Errorf("marker is %d chars; it must not carry the payload", len(marker))
			}
		})
	}
}

// A remote URL is the one case worth naming: it is short, and telling the reader
// which URL was skipped is genuinely useful.
func TestRemoteURLIsNamedInTheMarker(t *testing.T) {
	blocks := imagePartBlocks(t, "https://example.com/photo.png")
	marker, _ := blocks[0]["text"].(string)
	if !strings.Contains(marker, "https://example.com/photo.png") {
		t.Errorf("marker should name the skipped URL: %q", marker)
	}
	if !strings.Contains(marker, "base64") {
		t.Errorf("marker should say what to send instead: %q", marker)
	}
}
