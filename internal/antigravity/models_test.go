package antigravity

import "testing"

// Gemini 3.8 publishes only a "-tiered" variant upstream, exactly like 3.7, so
// every client-facing thinking level must collapse onto it. Passing a -high or
// -low id straight through gets a 404 from Google.
func TestGemini38MapsToTieredUpstream(t *testing.T) {
	for _, id := range []string{
		"gemini-3.8-flash",
		"gemini-3.8-flash-low",
		"gemini-3.8-flash-medium",
		"gemini-3.8-flash-high",
		"gemini-3.8-flash-tiered",
	} {
		if got := UpstreamModelName(id); got != "gemini-3.8-flash-tiered" {
			t.Errorf("UpstreamModelName(%q) = %q, want gemini-3.8-flash-tiered", id, got)
		}
	}
}

// Cyber is Fairwind-gated: it must pass through verbatim so an entitled token
// can use whatever wire id Google issued, while staying out of listings so
// unentitled users are never offered a model that 404s for them.
func TestGemini38CyberPassthroughHidden(t *testing.T) {
	for _, id := range []string{
		"gemini-3.8-flash-cyber",
		"gemini-3.8-flash-cyber-low",
		"gemini-3.8-flash-cyber-medium",
		"gemini-3.8-flash-cyber-high",
		"gemini-3.8-flash-cyber-tiered",
	} {
		if got := UpstreamModelName(id); got != id {
			t.Errorf("UpstreamModelName(%q) = %q, want passthrough %q", id, got, id)
		}
		if IsVisibleModel(id) {
			t.Errorf("IsVisibleModel(%q) = true, want false (gated, must stay hidden)", id)
		}
	}
}

// 3.8 is the newest flash, so it is the one listed; 3.7 stays reachable by id.
func TestGemini38IsListedAnd37StaysCallable(t *testing.T) {
	if !IsVisibleModel("gemini-3.8-flash-high") {
		t.Error("gemini-3.8-flash-high should appear in listings")
	}
	if got := UpstreamModelName("gemini-3.7-flash-high"); got != "gemini-3.7-flash-tiered" {
		t.Errorf("3.7 must remain callable: got %q", got)
	}
}
