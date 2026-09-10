// Package logx renders gravity-go's console output and mirrors every line into
// the dashboard log buffer.
package logx

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"gravity-go/internal/logbuf"
)

const separator = "================================"

var providerLabels = map[string]string{
	"antigravity": "Antigravity",
}

var providerNames = map[string]string{
	"antigravity": "Antigravity",
}

var verbose bool
var verboseMu sync.RWMutex

// SetVerbose enables Debug output.
func SetVerbose(v bool) {
	verboseMu.Lock()
	defer verboseMu.Unlock()
	verbose = v
}

func isVerbose() bool {
	verboseMu.RLock()
	defer verboseMu.RUnlock()
	return verbose
}

// ansiPattern matches SGR colour escapes.
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

// StripANSI removes colour escapes from a log line.
func StripANSI(line string) string { return ansiPattern.ReplaceAllString(line, "") }

// emit writes to stdout and mirrors into the dashboard buffer.
//
// The terminal keeps its colour; the buffer does not. The dashboard renders log
// lines as text, so an un-stripped line showed up there as a literal
// "[32m...[0m" wrapped around every success entry.
func emit(level, line string) {
	fmt.Fprintln(os.Stdout, line)
	logbuf.Append(level, StripANSI(line))
}

// Info prints an informational line.
func Info(format string, args ...any) { emit("info", fmt.Sprintf(format, args...)) }

// Warn prints a warning line.
func Warn(format string, args ...any) { emit("warn", fmt.Sprintf(format, args...)) }

// Error prints an error line.
func Error(format string, args ...any) { emit("error", fmt.Sprintf(format, args...)) }

// Success prints a success line.
func Success(format string, args ...any) { emit("info", fmt.Sprintf(format, args...)) }

// Debug prints only when verbose mode is on.
func Debug(format string, args ...any) {
	if isVerbose() {
		emit("debug", fmt.Sprintf(format, args...))
	}
}

// Raw prints a preformatted line (already coloured, etc).
func Raw(line string) { emit("log", line) }

// FormatTime renders the clock prefix used across the logs.
func FormatTime() string {
	return time.Now().Format("3:04:05 PM")
}

// LogStartup prints the pre-listen banner.
func LogStartup() {
	fmt.Println()
	fmt.Println(separator)
	fmt.Println()
	fmt.Println("Starting…")
}

// LogStartupSuccess prints the post-listen banner.
func LogStartupSuccess(port int) {
	fmt.Printf("Succeed. PID: %d.\n", os.Getpid())
	fmt.Printf("listen on: http://localhost:%d/quota\n", port)
	fmt.Println()
	fmt.Println(separator)
	fmt.Println()
}

// SuccessLineParams describes one completed upstream call.
type SuccessLineParams struct {
	Elapsed  string
	Model    string
	Provider string
	Account  string
	RouteTag string
}

// FormatSuccessLine renders the green 200 line.
func FormatSuccessLine(p SuccessLineParams) string {
	model := p.Model
	if model == "" {
		model = "unknown"
	}
	provider := p.Provider
	if provider == "" {
		provider = "unknown"
	}
	label, ok := providerLabels[provider]
	if !ok {
		label = provider
	}
	account := p.Account
	if account == "" {
		account = "-"
	}
	line := fmt.Sprintf("[%s] 200 %s•%s•%s•%ss", FormatTime(), model, label, account, p.Elapsed)
	return "\x1b[32m" + line + "\x1b[0m"
}

// ProviderName maps a provider id to its display name.
func ProviderName(provider string) string {
	if name, ok := providerNames[provider]; ok {
		return name
	}
	return provider
}

// RequestContext carries per-request attribution for the access log. The
// TypeScript build used AsyncLocalStorage; Go threads it through context.Context.
type RequestContext struct {
	mu       sync.Mutex
	Model    string
	Provider string
	Account  string
	RouteTag string
}

// Set records attribution for the in-flight request.
func (rc *RequestContext) Set(model, provider, account, routeTag string) {
	if rc == nil {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if model != "" {
		rc.Model = model
	}
	if provider != "" {
		rc.Provider = provider
	}
	if account != "" {
		rc.Account = account
	}
	if routeTag != "" {
		rc.RouteTag = routeTag
	}
}

// Snapshot reads the attribution back.
func (rc *RequestContext) Snapshot() (model, provider, account, routeTag string) {
	if rc == nil {
		return "", "", "", ""
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.Model, rc.Provider, rc.Account, rc.RouteTag
}

type ctxKey struct{}

// WithRequestContext attaches a fresh RequestContext to ctx.
func WithRequestContext(ctx context.Context) (context.Context, *RequestContext) {
	rc := &RequestContext{}
	return context.WithValue(ctx, ctxKey{}, rc), rc
}

// FromContext retrieves the RequestContext, or nil.
func FromContext(ctx context.Context) *RequestContext {
	if ctx == nil {
		return nil
	}
	rc, _ := ctx.Value(ctxKey{}).(*RequestContext)
	return rc
}

// SetRequestInfo is the convenience form used from the chat path.
func SetRequestInfo(ctx context.Context, model, provider, account, routeTag string) {
	FromContext(ctx).Set(model, provider, account, routeTag)
}
