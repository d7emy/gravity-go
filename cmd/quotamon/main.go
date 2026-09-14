// Command quotamon watches an account's Antigravity quota bars and token
// usage through a running gravity-go proxy, printing deltas between polls.
//
// It can optionally fire a small canary completion each tick so you can tell
// the difference between "no traffic is landing" and "quota is not moving".
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

type bar struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Percentage int    `json:"percentage"`
	ResetTime  string `json:"resetTime"`
}

type accountView struct {
	Provider    string `json:"provider"`
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Enabled     bool   `json:"enabled"`
	Bars        []bar  `json:"bars"`
}

type quotaSnapshot struct {
	Timestamp string        `json:"timestamp"`
	Accounts  []accountView `json:"accounts"`
}

type modelRow struct {
	Model  string `json:"model"`
	Input  int64  `json:"input"`
	Output int64  `json:"output"`
}

type usageReport struct {
	LastUpdated string     `json:"lastUpdated"`
	Models      []modelRow `json:"models"`
}

type sample struct {
	At        time.Time
	QuotaTime time.Time
	Bars      map[string]bar
	BarsOrder []string
	Enabled   bool
	In        int64
	Out       int64
}

func main() {
	baseURL := flag.String("url", "http://127.0.0.1:8964", "gravity-go base URL")
	account := flag.String("account", "bboyghicqjmtbystarasa@gmail.com", "account id (email)")
	interval := flag.Duration("interval", 5*time.Second, "poll interval")
	count := flag.Int("count", 0, "number of polls before exiting (0 = forever)")
	canary := flag.Bool("canary", false, "send completions each poll to prove traffic lands")
	canaryMax := flag.Int("canary-max-tokens", 8192, "max_tokens for the canary request")
	canaryChars := flag.Int("canary-chars", 400, "prompt filler length for the canary request")
	canaryModel := flag.String("canary-model", "gemini-3.8-flash-high", "model for the canary request")
	canaryWorkers := flag.Int("canary-workers", 1, "parallel canary requests per poll")
	stopAtZero := flag.Bool("stop-at-zero", false, "exit when gemini-5h reaches 0%")
	logPath := flag.String("log", "", "optional JSONL file to append samples to")
	flag.Parse()

	client := &http.Client{}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var prev *sample
	var first *sample
	polls := 0
	changed := 0

	fmt.Printf("quotamon: %s account=%s interval=%s canary=%v\n", *baseURL, *account, *interval, *canary)
	if *canary {
		fmt.Printf("canary: model=%s max_tokens=%d chars=%d workers=%d\n", *canaryModel, *canaryMax, *canaryChars, *canaryWorkers)
	}

	for {
		s, err := takeSample(ctx, client, *baseURL, *account)
		if err != nil {
			fmt.Printf("[%s] ERROR: %v\n", time.Now().Format("15:04:05"), err)
		} else {
			if first == nil {
				first = s
			}
			canaryLine := ""
			if *canary {
				ok, rate, other, in, out, firstErr := runCanaryWave(ctx, client, *baseURL, *canaryModel, *canaryMax, *canaryChars, *canaryWorkers)
				s.In += in
				s.Out += out
				canaryLine = fmt.Sprintf("  canary: ok=%d 429=%d err=%d +%d in/+%d out", ok, rate, other, in, out)
				if firstErr != nil {
					canaryLine += fmt.Sprintf("  first-error=%v", firstErr)
				}
			}
			printSample(s, prev, first, canaryLine)
			if prev != nil && anyChanged(prev, s) {
				changed++
			}
			if *logPath != "" {
				appendLog(*logPath, s)
			}
			prev = s
			if *stopAtZero && pct(s, "gemini-5h") == 0 {
				fmt.Printf("[%s] gemini-5h reached 0%%, stopping\n", s.At.Format("15:04:05"))
				break
			}
		}

		polls++
		if *count > 0 && polls >= *count {
			break
		}
		select {
		case <-ctx.Done():
			fmt.Println()
			fmt.Printf("stopped after %d polls, %d with quota movement\n", polls, changed)
			return
		case <-time.After(*interval):
		}
	}
	fmt.Printf("done: %d polls, %d with quota movement\n", polls, changed)
}

func takeSample(ctx context.Context, client *http.Client, baseURL, account string) (*sample, error) {
	var quota quotaSnapshot
	if err := getJSON(ctx, client, baseURL+"/quota/json", &quota); err != nil {
		return nil, fmt.Errorf("quota: %w", err)
	}
	var usage usageReport
	if err := getJSON(ctx, client, baseURL+"/usage", &usage); err != nil {
		return nil, fmt.Errorf("usage: %w", err)
	}

	s := &sample{
		At:   time.Now(),
		Bars: map[string]bar{},
	}
	if t, err := time.Parse(time.RFC3339, quota.Timestamp); err == nil {
		s.QuotaTime = t
	} else {
		s.QuotaTime = time.Now()
	}
	for _, acct := range quota.Accounts {
		if acct.AccountID != account {
			continue
		}
		s.Enabled = acct.Enabled
		for _, b := range acct.Bars {
			s.Bars[b.Key] = b
			s.BarsOrder = append(s.BarsOrder, b.Key)
		}
	}
	if len(s.Bars) == 0 {
		return nil, fmt.Errorf("account %s not found in /quota/json", account)
	}
	for _, m := range usage.Models {
		s.In += m.Input
		s.Out += m.Output
	}
	return s, nil
}

func getJSON(ctx context.Context, client *http.Client, url string, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 160))
	}
	return json.Unmarshal(body, out)
}

func sendCanary(ctx context.Context, client *http.Client, baseURL, model string, maxTokens, chars int) (int, int64, int64, error) {
	unit := "The quick brown fox jumps over the lazy dog. Pack my box with five dozen liquor jugs. "
	var sb strings.Builder
	for sb.Len() < chars {
		sb.WriteString(unit)
	}
	prompt := sb.String() + "\n\nProduce a very long numbered technical manual with at least 300 sections, each 2 sentences. Do not stop early."
	payload := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"stream":     false,
		"messages":   []map[string]any{{"role": "user", "content": prompt}},
	}
	body, _ := json.Marshal(payload)
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return 0, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	var parsed struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return resp.StatusCode, 0, 0, fmt.Errorf("decode: %w", err)
	}
	return resp.StatusCode, parsed.Usage.InputTokens, parsed.Usage.OutputTokens, nil
}

func runCanaryWave(ctx context.Context, client *http.Client, baseURL, model string, maxTokens, chars, workers int) (ok, rate, other int, in, out int64, firstErr error) {
	if workers < 1 {
		workers = 1
	}
	type result struct {
		status  int
		in, out int64
		err     error
	}
	results := make([]result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, ri, ro, err := sendCanary(ctx, client, baseURL, model, maxTokens, chars)
			results[i] = result{status: status, in: ri, out: ro, err: err}
		}(i)
	}
	wg.Wait()
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			if r.status == http.StatusTooManyRequests {
				rate++
			} else {
				other++
			}
			continue
		}
		ok++
		in += r.in
		out += r.out
	}
	return ok, rate, other, in, out, firstErr
}

func printSample(s, prev, first *sample, canaryLine string) {
	age := s.At.Sub(s.QuotaTime).Round(time.Second)
	if age < 0 {
		age = 0
	}
	stale := ""
	if prev != nil && s.QuotaTime.Equal(prev.QuotaTime) {
		stale = " (sample unchanged=cached?)"
	}
	status := "enabled"
	if !s.Enabled {
		status = "DISABLED"
	}
	fmt.Printf("[%s] sample=%s age=%s %s%s\n",
		s.At.Format("15:04:05"), s.QuotaTime.Format("15:04:05Z"), age, status, stale)

	for _, key := range orderBars(s) {
		b := s.Bars[key]
		delta := ""
		if prev != nil {
			if pb, ok := prev.Bars[key]; ok && pb.Percentage != b.Percentage {
				d := b.Percentage - pb.Percentage
				if d > 0 {
					delta = fmt.Sprintf("  (+%d)", d)
				} else {
					delta = fmt.Sprintf("  (%d)", d)
				}
			}
		}
		fmt.Printf("  %-14s %3d%%%s  reset %s (%s)\n",
			key, b.Percentage, delta, shortTime(b.ResetTime), untilText(b.ResetTime))
	}

	tokenDelta := ""
	if prev != nil {
		tokenDelta = fmt.Sprintf("  +%d in / +%d out", s.In-prev.In, s.Out-prev.Out)
	}
	quotaDelta := ""
	if first != nil {
		quotaDelta = fmt.Sprintf("  drop since start: 5h=%d weekly=%d",
			pct(first, "gemini-5h")-pct(s, "gemini-5h"),
			pct(first, "gemini-weekly")-pct(s, "gemini-weekly"))
	}
	fmt.Printf("  usage totals: in=%d out=%d%s%s%s\n", s.In, s.Out, tokenDelta, quotaDelta, canaryLine)
}

func orderBars(s *sample) []string {
	preferred := []string{"gemini-5h", "gemini-weekly", "3p-5h", "3p-weekly"}
	out := make([]string, 0, len(s.Bars))
	seen := map[string]bool{}
	for _, k := range preferred {
		if _, ok := s.Bars[k]; ok {
			out = append(out, k)
			seen[k] = true
		}
	}
	for _, k := range s.BarsOrder {
		if !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	return out
}

func anyChanged(a, b *sample) bool {
	for k, vb := range b.Bars {
		if va, ok := a.Bars[k]; ok && va.Percentage != vb.Percentage {
			return true
		}
	}
	return false
}

func pct(s *sample, key string) int {
	if b, ok := s.Bars[key]; ok {
		return b.Percentage
	}
	return 0
}

func shortTime(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	return t.UTC().Format("01-02 15:04Z")
}

func untilText(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return "?"
	}
	d := time.Until(t).Round(time.Minute)
	if d <= 0 {
		return "now"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("in %dd%dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("in %dh%dm", hours, mins)
	}
	return fmt.Sprintf("in %dm", mins)
}

func appendLog(path string, s *sample) {
	type line struct {
		TS      string         `json:"ts"`
		Sample  string         `json:"sample"`
		In      int64          `json:"in"`
		Out     int64          `json:"out"`
		Enabled bool           `json:"enabled"`
		Bars    map[string]int `json:"bars"`
	}
	l := line{
		TS:      s.At.UTC().Format(time.RFC3339),
		Sample:  s.QuotaTime.UTC().Format(time.RFC3339),
		In:      s.In,
		Out:     s.Out,
		Enabled: s.Enabled,
		Bars:    map[string]int{},
	}
	for k, b := range s.Bars {
		l.Bars[k] = b.Percentage
	}
	data, _ := json.Marshal(l)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
