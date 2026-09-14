// Command check runs the whole verification suite in one step:
//
//	go run ./cmd/check
//
// It is written in Go rather than a Makefile or shell script so it needs no
// tooling beyond the Go install that is already required to build the project,
// and behaves the same on Windows, macOS and Linux.
//
// The race step is the reason this exists. `go test -race` needs cgo, which
// needs a C compiler, which is not present on every machine. Rather than failing
// with a confusing "C compiler not found", this detects the situation and tells
// you exactly what to install — while still running everything else.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ANSI colours, disabled when the output is redirected or NO_COLOR is set.
var (
	bold, red, green, yellow, dim, reset string
)

func initColour() {
	if os.Getenv("NO_COLOR") != "" {
		return
	}
	if fi, err := os.Stdout.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		return // not a terminal
	}
	bold, red, green, yellow, dim, reset =
		"\x1b[1m", "\x1b[31m", "\x1b[32m", "\x1b[33m", "\x1b[2m", "\x1b[0m"
}

type result struct {
	name     string
	status   string // "pass", "fail", "skip"
	detail   string
	duration time.Duration
}

func main() {
	initColour()

	var (
		skipRace = flag.Bool("no-race", false, "skip the race-detector step")
		onlyRace = flag.Bool("race-only", false, "run only the race-detector step")
		short    = flag.Bool("short", false, "pass -short to go test (skips slower cases)")
	)
	flag.Parse()

	fmt.Printf("\n%sgravity-go — full check%s\n", bold, reset)
	fmt.Printf("%s%s/%s · %s%s\n\n", dim, runtime.GOOS, runtime.GOARCH, goVersion(), reset)

	var results []result
	started := time.Now()

	if !*onlyRace {
		results = append(results,
			step("format", "gofmt", checkFormat),
			step("vet", "go vet ./...", func() (string, error) {
				return run("go", "vet", "./...")
			}),
			step("build", "go build ./...", func() (string, error) {
				return run("go", "build", "./...")
			}),
			step("tests", testLabel(*short), func() (string, error) {
				args := []string{"test", "./...", "-count=1"}
				if *short {
					args = append(args, "-short")
				}
				return run("go", args...)
			}),
		)
	}

	if !*skipRace {
		results = append(results, raceStep(*short))
	}

	report(results, time.Since(started))
}

func testLabel(short bool) string {
	if short {
		return "go test ./... -count=1 -short"
	}
	return "go test ./... -count=1"
}

func goVersion() string {
	out, err := exec.Command("go", "version").Output()
	if err != nil {
		return "go (version unknown)"
	}
	return strings.TrimSpace(string(out))
}

// step runs one named action and prints its outcome as it goes.
func step(name, label string, fn func() (string, error)) result {
	fmt.Printf("%s→%s %-8s %s%s%s\n", dim, reset, name, dim, label, reset)
	start := time.Now()

	output, err := fn()
	took := time.Since(start)

	if err != nil {
		fmt.Printf("  %s✗ failed%s %s(%s)%s\n", red, reset, dim, took.Round(time.Millisecond), reset)
		if trimmed := strings.TrimSpace(output); trimmed != "" {
			for _, line := range strings.Split(trimmed, "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
		fmt.Println()
		return result{name: name, status: "fail", detail: firstLine(output), duration: took}
	}

	fmt.Printf("  %s✓ ok%s %s(%s)%s\n\n", green, reset, dim, took.Round(time.Millisecond), reset)
	return result{name: name, status: "pass", duration: took}
}

// checkFormat reports any file gofmt would change. `gofmt -l` exits 0 even when
// it lists files, so the file list is the signal, not the exit code.
func checkFormat() (string, error) {
	out, err := run("gofmt", "-l", ".")
	if err != nil {
		return out, err
	}
	if trimmed := strings.TrimSpace(out); trimmed != "" {
		return "these files need gofmt:\n" + trimmed, errors.New("unformatted files")
	}
	return "", nil
}

// raceStep runs the detector, or explains precisely why it cannot.
func raceStep(short bool) result {
	label := "go test ./... -race"
	fmt.Printf("%s→%s %-8s %s%s%s\n", dim, reset, "race", dim, label, reset)

	if cc, ok := findCCompiler(); !ok {
		fmt.Printf("  %s⊘ skipped%s — no C compiler found\n", yellow, reset)
		fmt.Printf("    %sThe race detector links a prebuilt C runtime, so it needs cgo,%s\n", dim, reset)
		fmt.Printf("    %swhich needs a C compiler. Everything else above still ran.%s\n", dim, reset)
		fmt.Printf("    %sInstall one with:%s\n", dim, reset)
		fmt.Printf("      %s\n", installHint())
		fmt.Printf("    %sthen open a new terminal and re-run this command.%s\n\n", dim, reset)
		return result{name: "race", status: "skip", detail: "no C compiler"}
	} else {
		fmt.Printf("  %susing %s%s\n", dim, cc, reset)
	}

	start := time.Now()
	args := []string{"test", "./...", "-race", "-count=1"}
	if short {
		args = append(args, "-short")
	}

	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	raw, err := cmd.CombinedOutput()
	output := string(raw)
	took := time.Since(start)

	if err != nil {
		fmt.Printf("  %s✗ failed%s %s(%s)%s\n", red, reset, dim, took.Round(time.Second), reset)
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
		return result{name: "race", status: "fail", detail: firstLine(output), duration: took}
	}

	fmt.Printf("  %s✓ no races detected%s %s(%s)%s\n\n", green, reset, dim, took.Round(time.Second), reset)
	return result{name: "race", status: "pass", duration: took}
}

// findCCompiler looks for a usable C compiler the way cgo would.
func findCCompiler() (string, bool) {
	// An explicit CC wins, as it does for cgo itself.
	if cc := strings.TrimSpace(os.Getenv("CC")); cc != "" {
		if path, err := exec.LookPath(cc); err == nil {
			return path, true
		}
	}
	for _, name := range []string{"gcc", "clang", "cc", "x86_64-w64-mingw32-gcc"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, true
		}
	}
	return "", false
}

func installHint() string {
	switch runtime.GOOS {
	case "windows":
		return "winget install -e --id BrechtSanders.WinLibs.POSIX.UCRT.Base"
	case "darwin":
		return "xcode-select --install"
	default:
		return "sudo apt install build-essential   # or your distro's equivalent"
	}
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// report prints the summary and sets the exit code. A skipped race step is not a
// failure: it is a machine capability gap, not a defect in the code.
func report(results []result, total time.Duration) {
	fmt.Printf("%s────────────────────────────%s\n", dim, reset)

	failed, skipped := 0, 0
	for _, r := range results {
		var mark, colour string
		switch r.status {
		case "pass":
			mark, colour = "✓", green
		case "fail":
			mark, colour = "✗", red
			failed++
		default:
			mark, colour = "⊘", yellow
			skipped++
		}
		detail := ""
		if r.detail != "" {
			detail = "  " + dim + r.detail + reset
		}
		fmt.Printf("  %s%s%s %-6s%s\n", colour, mark, reset, r.name, detail)
	}

	fmt.Printf("%s────────────────────────────%s\n", dim, reset)

	switch {
	case failed > 0:
		fmt.Printf("%s%s%d step(s) failed%s in %s\n\n",
			bold, red, failed, reset, total.Round(time.Millisecond))
		os.Exit(1)
	case skipped > 0:
		fmt.Printf("%s%sall checks passed%s, %d skipped, in %s\n\n",
			bold, green, reset, skipped, total.Round(time.Millisecond))
	default:
		fmt.Printf("%s%sall checks passed%s in %s\n\n",
			bold, green, reset, total.Round(time.Millisecond))
	}
}
