package antigravity

import (
	"strings"
	"testing"
	"time"
)

// Regression: GRAVITY_NO_OPEN is documented as "do not auto-open the dashboard".
// Checking it inside the browser launcher meant it also silently suppressed the
// OAuth sign-in window, so "add account" appeared to do nothing. The two flags
// must stay independent, and the launcher itself must hold no policy.
//
// This asserts on the command that would be run rather than running it. An
// earlier version called OpenBrowser directly, which opened a real browser tab
// on every `go test ./...` — exactly the kind of side effect a unit test must
// not have.
func TestBrowserCommandIgnoresDashboardFlag(t *testing.T) {
	t.Setenv("GRAVITY_NO_OPEN", "1")
	t.Setenv("ANTI_API_NO_OPEN", "1")

	cmd := browserCommand("https://example.com/signin")
	if cmd == nil {
		t.Fatal("browserCommand() = nil; the dashboard flag must not suppress sign-in")
	}
	if cmd.Path == "" || len(cmd.Args) == 0 {
		t.Fatalf("browserCommand() built an unusable command: %+v", cmd)
	}

	// The target must survive into the arguments on every platform.
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "https://example.com/signin") {
		t.Errorf("command args = %v, want them to carry the URL", cmd.Args)
	}
}

// The launcher must not branch on the environment at all: the same command comes
// out whether or not the dashboard flag is set.
func TestBrowserCommandIsFlagIndependent(t *testing.T) {
	t.Setenv("GRAVITY_NO_OPEN", "")
	withoutFlag := strings.Join(browserCommand("https://example.com/x").Args, " ")

	t.Setenv("GRAVITY_NO_OPEN", "1")
	withFlag := strings.Join(browserCommand("https://example.com/x").Args, " ")

	if withoutFlag != withFlag {
		t.Errorf("command differs with the flag set: without=%q with=%q", withoutFlag, withFlag)
	}
}

func TestPendingAuthURLLifecycle(t *testing.T) {
	clearPendingAuth()

	if _, _, active := PendingAuthURL(); active {
		t.Fatal("no attempt should be active before one starts")
	}

	setPendingAuth("https://accounts.google.com/o/oauth2/v2/auth?x=1", true)

	url, opened, active := PendingAuthURL()
	if !active {
		t.Fatal("attempt should be active after it starts")
	}
	if url != "https://accounts.google.com/o/oauth2/v2/auth?x=1" {
		t.Errorf("url = %q", url)
	}
	if !opened {
		t.Error("browserOpened should reflect a successful launch")
	}

	clearPendingAuth()
	if _, _, active := PendingAuthURL(); active {
		t.Error("attempt should not be active once cleared")
	}
}

// A URL left behind by an abandoned attempt must not be offered forever: the
// flow itself gives up after five minutes.
func TestPendingAuthURLExpires(t *testing.T) {
	clearPendingAuth()
	setPendingAuth("https://example.com/auth", false)

	pendingAuth.mu.Lock()
	pendingAuth.startedAt = time.Now().Add(-6 * time.Minute)
	pendingAuth.mu.Unlock()

	if _, _, active := PendingAuthURL(); active {
		t.Error("a stale sign-in URL should not be advertised")
	}
	clearPendingAuth()
}

// The launch result must be reported honestly, so the dashboard can say
// "couldn't open a browser" rather than "a window should have opened".
func TestPendingAuthRecordsFailedLaunch(t *testing.T) {
	clearPendingAuth()
	setPendingAuth("https://example.com/auth", false)

	_, opened, active := PendingAuthURL()
	if !active {
		t.Fatal("attempt should be active")
	}
	if opened {
		t.Error("browserOpened should be false when the launch failed")
	}
	clearPendingAuth()
}
