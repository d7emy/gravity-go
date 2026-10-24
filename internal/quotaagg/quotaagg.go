// Package quotaagg builds the dashboard's quota view, with a disk cache so a
// slow or failing upstream still renders the last known numbers.
package quotaagg

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"gravity-go/internal/antigravity"
	"gravity-go/internal/apperr"
	"gravity-go/internal/authstore"
	"gravity-go/internal/logx"
	"gravity-go/internal/paths"
)

// ModelOption is one selectable model on an account card.
type ModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// AccountView is one account's card on the dashboard.
type AccountView struct {
	Provider    string                 `json:"provider"`
	AccountID   string                 `json:"accountId"`
	DisplayName string                 `json:"displayName"`
	Bars        []antigravity.QuotaBar `json:"bars"`
	Models      []ModelOption          `json:"models"`
	// Enabled drives the dashboard's per-account switch. It is sent on every
	// snapshot rather than tracked client-side so a reload, a second browser tab
	// or a change made through the API all show the same state.
	Enabled bool `json:"enabled"`
}

// Snapshot is the /quota/json payload.
type Snapshot struct {
	Timestamp string        `json:"timestamp"`
	Accounts  []AccountView `json:"accounts"`
}

type cacheEntry struct {
	Provider    string                 `json:"provider"`
	AccountID   string                 `json:"accountId"`
	DisplayName string                 `json:"displayName"`
	Bars        []antigravity.QuotaBar `json:"bars"`
	UpdatedAt   string                 `json:"updatedAt"`
}

// fetchTimeout bounds the whole refresh so the dashboard never hangs on a slow
// upstream; cached numbers are served instead. The live value is jittered
// 3..5s per call (see antigravity.QuotaFetchTimeout) so dashboard polls across
// processes do not synchronize.
const fetchTimeout = 4 * time.Second

var (
	cacheMu     sync.Mutex
	cache       = map[string]cacheEntry{}
	cacheLoaded bool
)

func cacheFile() string { return paths.DataFile("quota-cache.json") }

func loadCacheLocked() {
	if cacheLoaded {
		return
	}
	cacheLoaded = true
	data, err := os.ReadFile(cacheFile())
	if err != nil {
		return
	}
	var parsed map[string]cacheEntry
	if err := json.Unmarshal(data, &parsed); err != nil {
		return
	}
	cache = parsed
}

func saveCacheLocked() {
	if _, err := paths.EnsureDataDir(); err != nil {
		return
	}
	payload, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	// Best-effort: a failed cache write must never fail the request.
	_ = os.WriteFile(cacheFile(), payload, 0o644)
}

// Get builds the current quota snapshot for every stored account.
func Get(ctx context.Context) Snapshot {
	cacheMu.Lock()
	loadCacheLocked()
	cacheMu.Unlock()

	antigravity.Accounts.Load()
	accounts := authstore.List()

	views := make([]AccountView, len(accounts))
	var wg sync.WaitGroup

	fetchCtx, cancel := context.WithTimeout(ctx, antigravity.QuotaFetchTimeout())
	defer cancel()

	for i, account := range accounts {
		wg.Add(1)
		go func(i int, account authstore.Account) {
			defer wg.Done()
			views[i] = buildAccountView(fetchCtx, account)
		}(i, account)
	}
	wg.Wait()

	cacheMu.Lock()
	saveCacheLocked()
	cacheMu.Unlock()

	return Snapshot{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Accounts:  views,
	}
}

func displayNameFor(account authstore.Account) string {
	if account.Email != "" {
		return account.Email
	}
	if account.Login != "" {
		return account.Login
	}
	return account.ID
}

func cachedBars(accountID string) []antigravity.QuotaBar {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if entry, ok := cache["antigravity:"+accountID]; ok {
		return entry.Bars
	}
	return nil
}

func storeBars(accountID, displayName string, bars []antigravity.QuotaBar) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache["antigravity:"+accountID] = cacheEntry{
		Provider:    "antigravity",
		AccountID:   accountID,
		DisplayName: displayName,
		Bars:        bars,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
}

// VisibleModelOptions lists the models offered on an account card.
func VisibleModelOptions() []ModelOption {
	models := antigravity.FilterVisibleModels(antigravity.AvailableModels)
	out := make([]ModelOption, 0, len(models))
	for _, model := range models {
		out = append(out, ModelOption{ID: model.ID, Label: model.Name})
	}
	return out
}

func buildAccountView(ctx context.Context, account authstore.Account) AccountView {
	displayName := displayNameFor(account)
	view := AccountView{
		Provider:    "antigravity",
		AccountID:   account.ID,
		DisplayName: displayName,
		Models:      VisibleModelOptions(),
		Enabled:     antigravity.Accounts.IsEnabled(account.ID),
	}

	bars, ok := fetchBars(ctx, account)
	switch {
	case ok:
		storeBars(account.ID, displayName, bars)
		view.Bars = bars
	default:
		if cached := cachedBars(account.ID); cached != nil {
			view.Bars = cached
		} else {
			view.Bars = antigravity.DefaultQuotaBars()
		}
	}
	return view
}

// fetchBars refreshes one account's quota.
//
// Token renewal is delegated to the account manager rather than done here, so a
// dashboard poll and an in-flight completion coalesce into a single refresh
// instead of each firing their own at Google.
func fetchBars(ctx context.Context, account authstore.Account) ([]antigravity.QuotaBar, bool) {
	accessToken, projectID := account.AccessToken, account.ProjectID
	if current := antigravity.Accounts.CurrentToken(ctx, account.ID); current != nil {
		accessToken, projectID = current.AccessToken, current.ProjectID
	}

	bars, err := queryQuota(ctx, accessToken, projectID, &account)
	if err == nil {
		return bars, true
	}

	// A 401 on a token the manager thought was fine is worth exactly one forced
	// refresh before giving up and showing cached numbers.
	var upstream *apperr.UpstreamError
	if asUpstream(err, &upstream) && upstream.Status == 401 {
		if refreshed := antigravity.Accounts.ForceRefresh(ctx, account.ID); refreshed != nil {
			if bars, err := queryQuota(ctx, refreshed.AccessToken, refreshed.ProjectID, &account); err == nil {
				return bars, true
			}
		}
	}

	logx.Debug("Antigravity quota fetch failed for %s: %v", account.ID, err)
	return nil, false
}

// queryQuota tries the summary endpoint, falling back to per-model quota, which
// is what accounts without a summary expose.
func queryQuota(
	ctx context.Context, accessToken, projectID string, account *authstore.Account,
) ([]antigravity.QuotaBar, error) {
	summary, err := antigravity.FetchQuotaSummary(ctx, accessToken, projectID)
	if err == nil {
		bars := antigravity.BuildQuotaBars(summary)
		return withGeminiBars(ctx, accessToken, projectID, account.ID, bars), nil
	}

	models, resolvedProject, modelErr := antigravity.FetchModels(ctx, accessToken, projectID)
	if modelErr != nil {
		// Report the summary failure: it is the more informative of the two.
		return nil, err
	}
	if account.ProjectID == "" && resolvedProject != "" {
		account.ProjectID = resolvedProject
		_ = authstore.Save(*account)
	}
	return antigravity.BuildModelBars(models), nil
}

// withGeminiBars adds Gemini quota when the summary leaves it out.
//
// The summary only lists the groups an account is metered on. Some accounts get
// just "Claude and GPT models" there and still call Gemini fine, so their Gemini
// numbers exist only as per-model quota. This is best effort: if that call fails
// the summary bars are shown as they are rather than failing the whole account.
func withGeminiBars(
	ctx context.Context, accessToken, projectID, accountID string, bars []antigravity.QuotaBar,
) []antigravity.QuotaBar {
	if antigravity.HasGeminiBar(bars) {
		return bars
	}

	models, _, err := antigravity.FetchModels(ctx, accessToken, projectID)
	if err != nil {
		logx.Debug("Antigravity per-model quota fetch failed for %s: %v", accountID, err)
		return bars
	}
	gemini, ok := antigravity.BuildGeminiModelBar(models)
	if !ok {
		return bars
	}
	// Gemini first, matching the order of the summary and of the placeholder set.
	return append([]antigravity.QuotaBar{gemini}, bars...)
}

func asUpstream(err error, target **apperr.UpstreamError) bool {
	if err == nil {
		return false
	}
	if upstream, ok := err.(*apperr.UpstreamError); ok {
		*target = upstream
		return true
	}
	return false
}
