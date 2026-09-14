package antigravity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// groundedSearchResponse is a realistic generateContent reply with grounding.
const groundedSearchResponse = `{"response":{"candidates":[{"content":{"parts":[
{"text":"Go 1.27.0 is the latest release."}]},
"groundingMetadata":{
 "webSearchQueries":["latest go release"],
 "groundingChunks":[
   {"web":{"uri":"https://go.dev/dl/","title":"go.dev"}},
   {"web":{"uri":"https://go.dev/dl/","title":"go.dev duplicate"}},
   {"web":{"uri":"https://tip.golang.org/","title":"tip"}}],
 "groundingSupports":[
   {"segment":{"startIndex":0,"endIndex":10},"groundingChunkIndices":[0,1,2]}]}}]}}`

// Regression: the search path cancelled its request context immediately after
// http.Client.Do returned. Do returns once headers arrive, so cancelling there
// tore down the connection while the body was still streaming — the read came
// back truncated and parsing failed with "Search response was not valid JSON".
// Short replies happened to be buffered already and worked, which is exactly the
// case where grounding had not fired, so the feature looked fine while being
// broken for its entire purpose.
func TestSearchReadsBodyThatArrivesAfterHeaders(t *testing.T) {
	useSingleTokenAuth(t)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			flusher, _ := w.(http.Flusher)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Commit headers, then stall before sending the body — the shape that
			// exposed the premature cancel.
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(150 * time.Millisecond)
			fmt.Fprint(w, groundedSearchResponse)
		}))
	defer server.Close()

	original := baseURLs
	baseURLs = []string{server.URL}
	defer func() { baseURLs = original }()

	result, err := PerformWebSearch(context.Background(), "latest go release", "")
	if err != nil {
		t.Fatalf("PerformWebSearch() error = %v", err)
	}
	if !strings.Contains(result.Answer, "Go 1.27.0") {
		t.Errorf("answer = %q, want the body that arrived after the headers", result.Answer)
	}
	if len(result.Sources) == 0 {
		t.Error("no sources parsed; grounding metadata was lost")
	}
}

// Duplicate grounding chunks pointing at the same URL should collapse to one
// source, or citation lists repeat the same link.
func TestSearchDeduplicatesSourcesByURL(t *testing.T) {
	useSingleTokenAuth(t)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, groundedSearchResponse)
		}))
	defer server.Close()

	original := baseURLs
	baseURLs = []string{server.URL}
	defer func() { baseURLs = original }()

	result, err := PerformWebSearch(context.Background(), "q", "")
	if err != nil {
		t.Fatalf("PerformWebSearch() error = %v", err)
	}

	seen := map[string]int{}
	for _, source := range result.Sources {
		seen[source.URL]++
	}
	for url, count := range seen {
		if count > 1 {
			t.Errorf("source %q appears %d times, want deduplicated", url, count)
		}
	}
	if len(result.Sources) != 2 {
		t.Errorf("got %d sources, want 2 after dedupe of 3 chunks", len(result.Sources))
	}
}

func TestSearchRejectsEmptyAndOverlongQueries(t *testing.T) {
	useSingleTokenAuth(t)

	if _, err := PerformWebSearch(context.Background(), "   ", ""); err == nil {
		t.Error("an empty query should be rejected")
	}
	if _, err := PerformWebSearch(context.Background(), strings.Repeat("x", 5000), ""); err == nil {
		t.Error("an overlong query should be rejected")
	}
}

// A non-2xx from upstream must surface as an upstream error, not be parsed as a
// body.
func TestSearchSurfacesUpstreamFailure(t *testing.T) {
	useSingleTokenAuth(t)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
		}))
	defer server.Close()

	original := baseURLs
	baseURLs = []string{server.URL}
	defer func() { baseURLs = original }()

	if _, err := PerformWebSearch(context.Background(), "q", ""); err == nil {
		t.Error("a 500 should surface as an error")
	}
}
