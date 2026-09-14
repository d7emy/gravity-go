package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
)

// TestProbeUpstreamModelCatalogue asks Google which model ids actually exist for
// this account, with their remaining quota. Skipped unless PROBE=1, because it
// needs real credentials and makes one live call.
//
// This is the tool that settles "does upstream accept model X". The catalogue is
// rollout-dependent: at time of writing the 3.6 flash family exposes
// -high/-low/-medium/-tiered while the 3.7 family exposes only -tiered, which is
// why every 3.7 flash variant must map onto gemini-3.7-flash-tiered.
//
//	PROBE=1 GRAVITY_DATA_DIR=$HOME/.gravity-go go test ./internal/antigravity/ //	  -run TestProbeUpstreamModelCatalogue -v
func TestProbeUpstreamModelCatalogue(t *testing.T) {
	if os.Getenv("PROBE") == "" {
		t.Skip("set PROBE=1")
	}
	InitAuth()
	Accounts.Load()

	acct := Accounts.NextAvailable(context.Background(), false)
	if acct == nil {
		t.Fatal("no account available")
	}

	models, project, err := FetchModels(context.Background(), acct.AccessToken, acct.ProjectID)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	fmt.Println("project:", project, "| models:", len(models))

	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("  %-40s remaining=%5.1f%%  reset=%s\n",
			n, models[n].RemainingFraction*100, models[n].ResetTime)
	}
}

// TestProbeQuotaSummaryRaw dumps the raw retrieveUserQuotaSummary payload.
//
// This is the endpoint the dashboard bars come from, and it reports something
// different from the per-model catalogue above: a cost-weighted weekly budget
// shared across a model group ("Gemini Flash, Gemini Pro" in one bucket), not a
// per-model request allowance. When the two disagree, this one is the binding
// limit. Skipped unless PROBE=1.
func TestProbeQuotaSummaryRaw(t *testing.T) {
	if os.Getenv("PROBE") == "" {
		t.Skip("set PROBE=1")
	}
	InitAuth()
	Accounts.Load()

	acct := Accounts.NextAvailable(context.Background(), false)
	if acct == nil {
		t.Fatal("no account")
	}

	resp, err := postCloudCode(context.Background(), "/v1internal:retrieveUserQuotaSummary",
		acct.AccessToken, map[string]any{"project": acct.ProjectID})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	var pretty any
	_ = json.Unmarshal([]byte(resp.Body), &pretty)
	out, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Println(string(out))
}
