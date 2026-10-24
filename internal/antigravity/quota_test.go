package antigravity

import (
	"encoding/json"
	"strings"
	"testing"
)

// The shape retrieveUserQuotaSummary returns for accounts that are only metered
// on the Claude/GPT group. It is a normal 200, not an error: Gemini simply is not
// in it, even though the same account can call Gemini models.
const claudeOnlySummary = `{
  "description": "Within each group, models share a weekly limit and a 5-hour limit.",
  "groups": [{
    "displayName": "Claude and GPT models",
    "description": "Models within this group: Claude Opus, Claude Sonnet, GPT-OSS",
    "buckets": [
      {"bucketId": "3p-weekly", "displayName": "Weekly Limit Remaining", "window": "weekly",
       "remainingFraction": 1, "resetTime": "2026-10-31T11:53:43Z"},
      {"bucketId": "3p-5h", "displayName": "Five Hour Limit Remaining", "window": "5h",
       "remainingFraction": 1, "resetTime": "2026-10-24T16:53:43Z"}
    ]
  }]
}`

func TestBuildQuotaBarsRendersClaudeOnlySummaryAsIs(t *testing.T) {
	var summary QuotaSummary
	if err := json.Unmarshal([]byte(claudeOnlySummary), &summary); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	bars := BuildQuotaBars(summary)

	if len(bars) != 2 || bars[0].Key != "3p-weekly" || bars[1].Key != "3p-5h" {
		t.Fatalf("bars = %+v, want just 3p-weekly and 3p-5h", bars)
	}
	if HasGeminiBar(bars) {
		t.Error("a Claude-only summary must not claim to carry Gemini quota")
	}
}

func TestHasGeminiBar(t *testing.T) {
	cases := []struct {
		name string
		bars []QuotaBar
		want bool
	}{
		{"no bars", nil, false},
		{"claude only", []QuotaBar{
			{Key: "3p-weekly", Label: "Claude weekly"},
			{Key: "3p-5h", Label: "Claude 5h"},
		}, false},
		{"gemini by key", []QuotaBar{{Key: "gemini-weekly", Label: "Weekly"}}, true},
		{"gemini by label", []QuotaBar{{Key: "1p-5h", Label: "Gemini 5h"}}, true},
		// An empty summary yields these; they already hold Gemini rows, so the
		// caller must not stack a second set on top.
		{"placeholders", DefaultQuotaBars(), true},
	}
	for _, c := range cases {
		if got := HasGeminiBar(c.bars); got != c.want {
			t.Errorf("%s: HasGeminiBar = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBuildGeminiModelBar(t *testing.T) {
	const soon, later = "2026-10-24T16:57:08Z", "2026-10-24T18:00:00Z"
	models := map[string]ModelQuota{
		"gemini-2.5-pro":          {RemainingFraction: 1, ResetTime: later},
		"gemini-3-flash":          {RemainingFraction: 1, ResetTime: later},
		"gemini-3.8-flash-tiered": {RemainingFraction: 0.35, ResetTime: soon},
		// Not Gemini: an exhausted Claude pool must not drag the Gemini bar down.
		"claude-opus-4-6-thinking": {RemainingFraction: 0, ResetTime: soon},
	}

	bar, ok := BuildGeminiModelBar(models)

	if !ok {
		t.Fatal("no bar built from a catalogue full of Gemini models")
	}
	// Across Pro and Flash alike, the bar reports the most constrained model and
	// the soonest reset.
	if bar.Key != "gemini-5h" || bar.Label != "Gemini 5h" || bar.Percentage != 35 || bar.ResetTime != soon {
		t.Errorf("bar = %+v, want gemini-5h/Gemini 5h at 35%% resetting %s", bar, soon)
	}
}

func TestBuildGeminiModelBarNeedsAGeminiModel(t *testing.T) {
	for name, models := range map[string]map[string]ModelQuota{
		"empty catalogue": {},
		"claude only":     {"claude-opus-4-6-thinking": {RemainingFraction: 1}},
		// Present in the catalogue, but a different product with its own limits.
		"image model": {"gemini-3.1-flash-image": {RemainingFraction: 1}},
	} {
		if bar, ok := BuildGeminiModelBar(models); ok {
			t.Errorf("%s: got bar %+v, want none", name, bar)
		}
	}
}

// Leaving the bar out is for "no data", never for "empty": a pool that really is
// out of quota has to show 0%.
func TestBuildGeminiModelBarReportsExhaustedAsZero(t *testing.T) {
	bar, ok := BuildGeminiModelBar(map[string]ModelQuota{"gemini-3-flash": {RemainingFraction: 0}})
	if !ok || bar.Percentage != 0 {
		t.Errorf("exhausted pool: bar = %+v, ok = %v, want a bar at 0%%", bar, ok)
	}
}

// Every Gemini model the dashboard lists must count towards the bar through the
// upstream id it is actually called as. Gemini 3.8 Flash shipped without being in
// these groups, so its quota could be drained while the bar read 100%.
func TestVisibleGeminiModelsCountTowardQuotaBar(t *testing.T) {
	for _, model := range FilterVisibleModels(AvailableModels) {
		if !strings.HasPrefix(model.ID, "gemini-") {
			continue
		}
		upstream := UpstreamModelName(model.ID)
		bar, ok := BuildGeminiModelBar(map[string]ModelQuota{
			upstream: {RemainingFraction: 0.2},
		})
		if !ok || bar.Percentage != 20 {
			t.Errorf("%s (upstream %s) is in no Gemini quota group; bar = %+v, ok = %v",
				model.ID, upstream, bar, ok)
		}
	}
}
