package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gravity-go/internal/apperr"
	"gravity-go/internal/appstate"
	"gravity-go/internal/logx"
	"gravity-go/internal/usage"
)

// baseURLs are tried in order. The daily endpoint is listed first: it is the
// most stable in practice.
var baseURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com",
	"https://daily-cloudcode-pa.sandbox.googleapis.com",
	"https://cloudcode-pa.googleapis.com",
}

// SetBaseURLsForTest overrides the upstream hosts for contained tests.
func SetBaseURLsForTest(urls []string) { baseURLs = urls }

const streamEndpoint = "/v1internal:streamGenerateContent"

const (
	// maxRetryAttempts stays low on purpose: aggressive retrying against a
	// rate-limited credential just cascades more 429s.
	maxRetryAttempts = 1

	maxNonQuota429Retries = 2
	maxNonQuota429Wait    = 4 * time.Second
	nonQuota429Cooldown   = 8 * time.Second

	// headerTimeout bounds time-to-first-byte, not the whole stream.
	headerTimeout = 30 * time.Second
	// idleTimeout aborts a stream that goes quiet mid-response.
	idleTimeout = 15 * time.Minute
)

// shouldTryNextEndpoint reports whether a status is worth retrying on a
// different host. 429 is deliberately excluded: it is account-specific, not
// endpoint-specific.
func shouldTryNextEndpoint(status int) bool {
	return status == 403 || status == 408 || status == 404 || status >= 500
}

// retryablePriority ranks errors so the most actionable one is surfaced when
// every endpoint fails.
func retryablePriority(status int) int {
	switch {
	case status == 529:
		return 70
	case status >= 500:
		return 60
	case status == 429:
		return 50
	case status == 403:
		return 45
	case status == 408:
		return 40
	case status == 404:
		return 10
	}
	return 0
}

// doUpstream issues one upstream call. The returned cancel must be called by the
// caller once it is finished with the body.
func doUpstream(
	ctx context.Context, url string, payload []byte, accessToken string,
) (*http.Response, context.CancelFunc, error) {
	reqCtx, cancel := context.WithCancel(ctx)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	setUpstreamHeaders(req, accessToken, "text/event-stream")

	// Bound time-to-first-byte only. Once headers land the stream may legitimately
	// run for many minutes, so the timer is stopped rather than left to fire.
	headerTimer := time.AfterFunc(headerTimeout, cancel)
	resp, err := streamClient.Do(req)
	headerTimer.Stop()

	if err != nil {
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}

// attemptState carries the mutable pieces of the retry loop.
type attemptState struct {
	accessToken string
	accountID   string

	nonQuota429Count   int
	signatureRepaired  bool
	projectRefreshDone map[string]bool

	preferredStatus     int
	preferredBody       string
	preferredRetryAfter string
}

// classifyFailure decides what to do about one non-2xx response. The returned
// action is "retry" (restart the attempt loop), "next" (try the next endpoint),
// or "fail" (surface err).
func (s *attemptState) classifyFailure(
	ctx context.Context,
	status int,
	body string,
	retryAfter string,
	request map[string]any,
	allowRotation bool,
) (action string, err error) {
	if traceID := ExtractTraceID(body); traceID != "" {
		logx.Raw(fmt.Sprintf("[%s] %d upstream traceId=%s", logx.FormatTime(), status, traceID))
	}

	// A stale project id: re-resolve once per account and retry.
	if status == 404 && s.accountID != "" && !s.projectRefreshDone[s.accountID] {
		s.projectRefreshDone[s.accountID] = true
		if missing := extractMissingProjectID(body); missing != "" {
			if refreshed := Accounts.RefreshProjectID(ctx, s.accountID, s.accessToken); refreshed != "" {
				request["project"] = refreshed
				logx.Warn("[Antigravity] Refreshed project for %s: %s -> %s",
					s.accountID, missing, refreshed)
				return "retry", nil
			}
		}
	}

	// A repairable 400: fix the request in place and retry once.
	if !s.signatureRepaired {
		if repaired := RepairBadRequest(request, status, body); repaired != "" {
			s.signatureRepaired = true
			// Logged unconditionally: this is the line that tells you a turn was
			// rescued instead of dying.
			logx.Raw(fmt.Sprintf("[%s] recovered: %s", logx.FormatTime(), repaired))
			return "retry", nil
		}
	}

	if status == 429 && s.accountID != "" {
		return s.classify429(ctx, body, retryAfter, request, allowRotation)
	}

	// 401: refresh this account's token, else rotate.
	if status == 401 && s.accountID != "" {
		if refreshed := Accounts.ByID(ctx, s.accountID); refreshed != nil {
			s.accessToken = refreshed.AccessToken
			return "retry", nil
		}
		if allowRotation && Accounts.Count() > 1 {
			if next := Accounts.NextExcluding(ctx, s.accountID); next != nil && next.AccountID != s.accountID {
				s.accessToken = next.AccessToken
				s.accountID = next.AccountID
				request["project"] = next.ProjectID
				return "retry", nil
			}
		}
		return "fail", apperr.NewUpstream("antigravity", 401, body, retryAfter)
	}

	if shouldTryNextEndpoint(status) {
		if retryablePriority(status) > retryablePriority(s.preferredStatus) {
			s.preferredStatus = status
			s.preferredBody = body
			s.preferredRetryAfter = retryAfter
		}
		return "next", nil
	}

	return "fail", apperr.NewUpstream("antigravity", status, body, retryAfter)
}

// classify429 splits transient throttling (wait, keep the account) from a spent
// quota (rotate to another account).
func (s *attemptState) classify429(
	ctx context.Context,
	body string,
	retryAfter string,
	request map[string]any,
	allowRotation bool,
) (string, error) {
	quotaExhausted := isQuotaExhaustedErrorText(body)

	delay := 2 * time.Second
	if parsed, ok := ParseRetryDelay(body, retryAfter); ok {
		delay = parsed
	}
	if delay > maxNonQuota429Wait {
		delay = maxNonQuota429Wait
	}

	// Transient throttling: keep the account, just wait it out.
	if !quotaExhausted && s.nonQuota429Count < maxNonQuota429Retries {
		// Reserve the account during the wait so concurrent requests do not
		// pick it and immediately eat another 429. Both hold and sleep carry
		// small jitter so parallel processes do not retry in lock-step; the
		// mandated delay+200ms floor is always preserved.
		holdJitter := retryHoldJitter()
		release := Accounts.HoldRateLimit(s.accountID, delay+500*time.Millisecond+holdJitter)
		select {
		case <-time.After(delay + 200*time.Millisecond + retrySleepJitter()):
		case <-ctx.Done():
			if release != nil {
				release()
			}
			return "fail", ctx.Err()
		}
		if release != nil {
			release()
		}
		s.nonQuota429Count++
		return "retry", nil
	}

	if quotaExhausted {
		outcome := Accounts.MarkRateLimitedFromError(s.accountID, 429, body, retryAfter)
		if outcome != nil && outcome.Reason == reasonQuotaExhausted {
			Accounts.MoveToEndOfQueue(s.accountID)
		}
		if allowRotation && Accounts.Count() > 1 {
			if next := Accounts.NextExcluding(ctx, s.accountID); next != nil && next.AccountID != s.accountID {
				s.accessToken = next.AccessToken
				s.accountID = next.AccountID
				request["project"] = next.ProjectID
				return "retry", nil
			}
		}
		return "fail", apperr.NewUpstream("antigravity", 429, body, retryAfter)
	}

	// Out of in-place retries on a non-quota 429: cool the account down and
	// rotate if we can. Jitter spreads re-probes so demoted accounts do not
	// all return at the same instant.
	cooldown := delay
	if cooldown < nonQuota429Cooldown {
		cooldown = nonQuota429Cooldown
	}
	Accounts.MarkRateLimited(s.accountID, cooldown+cooldownJitter())

	if allowRotation && Accounts.Count() > 1 {
		if next := Accounts.NextExcluding(ctx, s.accountID); next != nil && next.AccountID != s.accountID {
			s.accessToken = next.AccessToken
			s.accountID = next.AccountID
			request["project"] = next.ProjectID
			s.nonQuota429Count = 0
			return "retry", nil
		}
	}

	upstream := apperr.NewUpstream("antigravity", 429, body, retryAfter)
	upstream.Retryable = true
	return "fail", upstream
}

// sendRequestBuffered runs the non-streaming path: it drains the SSE body and
// returns the decoded chunks.
func sendRequestBuffered(
	ctx context.Context,
	request map[string]any,
	accessToken, accountID string,
	allowRotation bool,
	modelName, routeTag string,
) ([]upstreamChunk, error) {
	startTime := time.Now()
	state := &attemptState{
		accessToken:        accessToken,
		accountID:          accountID,
		projectRefreshDone: map[string]bool{},
	}

	rotationBudget := 0
	if allowRotation {
		rotationBudget = Accounts.Count() - 1
		if rotationBudget < 0 {
			rotationBudget = 0
		}
	}
	maxAttempts := maxRetryAttempts
	if n := maxNonQuota429Retries + 1 + rotationBudget; n > maxAttempts {
		maxAttempts = n
	}

	var lastStatus int
	var lastBody, lastRetryAfter string

	for attempt := 0; attempt < maxAttempts; attempt++ {
		retryAttempt := false

		// Host order is shuffled per attempt so this client does not sweep
		// endpoints in clone-identical order. Every host is still tried.
		for _, baseURL := range shuffledBaseURLs() {
			payload, err := json.Marshal(request)
			if err != nil {
				return nil, err
			}

			resp, cancel, err := doUpstream(ctx, baseURL+streamEndpoint+"?alt=sse",
				payload, state.accessToken)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				logx.Debug("[SSE] %s: %v", baseURL, err)
				continue
			}

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				cancel()
				if readErr != nil {
					logx.Debug("[SSE] read error from %s: %v", baseURL, readErr)
					continue
				}

				if state.accountID != "" {
					Accounts.MarkSuccess(state.accountID)
				}
				logSuccess(startTime, modelName, state.accountID, routeTag)
				return collectSSEChunks(string(body)), nil
			}

			rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			retryAfter := resp.Header.Get("Retry-After")
			resp.Body.Close()
			cancel()

			lastStatus = resp.StatusCode
			lastBody = string(rawBody)
			lastRetryAfter = retryAfter
			logx.Warn("SSE error %d %s", lastStatus, truncate(lastBody, 200))

			action, failErr := state.classifyFailure(
				ctx, lastStatus, lastBody, lastRetryAfter, request, allowRotation)
			switch action {
			case "retry":
				retryAttempt = true
			case "next":
				continue
			case "fail":
				return nil, failErr
			}
			break // leave the endpoint loop and start a fresh attempt
		}

		if retryAttempt {
			continue
		}

		if lastStatus > 0 && attempt < maxAttempts-1 {
			strategy := DetermineRetryStrategy(lastStatus, lastBody, lastRetryAfter)
			if delay, ok := CalculateRetryDelay(strategy, attempt); ok {
				select {
				case <-time.After(delay):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		break
	}

	if state.preferredStatus > 0 {
		return nil, apperr.NewUpstream("antigravity",
			state.preferredStatus, state.preferredBody, state.preferredRetryAfter)
	}
	if lastStatus > 0 {
		return nil, apperr.NewUpstream("antigravity", lastStatus, lastBody, lastRetryAfter)
	}
	return nil, errors.New("all endpoints failed")
}

// sendRequestStreaming runs the streaming path, invoking emit for each decoded
// chunk as it arrives.
func sendRequestStreaming(
	ctx context.Context,
	request map[string]any,
	accessToken, accountID string,
	allowRotation bool,
	modelName, routeTag string,
	emit func(upstreamChunk) error,
) error {
	startTime := time.Now()
	state := &attemptState{
		accessToken:        accessToken,
		accountID:          accountID,
		projectRefreshDone: map[string]bool{},
	}

	rotationBudget := 0
	if allowRotation {
		rotationBudget = Accounts.Count() - 1
		if rotationBudget < 0 {
			rotationBudget = 0
		}
	}
	maxAttempts := maxNonQuota429Retries + 1 + rotationBudget

	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		retryAttempt := false

		for _, baseURL := range shuffledBaseURLs() {
			payload, err := json.Marshal(request)
			if err != nil {
				return err
			}

			resp, cancel, err := doUpstream(ctx, baseURL+streamEndpoint+"?alt=sse",
				payload, state.accessToken)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				logx.Debug("[SSE Streaming] %s: %v", baseURL, err)
				continue
			}

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				retryAfter := resp.Header.Get("Retry-After")
				status := resp.StatusCode
				resp.Body.Close()
				cancel()

				body := string(rawBody)
				logx.Warn("SSE error %d %s", status, truncate(body, 200))
				lastErr = apperr.NewUpstream("antigravity", status, body, retryAfter)

				action, failErr := state.classifyFailure(
					ctx, status, body, retryAfter, request, allowRotation)
				switch action {
				case "retry":
					retryAttempt = true
				case "next":
					continue
				case "fail":
					return failErr
				}
				break
			}

			if state.accountID != "" {
				Accounts.MarkSuccess(state.accountID)
			}

			emitted, streamErr := consumeStream(resp, cancel, emit)
			if streamErr != nil {
				// Once bytes have reached the client the response cannot be
				// restarted, so the error must propagate.
				if emitted {
					var upstream *apperr.UpstreamError
					if errors.As(streamErr, &upstream) {
						upstream.StreamingStarted = true
					}
					return streamErr
				}
				logx.Warn("[SSE Streaming] Error on %s: %v", baseURL, streamErr)
				lastErr = streamErr
				continue
			}

			logSuccess(startTime, modelName, state.accountID, routeTag)
			return nil
		}

		if !retryAttempt {
			break
		}
	}

	if state.preferredStatus > 0 {
		return apperr.NewUpstream("antigravity",
			state.preferredStatus, state.preferredBody, state.preferredRetryAfter)
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("all endpoints failed")
}

// consumeStream reads the SSE body, emitting each decoded chunk. It reports
// whether anything was emitted, which decides if a failure is recoverable.
func consumeStream(
	resp *http.Response, cancel context.CancelFunc, emit func(upstreamChunk) error,
) (emitted bool, err error) {
	defer resp.Body.Close()
	defer cancel()

	// Abort a stream that goes quiet. The timer is reset on every read.
	idleTimer := time.AfterFunc(idleTimeout, cancel)
	defer idleTimer.Stop()

	reader := &sseReader{source: resp.Body}
	for {
		event, readErr := reader.next()
		if event != "" {
			idleTimer.Reset(idleTimeout)
			data := strings.TrimSpace(extractSSEEventData(event))
			if data != "" && data != "[DONE]" {
				var chunk upstreamChunk
				if json.Unmarshal([]byte(data), &chunk) == nil {
					if emitErr := emit(chunk); emitErr != nil {
						return emitted, emitErr
					}
					emitted = true
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return emitted, readErr
		}
	}

	if !emitted {
		return false, errors.New("stream completed without yielding any data")
	}
	return true, nil
}

// sseReader splits a stream into SSE event blocks on blank lines.
type sseReader struct {
	source io.Reader
	buffer []byte
	// scanned is how much of buffer has already been searched for a separator.
	// Without it every 8KB read rescanned the whole buffer from the start, so a
	// single large event cost O(n^2): a 1MB event scanned ~64MB and ran at a
	// fraction of the throughput of a small one. Long model output and big tool
	// results arrive as exactly that kind of event.
	scanned int
	chunk   [8192]byte
}

// separatorOverlap is the longest separator (a CRLF pair repeated) minus one
// byte. Rescanning this much of the already-searched region keeps a separator
// that straddles a read boundary from being missed.
const separatorOverlap = 3

// next returns the next complete event block. At EOF it returns any trailing
// partial block alongside io.EOF.
func (r *sseReader) next() (string, error) {
	for {
		from := r.scanned - separatorOverlap
		if from < 0 {
			from = 0
		}
		if idx := indexBlankLine(r.buffer[from:]); idx.start >= 0 {
			event := string(r.buffer[:from+idx.start])
			r.buffer = r.buffer[from+idx.end:]
			r.scanned = 0
			return event, nil
		}
		r.scanned = len(r.buffer)

		n, err := r.source.Read(r.chunk[:])
		if n > 0 {
			r.buffer = append(r.buffer, r.chunk[:n]...)
			continue
		}
		if err != nil {
			// Hand back whatever trailing event data remains.
			tail := string(r.buffer)
			r.buffer = nil
			r.scanned = 0
			return tail, err
		}
	}
}

type blankLineIndex struct{ start, end int }

// indexBlankLine finds the first \n\n or \r\n\r\n separator.
func indexBlankLine(buf []byte) blankLineIndex {
	for i := 0; i+1 < len(buf); i++ {
		if buf[i] == '\n' && buf[i+1] == '\n' {
			return blankLineIndex{start: i, end: i + 2}
		}
		if i+3 < len(buf) && buf[i] == '\r' && buf[i+1] == '\n' &&
			buf[i+2] == '\r' && buf[i+3] == '\n' {
			return blankLineIndex{start: i, end: i + 4}
		}
	}
	return blankLineIndex{start: -1, end: -1}
}

func logSuccess(startTime time.Time, modelName, accountID, routeTag string) {
	elapsed := fmt.Sprintf("%.1f", time.Since(startTime).Seconds())
	account := "-"
	if accountID != "" {
		if email := Accounts.EmailFor(accountID); email != "" {
			account = email
		} else {
			account = accountID
		}
	}
	logx.Raw(logx.FormatSuccessLine(logx.SuccessLineParams{
		Elapsed:  elapsed,
		Model:    modelName,
		Provider: "antigravity",
		Account:  account,
		RouteTag: routeTag,
	}))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// resolveAccount picks the credential for a request, falling back to the
// process-level token when no managed accounts exist.
func resolveAccount(ctx context.Context, opts CallOptions) (
	accessToken, accountID, projectID, email string, err error,
) {
	if opts.AccountID != "" {
		account := Accounts.ByID(ctx, opts.AccountID)
		if account == nil {
			return "", "", "", "", apperr.NewUpstream("antigravity", 429,
				"Account unavailable: "+opts.AccountID, "")
		}
		return account.AccessToken, account.AccountID, account.ProjectID, account.Email, nil
	}

	if account := Accounts.NextAvailable(ctx, false); account != nil {
		return account.AccessToken, account.AccountID, account.ProjectID, account.Email, nil
	}

	// Every account switched off means exactly that: serve nothing.
	//
	// The fallback below uses the process-level token, which belongs to one of
	// these same Google accounts. Falling through to it would keep serving
	// traffic from an account the user had just paused -- the switch would look
	// right in the dashboard and do nothing. The fallback is for an install with
	// no managed accounts at all, not for one where they are all turned off.
	if Accounts.Count() > 0 && Accounts.EnabledCount() == 0 {
		return "", "", "", "", apperr.NewUpstream("antigravity", 503,
			"All accounts are paused. Enable one in the dashboard to resume.", "")
	}

	token, tokenErr := GetAccessToken(ctx)
	if tokenErr != nil {
		return "", "", "", "", tokenErr
	}
	auth := appstate.GetAuth()
	return token, "", auth.ProjectID, auth.UserEmail, nil
}

// CreateChatCompletion runs a non-streaming completion.
func CreateChatCompletion(
	ctx context.Context, req ChatRequest, opts CallOptions,
) (ChatResponse, error) {
	accessToken, accountID, projectID, email, err := resolveAccount(ctx, opts)
	if err != nil {
		return ChatResponse{}, err
	}

	logx.SetRequestInfo(ctx, req.Model, "antigravity", email, opts.RouteTag)

	if accountID != "" {
		release := Accounts.AcquireLock(ctx, accountID)
		defer release()
	}

	upstreamModel := UpstreamModelName(req.Model)
	request := buildUpstreamRequest(upstreamModel, req, req.Model, projectID)

	chunks, err := sendRequestBuffered(ctx, request, accessToken, accountID,
		opts.AllowRotation, req.Model, opts.RouteTag)
	if err != nil {
		return ChatResponse{}, err
	}

	result, err := parseAPIResponse(chunks)
	if err != nil {
		return ChatResponse{}, err
	}

	if result.Usage.InputTokens > 0 || result.Usage.OutputTokens > 0 {
		usage.Record(upstreamModel, result.Usage.InputTokens, result.Usage.OutputTokens)
	}
	return result, nil
}

// CreateChatCompletionStream runs a streaming completion, calling emit with each
// Anthropic SSE event as it is produced.
func CreateChatCompletionStream(
	ctx context.Context, req ChatRequest, opts CallOptions, emit func(string) error,
) error {
	accessToken, accountID, projectID, email, err := resolveAccount(ctx, opts)
	if err != nil {
		return err
	}

	logx.SetRequestInfo(ctx, req.Model, "antigravity", email, opts.RouteTag)

	if accountID != "" {
		release := Accounts.AcquireLock(ctx, accountID)
		defer release()
	}

	upstreamModel := UpstreamModelName(req.Model)
	request := buildUpstreamRequest(upstreamModel, req, req.Model, projectID)

	translator := newStreamTranslator(req.Model, emit)
	if err := translator.start(); err != nil {
		return err
	}

	streamErr := sendRequestStreaming(ctx, request, accessToken, accountID,
		opts.AllowRotation, req.Model, opts.RouteTag, translator.handleChunk)
	if streamErr != nil {
		return streamErr
	}

	if err := translator.finish(req.ToolChoice); err != nil {
		return err
	}

	if translator.inputTokens > 0 || translator.outputTokens > 0 {
		usage.Record(upstreamModel, translator.inputTokens, translator.outputTokens)
	}
	return nil
}
