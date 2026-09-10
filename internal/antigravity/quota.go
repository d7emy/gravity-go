package antigravity

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"gravity-go/internal/apperr"
)

var cloudCodeBaseURL = "https://cloudcode-pa.googleapis.com"

// SetCloudCodeBaseURLForTest overrides the quota host for contained tests.
func SetCloudCodeBaseURLForTest(url string) { cloudCodeBaseURL = url }

// QuotaBucket is one quota window reported by the summary endpoint.
type QuotaBucket struct {
	BucketID          string  `json:"bucketId"`
	DisplayName       string  `json:"displayName"`
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime"`
	Description       string  `json:"description"`
}

// QuotaGroup groups related buckets.
type QuotaGroup struct {
	DisplayName string        `json:"displayName"`
	Description string        `json:"description"`
	Buckets     []QuotaBucket `json:"buckets"`
}

// QuotaSummary is the retrieveUserQuotaSummary payload.
type QuotaSummary struct {
	Groups      []QuotaGroup `json:"groups"`
	Description string       `json:"description"`
}

// ModelQuota is the per-model quota carried by fetchAvailableModels.
type ModelQuota struct {
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime"`
}

func postCloudCode(
	ctx context.Context, path, accessToken string, payload any,
) (jsonResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return jsonResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cloudCodeBaseURL+path, strings.NewReader(string(body)))
	if err != nil {
		return jsonResponse{}, err
	}
	setUpstreamHeaders(req, accessToken, "")
	return doJSON(req)
}

// resolveProjectID falls back to a lookup, then to a generated id.
func resolveProjectID(ctx context.Context, accessToken, projectID string) string {
	if projectID != "" {
		return projectID
	}
	if resolved := GetProjectID(ctx, accessToken); resolved != "" {
		return resolved
	}
	return GenerateMockProjectID()
}

// FetchQuotaSummary reads the account's quota windows.
func FetchQuotaSummary(
	ctx context.Context, accessToken, projectID string,
) (QuotaSummary, error) {
	project := resolveProjectID(ctx, accessToken, projectID)
	resp, err := postCloudCode(ctx, "/v1internal:retrieveUserQuotaSummary",
		accessToken, map[string]any{"project": project})
	if err != nil {
		return QuotaSummary{}, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return QuotaSummary{}, apperr.NewUpstream("antigravity", resp.Status, resp.Body, "")
	}

	var summary QuotaSummary
	if err := json.Unmarshal([]byte(resp.Body), &summary); err != nil {
		return QuotaSummary{}, err
	}
	return summary, nil
}

// FetchModels reads per-model quota, and reports the project id it used.
func FetchModels(
	ctx context.Context, accessToken, projectID string,
) (map[string]ModelQuota, string, error) {
	project := resolveProjectID(ctx, accessToken, projectID)
	resp, err := postCloudCode(ctx, "/v1internal:fetchAvailableModels",
		accessToken, map[string]any{"project": project})
	if err != nil {
		return nil, project, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, project, apperr.NewUpstream("antigravity", resp.Status, resp.Body, "")
	}

	var data struct {
		Models map[string]struct {
			QuotaInfo *ModelQuota `json:"quotaInfo"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &data); err != nil {
		return nil, project, err
	}

	out := make(map[string]ModelQuota, len(data.Models))
	for name, info := range data.Models {
		if info.QuotaInfo != nil {
			out[name] = *info.QuotaInfo
		} else {
			out[name] = ModelQuota{}
		}
	}
	return out, project, nil
}

// QuotaBar is one bar rendered on the dashboard.
type QuotaBar struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Percentage int    `json:"percentage"`
	ResetTime  string `json:"resetTime,omitempty"`
}

// bucketLabels renames upstream bucket ids to the dashboard's wording.
var bucketLabels = map[string]string{
	"gemini-weekly": "Gemini weekly",
	"gemini-5h":     "Gemini 5h",
	"3p-weekly":     "Claude weekly",
	"3p-5h":         "Claude 5h",
}

// DefaultQuotaBars is the zeroed placeholder set.
func DefaultQuotaBars() []QuotaBar {
	return []QuotaBar{
		{Key: "gemini-weekly", Label: "Gemini weekly"},
		{Key: "gemini-5h", Label: "Gemini 5h"},
		{Key: "3p-weekly", Label: "Claude weekly"},
		{Key: "3p-5h", Label: "Claude 5h"},
	}
}

// BuildQuotaBars renders the summary response into dashboard bars.
func BuildQuotaBars(summary QuotaSummary) []QuotaBar {
	var bars []QuotaBar
	seen := map[string]bool{}

	for _, group := range summary.Groups {
		for _, bucket := range group.Buckets {
			if bucket.BucketID == "" || seen[bucket.BucketID] {
				continue
			}
			seen[bucket.BucketID] = true

			label := bucketLabels[bucket.BucketID]
			if label == "" {
				label = bucket.DisplayName
			}
			if label == "" {
				label = bucket.BucketID
			}
			bars = append(bars, QuotaBar{
				Key:        bucket.BucketID,
				Label:      label,
				Percentage: int(math.Round(bucket.RemainingFraction * 100)),
				ResetTime:  bucket.ResetTime,
			})
		}
	}

	if len(bars) == 0 {
		return DefaultQuotaBars()
	}
	return bars
}

// Model groupings used when only per-model quota is available.
var (
	claudeGptModelIDs = []string{
		"claude-sonnet-4-5", "claude-sonnet-4-5-thinking", "claude-sonnet-4-6",
		"claude-opus-4-5-thinking", "claude-opus-4-6-thinking",
		"gpt-oss-120b", "gpt-oss-120b-medium",
	}
	geminiProModelIDs = []string{
		"gemini-3-pro-low", "gemini-3-pro-high",
		"gemini-3.1-pro-low", "gemini-3.1-pro-high", "gemini-2.5-pro",
	}
	geminiFlashModelIDs = []string{
		"gemini-3-flash", "gemini-2.5-flash", "gemini-2.5-flash-thinking",
		"gemini-3.7-flash", "gemini-3.7-flash-low", "gemini-3.7-flash-medium",
		"gemini-3.7-flash-high", "gemini-3.7-flash-tiered",
		"gemini-3.6-flash-low", "gemini-3.6-flash-medium",
		"gemini-3.6-flash-high", "gemini-3.6-flash-tiered",
		"gemini-3.5-flash-low", "gemini-3.5-flash-extra-low",
	}
)

// BuildModelBars collapses per-model quota into three grouped bars.
func BuildModelBars(models map[string]ModelQuota) []QuotaBar {
	return []QuotaBar{
		mergedBar("claude_gpt", "claude&gpt", models, claudeGptModelIDs),
		mergedBar("gpro", "gpro", models, geminiProModelIDs),
		mergedBar("gflash", "gflash", models, geminiFlashModelIDs),
	}
}

// mergedBar reports the worst remaining fraction in a group, and the soonest
// reset among them.
func mergedBar(key, label string, models map[string]ModelQuota, ids []string) QuotaBar {
	percentage := -1
	var resetTimes []string

	for _, id := range ids {
		info, ok := models[id]
		if !ok {
			continue
		}
		value := int(math.Round(info.RemainingFraction * 100))
		if percentage < 0 || value < percentage {
			percentage = value
		}
		if info.ResetTime != "" {
			resetTimes = append(resetTimes, info.ResetTime)
		}
	}

	if percentage < 0 {
		return QuotaBar{Key: key, Label: label}
	}
	return QuotaBar{
		Key:        key,
		Label:      label,
		Percentage: percentage,
		ResetTime:  earliestResetTime(resetTimes),
	}
}

func earliestResetTime(times []string) string {
	valid := times[:0]
	for _, t := range times {
		if _, err := time.Parse(time.RFC3339, t); err == nil {
			valid = append(valid, t)
		}
	}
	if len(valid) == 0 {
		return ""
	}
	sort.Slice(valid, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339, valid[i])
		b, _ := time.Parse(time.RFC3339, valid[j])
		return a.Before(b)
	})
	return valid[0]
}

// PickResetTime returns the reset time for a model, or the soonest overall.
func PickResetTime(models map[string]ModelQuota, modelID string) string {
	if modelID != "" {
		if info, ok := models[modelID]; ok && info.ResetTime != "" {
			if _, err := time.Parse(time.RFC3339, info.ResetTime); err == nil {
				return info.ResetTime
			}
		}
	}
	var times []string
	for _, info := range models {
		if info.ResetTime != "" {
			times = append(times, info.ResetTime)
		}
	}
	return earliestResetTime(times)
}
