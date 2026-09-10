package antigravity

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"gravity-go/internal/apperr"
	"gravity-go/internal/appstate"
)

// Search model and limits – mirrors src/services/antigravity/search.ts
const (
	defaultSearchModel    = "gemini-3.7-flash-tiered"
	searchThinkingBudget  = 32768
	searchMaxOutputTokens = 64000
	searchEndpoint        = "/v1internal:generateContent"
	searchFetchTimeout    = 60 * time.Second
	maxSearchQueryLength  = 2000
)

func GetSearchModel() string {
	// Support both prefixes like other env vars; original uses ANTI_API_SEARCH_MODEL
	if v := strings.TrimSpace(getEnv("GRAVITY_SEARCH_MODEL", "ANTI_API_SEARCH_MODEL")); v != "" {
		return v
	}
	return defaultSearchModel
}

func getEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// SearchSource is one grounded URL.
type SearchSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// SearchCitation links a text segment to source indexes.
type SearchCitation struct {
	Text          string `json:"text"`
	SourceIndexes []int  `json:"sourceIndexes"`
}

// SearchResult is the JSON payload for /search.
type SearchResult struct {
	Query     string           `json:"query"`
	Answer    string           `json:"answer"`
	Sources   []SearchSource   `json:"sources"`
	Citations []SearchCitation `json:"citations"`
	Searched  []string         `json:"searched"`
	Model     string           `json:"model"`
	ElapsedMs int64            `json:"elapsedMs"`
}

func buildSearchRequest(query, model, projectID string) map[string]any {
	if projectID == "" {
		projectID = appstate.ProjectID()
		if projectID == "" {
			projectID = "unknown"
		}
	}
	return map[string]any{
		"model":       model,
		"userAgent":   UserAgent(),
		"requestType": "agent",
		"project":     projectID,
		"requestId":   generateRequestID(),
		"request": map[string]any{
			"contents": []any{
				map[string]any{
					"role":  "user",
					"parts": []any{map[string]any{"text": query}},
				},
			},
			"generationConfig": map[string]any{
				"maxOutputTokens": searchMaxOutputTokens,
				"thinkingConfig": map[string]any{
					"includeThoughts": true,
					"thinkingBudget":  searchThinkingBudget,
				},
			},
			"tools": []any{map[string]any{"googleSearch": map[string]any{}}},
		},
	}
}

// ParseSearchResponse decodes the upstream generateContent response.
func ParseSearchResponse(raw, query, model string, elapsedMs int64) (SearchResult, error) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return SearchResult{}, apperr.NewUpstream("antigravity", 502, "Search response was not valid JSON", "")
	}
	payload := parsed
	if resp, ok := parsed["response"].(map[string]any); ok {
		payload = resp
	}
	candidates, _ := payload["candidates"].([]any)
	var firstCand map[string]any
	if len(candidates) > 0 {
		firstCand, _ = candidates[0].(map[string]any)
	}
	grounding, _ := firstCand["groundingMetadata"].(map[string]any)
	if grounding == nil {
		grounding = map[string]any{}
	}

	// Answer: concatenate non-thought text parts
	answer := ""
	if content, ok := firstCand["content"].(map[string]any); ok {
		if parts, ok := content["parts"].([]any); ok {
			for _, p := range parts {
				pm, _ := p.(map[string]any)
				if pm == nil {
					continue
				}
				if thought, _ := pm["thought"].(bool); thought {
					continue
				}
				if text, _ := pm["text"].(string); text != "" {
					answer += text
				}
			}
		}
	}

	// Sources dedup
	type chunk struct {
		URL   string
		Title string
	}
	var rawChunks []map[string]any
	if v, ok := grounding["groundingChunks"].([]any); ok {
		for _, c := range v {
			if m, ok := c.(map[string]any); ok {
				rawChunks = append(rawChunks, m)
			}
		}
	}
	sources := []SearchSource{}
	indexRemap := map[int]int{}
	for i, ch := range rawChunks {
		web, _ := ch["web"].(map[string]any)
		if web == nil {
			continue
		}
		urlStr, _ := web["uri"].(string)
		if urlStr == "" {
			continue
		}
		// dedup by url
		existing := -1
		for idx, s := range sources {
			if s.URL == urlStr {
				existing = idx
				break
			}
		}
		if existing >= 0 {
			indexRemap[i] = existing
			continue
		}
		title, _ := web["title"].(string)
		if title == "" {
			// best-effort hostname extraction; not importing net/url for minimal deps
			title = urlStr
			if idx := strings.Index(urlStr, "://"); idx >= 0 {
				rest := urlStr[idx+3:]
				if slash := strings.Index(rest, "/"); slash >= 0 {
					title = rest[:slash]
				} else {
					title = rest
				}
			}
		}
		indexRemap[i] = len(sources)
		sources = append(sources, SearchSource{Title: title, URL: urlStr})
	}

	// Citations
	citations := []SearchCitation{}
	if supports, ok := grounding["groundingSupports"].([]any); ok {
		for _, s := range supports {
			sm, _ := s.(map[string]any)
			if sm == nil {
				continue
			}
			segment, _ := sm["segment"].(map[string]any)
			text, _ := segment["text"].(string)
			if text == "" {
				continue
			}
			indicesRaw, _ := sm["groundingChunkIndices"].([]any)
			mapped := []int{}
			for _, idxRaw := range indicesRaw {
				var idx int
				switch v := idxRaw.(type) {
				case float64:
					idx = int(v)
				case int:
					idx = v
				}
				if remapped, ok := indexRemap[idx]; ok {
					mapped = append(mapped, remapped)
				}
			}
			if len(mapped) == 0 {
				continue
			}
			// dedup mapped
			seen := map[int]bool{}
			uniq := []int{}
			for _, v := range mapped {
				if !seen[v] {
					seen[v] = true
					uniq = append(uniq, v)
				}
			}
			citations = append(citations, SearchCitation{Text: text, SourceIndexes: uniq})
		}
	}

	searched := []string{}
	if qs, ok := grounding["webSearchQueries"].([]any); ok {
		for _, q := range qs {
			if s, ok := q.(string); ok && s != "" {
				searched = append(searched, s)
			}
		}
	}
	if sources == nil {
		sources = []SearchSource{}
	}
	if citations == nil {
		citations = []SearchCitation{}
	}
	if searched == nil {
		searched = []string{}
	}

	return SearchResult{
		Query:     query,
		Answer:    strings.TrimSpace(answer),
		Sources:   sources,
		Citations: citations,
		Searched:  searched,
		Model:     model,
		ElapsedMs: elapsedMs,
	}, nil
}

// PerformWebSearch runs one grounded search, failing over across baseURLs.
func PerformWebSearch(ctx context.Context, query string, modelOpt string) (SearchResult, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return SearchResult{}, apperr.NewUpstream("antigravity", 400, "Search query is empty", "")
	}
	if len(trimmed) > maxSearchQueryLength {
		return SearchResult{}, apperr.NewUpstream("antigravity", 400, "Query too long", "")
	}
	model := strings.TrimSpace(modelOpt)
	if model == "" {
		model = GetSearchModel()
	}

	accessToken, projectID, err := resolveSearchAccount(ctx)
	if err != nil {
		return SearchResult{}, err
	}

	bodyMap := buildSearchRequest(trimmed, model, projectID)
	bodyBytes, err := json.Marshal(bodyMap)
	if err != nil {
		return SearchResult{}, err
	}
	startedAt := time.Now()
	var lastStatus int
	var lastBody string

	for _, baseURL := range shuffledBaseURLs() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+searchEndpoint, strings.NewReader(string(bodyBytes)))
		if err != nil {
			continue
		}
		setUpstreamHeaders(req, accessToken, "application/json")

		// per-request timeout like chat's headerTimeout but for search we use shortClient with deadline
		reqCtx, cancel := context.WithTimeout(ctx, searchFetchTimeout)
		req = req.WithContext(reqCtx)
		resp, err := shortClient.Do(req)
		if err != nil {
			cancel()
			lastBody = err.Error()
			continue
		}
		respBody, readErr := readLimited(resp)
		resp.Body.Close()
		cancel()
		if readErr != nil {
			lastBody = readErr.Error()
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return ParseSearchResponse(string(respBody), trimmed, model, time.Since(startedAt).Milliseconds())
		}
		lastStatus = resp.StatusCode
		lastBody = string(respBody)
		if lastStatus < 429 {
			break
		}
	}
	if lastStatus == 0 {
		lastStatus = 502
	}
	if lastBody == "" {
		lastBody = "Search failed on all endpoints"
	}
	return SearchResult{}, apperr.NewUpstream("antigravity", lastStatus, lastBody, "")
}

func resolveSearchAccount(ctx context.Context) (string, string, error) {
	// Reuse the same rotation logic as chat; for search we don't need AllowRotation flag.
	if acct := Accounts.NextAvailable(ctx, false); acct != nil {
		return acct.AccessToken, acct.ProjectID, nil
	}
	tok, err := GetAccessToken(ctx)
	if err != nil {
		return "", "", err
	}
	auth := appstate.GetAuth()
	return tok, auth.ProjectID, nil
}

func readLimited(resp *http.Response) ([]byte, error) {
	// Use same 8MiB cap as httpclient.doJSON
	const limit = 8 << 20
	b, err := readAllLimited(resp, limit)
	return b, err
}

func readAllLimited(resp *http.Response, limit int64) ([]byte, error) {
	// simple limited read
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	var total int64
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			total += int64(n)
			if total > limit {
				// truncate
				remaining := limit - int64(len(buf))
				if remaining > 0 {
					buf = append(buf, tmp[:remaining]...)
				}
				break
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}
