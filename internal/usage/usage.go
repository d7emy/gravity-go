// Package usage tracks token spend per model and per day for the dashboard.
package usage

import (
	"encoding/json"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gravity-go/internal/paths"
)

type pricing struct{ Input, Output float64 }

// Coarse per-family prices, per million tokens. These are a fallback for models
// whose real price is not known: they are inherited unchanged from the original
// TypeScript implementation and match no published price sheet exactly. In particular
// they cost a flash model and a pro model identically, which is badly wrong --
// prefer an entry in modelRates over adding anything here.
var priceTable = map[string]pricing{
	"gpt":    {Input: 1.75, Output: 14.0},
	"claude": {Input: 5.0, Output: 25.0},
	"gemini": {Input: 2.0, Output: 12.0},
}

// modelRate is a real, published price for one model.
type modelRate struct {
	// Match is tested as a substring of the lowercased model id, so one entry
	// covers both the upstream id recorded in usage (gemini-3.7-flash-tiered)
	// and the client-facing aliases (gemini-3.7-flash-high).
	Match string
	Label string
	Note  string
	pricing
}

// modelRates are checked in order and take precedence over priceTable. Only add
// a model here when its actual price is known -- a wrong specific number is
// worse than an admitted estimate.
var modelRates = []modelRate{
	{
		Match: "gemini-3.7-flash",
		Label: "gemini 3.7 flash",
		// Currently half price on a promotion, against a $1.50/$7.50 list. The
		// promotional figure is used because it is what these tokens are worth
		// today; this needs revisiting when the promotion ends.
		Note:    "50% promotional discount, from $1.50/$7.50 list",
		pricing: pricing{Input: 0.75, Output: 3.75},
	},
}

// rateFor picks the price for a model and names where that price came from.
func rateFor(model string) (pricing, string) {
	m := strings.ToLower(model)
	for _, r := range modelRates {
		if strings.Contains(m, r.Match) {
			return r.pricing, r.Label
		}
	}
	family := detectProvider(model)
	return priceTable[family], family + " family (estimate)"
}

type modelUsage struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

type dailyUsage struct {
	Date   string  `json:"date"`
	Cost   float64 `json:"cost"`
	Input  int64   `json:"input"`
	Output int64   `json:"output"`
}

type usageData struct {
	LastUpdated string                `json:"lastUpdated"`
	Models      map[string]modelUsage `json:"models"`
	Daily       []dailyUsage          `json:"daily"`
}

var (
	mu        sync.Mutex
	cache     = usageData{Models: map[string]modelUsage{}}
	dirty     bool
	saveTimer *time.Timer
)

func usageFile() string { return paths.DataFile("usage.json") }

// Load reads usage.json into memory. Called once at startup.
func Load() {
	mu.Lock()
	defer mu.Unlock()

	data, err := os.ReadFile(usageFile())
	if err != nil {
		return
	}
	var parsed usageData
	if err := json.Unmarshal(data, &parsed); err != nil {
		return
	}
	if parsed.Models == nil {
		parsed.Models = map[string]modelUsage{}
	}
	if parsed.LastUpdated == "" {
		parsed.LastUpdated = time.Now().UTC().Format(time.RFC3339)
	}
	cache = parsed
}

func saveLocked() {
	if !dirty {
		return
	}
	cache.LastUpdated = time.Now().UTC().Format(time.RFC3339)
	if _, err := paths.EnsureDataDir(); err != nil {
		return
	}
	payload, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(usageFile(), payload, 0o644); err == nil {
		dirty = false
	}
}

// scheduleSaveLocked debounces the write by 5s, matching the TS behaviour.
func scheduleSaveLocked() {
	dirty = true
	if saveTimer != nil {
		return
	}
	saveTimer = time.AfterFunc(5*time.Second, func() {
		mu.Lock()
		defer mu.Unlock()
		saveLocked()
		saveTimer = nil
	})
}

func detectProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gpt"), strings.Contains(m, "o1"),
		strings.Contains(m, "o3"), strings.Contains(m, "o4"):
		return "gpt"
	case strings.Contains(m, "claude"), strings.Contains(m, "opus"),
		strings.Contains(m, "sonnet"):
		return "claude"
	case strings.Contains(m, "gemini"), strings.Contains(m, "flash"),
		strings.Contains(m, "pro"):
		return "gemini"
	}
	// Antigravity's native ids are mostly Claude-priced.
	return "claude"
}

func todayString() string { return time.Now().Format("2006-01-02") }

// Record adds token counts for one completed request.
func Record(model string, inputTokens, outputTokens int64) {
	if model == "" || (inputTokens <= 0 && outputTokens <= 0) {
		return
	}
	mu.Lock()
	defer mu.Unlock()

	existing := cache.Models[model]
	cache.Models[model] = modelUsage{
		Input:  existing.Input + inputTokens,
		Output: existing.Output + outputTokens,
	}

	p, _ := rateFor(model)
	requestCost := float64(inputTokens)/1_000_000*p.Input + float64(outputTokens)/1_000_000*p.Output

	today := todayString()
	found := false
	for i := range cache.Daily {
		if cache.Daily[i].Date == today {
			cache.Daily[i].Cost += requestCost
			cache.Daily[i].Input += inputTokens
			cache.Daily[i].Output += outputTokens
			found = true
			break
		}
	}
	if !found {
		cache.Daily = append(cache.Daily, dailyUsage{
			Date: today, Cost: requestCost, Input: inputTokens, Output: outputTokens,
		})
		if len(cache.Daily) > 14 {
			cache.Daily = cache.Daily[len(cache.Daily)-14:]
		}
	}

	scheduleSaveLocked()
}

// ModelReport is one row of the usage table.
//
// InputRate/OutputRate are the USD-per-million-token rates actually used to
// price this row, and RateBasis names the family those rates came from. They
// are reported so the dashboard can show its working rather than presenting an
// unexplained dollar figure.
type ModelReport struct {
	Model      string  `json:"model"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	InputCost  float64 `json:"inputCost"`
	OutputCost float64 `json:"outputCost"`
	Cost       float64 `json:"cost"`
	InputRate  float64 `json:"inputRate"`
	OutputRate float64 `json:"outputRate"`
	RateBasis  string  `json:"rateBasis"`
	// CostExact is the same figure without the two-decimal rounding. A row
	// costing a fraction of a cent renders as "$0.00" otherwise, which reads as
	// "free" rather than "small".
	CostExact float64 `json:"costExact"`
}

// RateCard is one entry of the price table, in USD per million tokens. Kind is
// "model" for a real published price and "family" for a coarse fallback, so the
// dashboard can show which figures are grounded and which are guesses.
type RateCard struct {
	Family string  `json:"family"`
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Kind   string  `json:"kind"`
	Note   string  `json:"note,omitempty"`
}

// DailyReport is one bar of the daily chart. CostExact carries the unrounded
// figure for the same reason ModelReport does: a day's real traffic can easily
// come to less than a cent, and rounding it renders the whole chart flat.
type DailyReport struct {
	Date      string  `json:"date"`
	Cost      float64 `json:"cost"`
	CostExact float64 `json:"costExact"`
	Input     int64   `json:"input"`
	Output    int64   `json:"output"`
}

// Report is the /usage payload.
type Report struct {
	LastUpdated string        `json:"lastUpdated"`
	Today       string        `json:"today"`
	Models      []ModelReport `json:"models"`
	TotalCost   float64       `json:"totalCost"`
	TotalExact  float64       `json:"totalCostExact"`
	Daily       []DailyReport `json:"daily"`
	Rates       []RateCard    `json:"rates"`
	RateNote    string        `json:"rateNote"`
}

// rateNote states plainly what the dollar figures are and are not. Antigravity
// bills against a quota, not per token, so every cost here is a synthetic
// estimate produced from these rates -- never an amount anyone was charged.
const rateNote = "Estimates only. Antigravity bills against a weekly quota, not per token, " +
	"so no figure here was actually charged. Model rates are real published prices. Family " +
	"rates are a fallback for everything else: they are inherited from the original TypeScript " +
	"implementation, match no current price sheet, and cost a flash model the same as a pro model."

// Rates returns the whole price table: the specific model prices first, then the
// family fallbacks, each group in a stable order.
func Rates() []RateCard {
	cards := make([]RateCard, 0, len(modelRates)+len(priceTable))
	for _, r := range modelRates {
		cards = append(cards, RateCard{
			Family: r.Label, Input: r.Input, Output: r.Output,
			Kind: "model", Note: r.Note,
		})
	}

	families := make([]RateCard, 0, len(priceTable))
	for family, p := range priceTable {
		families = append(families, RateCard{
			Family: family, Input: p.Input, Output: p.Output, Kind: "family",
		})
	}
	sort.Slice(families, func(i, j int) bool { return families[i].Family < families[j].Family })
	return append(cards, families...)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Get builds the usage report.
func Get() Report {
	mu.Lock()
	defer mu.Unlock()

	models := make([]ModelReport, 0, len(cache.Models))
	var totalCost float64
	for model, u := range cache.Models {
		p, basis := rateFor(model)
		inputCost := float64(u.Input) / 1_000_000 * p.Input
		outputCost := float64(u.Output) / 1_000_000 * p.Output
		row := ModelReport{
			Model:      model,
			Input:      u.Input,
			Output:     u.Output,
			InputCost:  round2(inputCost),
			OutputCost: round2(outputCost),
			Cost:       round2(inputCost + outputCost),
			InputRate:  p.Input,
			OutputRate: p.Output,
			RateBasis:  basis,
			CostExact:  inputCost + outputCost,
		}
		models = append(models, row)
		// Accumulate the unrounded cost. Summing row.Cost instead would round
		// each model to two decimals first, so a set of sub-cent models totalled
		// to zero while the daily figure -- which accumulates unrounded and
		// rounds once at the end -- showed the real amount.
		totalCost += inputCost + outputCost
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Cost > models[j].Cost })

	daily := make([]DailyReport, 0, len(cache.Daily))
	for _, d := range cache.Daily {
		daily = append(daily, DailyReport{
			Date: d.Date, Cost: round2(d.Cost), CostExact: d.Cost,
			Input: d.Input, Output: d.Output,
		})
	}

	lastUpdated := cache.LastUpdated
	if lastUpdated == "" {
		lastUpdated = time.Now().UTC().Format(time.RFC3339)
	}
	return Report{
		LastUpdated: lastUpdated,
		Today:       todayString(),
		Models:      models,
		TotalCost:   round2(totalCost),
		TotalExact:  totalCost,
		Daily:       daily,
		Rates:       Rates(),
		RateNote:    rateNote,
	}
}

// Flush writes any debounced usage immediately. Called on shutdown so the last
// few seconds of recorded tokens are not lost to the 5s debounce.
func Flush() {
	mu.Lock()
	defer mu.Unlock()
	if saveTimer != nil {
		saveTimer.Stop()
		saveTimer = nil
	}
	saveLocked()
}

// Reset clears all recorded usage.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	cache = usageData{
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
		Models:      map[string]modelUsage{},
		Daily:       nil,
	}
	dirty = true
	saveLocked()
}
