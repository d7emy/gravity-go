package usage

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gravity-go/internal/paths"
)

func useTempDataDir(t *testing.T) {
	t.Helper()
	t.Setenv("GRAVITY_DATA_DIR", t.TempDir())
	Reset()
}

func TestRecordAccumulatesPerModel(t *testing.T) {
	useTempDataDir(t)

	Record("claude-opus-4-6-thinking", 100, 50)
	Record("claude-opus-4-6-thinking", 200, 25)

	report := Get()
	if len(report.Models) != 1 {
		t.Fatalf("got %d models, want 1", len(report.Models))
	}
	if report.Models[0].Input != 300 || report.Models[0].Output != 75 {
		t.Errorf("tokens = %d/%d, want 300/75", report.Models[0].Input, report.Models[0].Output)
	}
}

func TestRecordIgnoresEmptyInput(t *testing.T) {
	useTempDataDir(t)

	Record("", 100, 100)
	Record("some-model", 0, 0)

	if models := Get().Models; len(models) != 0 {
		t.Errorf("got %d models, want none recorded", len(models))
	}
}

func TestModelsSortByCostDescending(t *testing.T) {
	useTempDataDir(t)

	Record("gemini-3.7-flash-tiered", 1000, 1000)
	Record("claude-opus-4-6-thinking", 1_000_000, 1_000_000)

	models := Get().Models
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	if models[0].Cost < models[1].Cost {
		t.Errorf("models are not sorted by cost: %v", models)
	}
}

// Regression: usage writes are debounced by 5s. Without a flush on shutdown,
// tokens recorded just before exit were silently lost.
func TestFlushPersistsDebouncedUsage(t *testing.T) {
	useTempDataDir(t)

	Record("claude-opus-4-6-thinking", 500, 250)

	// The debounce has not fired yet, so nothing is on disk.
	Flush()

	data, err := os.ReadFile(paths.DataFile("usage.json"))
	if err != nil {
		t.Fatalf("usage.json was not written: %v", err)
	}

	var parsed struct {
		Models map[string]struct {
			Input  int64 `json:"input"`
			Output int64 `json:"output"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("usage.json is not valid JSON: %v", err)
	}

	entry, ok := parsed.Models["claude-opus-4-6-thinking"]
	if !ok {
		t.Fatalf("model missing from the flushed file: %v", parsed.Models)
	}
	if entry.Input != 500 || entry.Output != 250 {
		t.Errorf("flushed tokens = %d/%d, want 500/250", entry.Input, entry.Output)
	}
}

func TestLoadRestoresPersistedUsage(t *testing.T) {
	useTempDataDir(t)

	Record("gemini-pro-agent", 1234, 567)
	Flush()

	// Simulate a restart: drop the in-memory cache only. Reset() would also wipe
	// the file, since it backs the dashboard's "reset usage" button.
	mu.Lock()
	cache = usageData{Models: map[string]modelUsage{}}
	dirty = false
	mu.Unlock()

	Load()

	models := Get().Models
	if len(models) != 1 || models[0].Model != "gemini-pro-agent" {
		t.Fatalf("models after reload = %v", models)
	}
	if models[0].Input != 1234 || models[0].Output != 567 {
		t.Errorf("tokens after reload = %d/%d, want 1234/567", models[0].Input, models[0].Output)
	}
}

// Reset backs the dashboard's "reset usage" button, so it must clear the file
// on disk too, not just the in-memory totals.
func TestResetClearsPersistedFile(t *testing.T) {
	useTempDataDir(t)

	Record("claude-opus-4-6-thinking", 10, 10)
	Flush()
	Reset()

	mu.Lock()
	cache = usageData{Models: map[string]modelUsage{}}
	dirty = false
	mu.Unlock()
	Load()

	if models := Get().Models; len(models) != 0 {
		t.Errorf("usage survived a reset: %v", models)
	}
}

// Regression: TotalCost used to accumulate the already-rounded per-model cost.
// Several models each costing a fraction of a cent all rounded to 0.00, so the
// dashboard showed "total $0.00" next to a daily figure of $0.01 — the daily
// number accumulates unrounded and rounds once, and the two disagreed.
func TestTotalCostSumsBeforeRounding(t *testing.T) {
	useTempDataDir(t)

	// Each row costs a fraction of a cent and rounds to 0.00 on its own, while
	// together they come to ~$0.0073 and round to $0.01. The pairing matters:
	// the three models are priced very differently, so moving these counts onto
	// the wrong models pushes one row over a cent and hides the defect.
	Record("gemini-3.7-flash-tiered", 2000, 800) // flash rate  -> ~$0.0045
	Record("claude-opus-4-6-thinking", 116, 7)   // claude rate -> ~$0.0008
	Record("gemini-pro-agent", 93, 153)          // gemini rate -> ~$0.0020

	for _, m := range Get().Models {
		if m.Cost != 0 {
			t.Fatalf("precondition: %s rounds to %v, want every row to round to 0",
				m.Model, m.Cost)
		}
	}

	report := Get()

	var roundedSum float64
	for _, m := range report.Models {
		roundedSum += m.Cost
	}
	if report.TotalCost <= 0 {
		t.Errorf("TotalCost = %v, want > 0; sub-cent models must not round away "+
			"(sum of rounded rows was %v)", report.TotalCost, roundedSum)
	}

	// The total must agree with the day's figure, which is built the same way.
	if len(report.Daily) != 1 {
		t.Fatalf("got %d daily rows, want 1", len(report.Daily))
	}
	if report.TotalCost != report.Daily[0].Cost {
		t.Errorf("TotalCost = %v but daily cost = %v; the two are the same money",
			report.TotalCost, report.Daily[0].Cost)
	}
}

// The dashboard shows what rate priced each row, so the report has to carry it
// and it has to be the rate actually applied — not a second, drifting copy.
func TestReportCarriesTheRateThatPricedEachRow(t *testing.T) {
	useTempDataDir(t)

	Record("gemini-3.7-flash-tiered", 1_000_000, 1_000_000)
	Record("claude-opus-4-6-thinking", 1_000_000, 1_000_000)

	report := Get()
	if len(report.Models) != 2 {
		t.Fatalf("got %d models, want 2", len(report.Models))
	}

	for _, m := range report.Models {
		if m.RateBasis == "" {
			t.Errorf("%s: RateBasis empty; the UI needs to name the price family", m.Model)
		}
		// One million tokens each way means the cost must equal the rates.
		if m.InputCost != round2(m.InputRate) {
			t.Errorf("%s: inputCost %v does not match inputRate %v on 1M tokens",
				m.Model, m.InputCost, m.InputRate)
		}
		if m.OutputCost != round2(m.OutputRate) {
			t.Errorf("%s: outputCost %v does not match outputRate %v on 1M tokens",
				m.Model, m.OutputCost, m.OutputRate)
		}
	}

	// Claude is priced above gemini, so the mapping must not be collapsing
	// every model onto one family.
	byModel := map[string]ModelReport{}
	for _, m := range report.Models {
		byModel[m.Model] = m
	}
	if byModel["claude-opus-4-6-thinking"].RateBasis == byModel["gemini-3.7-flash-tiered"].RateBasis {
		t.Error("both models resolved to the same rate family; detectProvider is not discriminating")
	}

	if report.RateNote == "" {
		t.Error("RateNote empty; the figures must not be presented without saying what they are")
	}
	if want := len(modelRates) + len(priceTable); len(report.Rates) != want {
		t.Errorf("got %d rate cards, want %d (the whole table must be inspectable)",
			len(report.Rates), want)
	}

	// Real prices must be labelled apart from the coarse fallbacks, and must
	// come first so the grounded numbers lead.
	if report.Rates[0].Kind != "model" {
		t.Errorf("first rate card kind = %q, want the published model rates listed first",
			report.Rates[0].Kind)
	}
	var models, families int
	for _, r := range report.Rates {
		switch r.Kind {
		case "model":
			models++
		case "family":
			families++
		default:
			t.Errorf("rate card %q has kind %q, want \"model\" or \"family\"", r.Family, r.Kind)
		}
	}
	if models != len(modelRates) || families != len(priceTable) {
		t.Errorf("got %d model / %d family cards, want %d / %d",
			models, families, len(modelRates), len(priceTable))
	}
}

// A row worth a fraction of a cent must still report a usable number, or the
// dashboard can only ever say "$0.00" and the rates cannot be checked.
func TestCostExactSurvivesRounding(t *testing.T) {
	useTempDataDir(t)

	Record("gemini-3.7-flash-tiered", 520, 227)

	m := Get().Models[0]
	if m.Cost != 0 {
		t.Fatalf("precondition: rounded cost = %v, want it to round to 0", m.Cost)
	}
	if m.CostExact <= 0 {
		t.Errorf("CostExact = %v, want the real sub-cent amount", m.CostExact)
	}
}

// Gemini 3.7 flash has a real published price. The coarse gemini family rate is
// $2/$12 — nearly three times the input and over three times the output — so if
// the specific rate ever stops matching, costs silently inflate rather than
// break. This pins the rate and the precedence.
func TestFlashUsesItsPublishedRateNotTheFamilyFallback(t *testing.T) {
	const wantIn, wantOut = 0.75, 3.75

	// Both the upstream id recorded in usage and the client-facing alias must
	// resolve to the same published price.
	for _, id := range []string{
		"gemini-3.7-flash-tiered",
		"gemini-3.7-flash-high",
		"gemini-3.7-flash",
	} {
		p, basis := rateFor(id)
		if p.Input != wantIn || p.Output != wantOut {
			t.Errorf("rateFor(%q) = %v/%v, want %v/%v (the gemini family fallback is %v/%v)",
				id, p.Input, p.Output, wantIn, wantOut,
				priceTable["gemini"].Input, priceTable["gemini"].Output)
		}
		if strings.Contains(basis, "estimate") {
			t.Errorf("rateFor(%q) basis = %q, want a published rate, not an estimate", id, basis)
		}
	}

	// A gemini model with no specific rate must still fall back to the family.
	p, basis := rateFor("gemini-pro-agent")
	if p != priceTable["gemini"] {
		t.Errorf("rateFor(gemini-pro-agent) = %+v, want the gemini family rate %+v", p, priceTable["gemini"])
	}
	if !strings.Contains(basis, "estimate") {
		t.Errorf("basis = %q, want it marked as an estimate", basis)
	}
}
