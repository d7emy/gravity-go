package logx

import "testing"

// Regression: the green 200-success line is built with SGR escapes for the
// terminal. The dashboard renders log lines as plain text, so those escapes
// appeared literally as "[32m...[0m" around every success entry.
func TestStripANSI(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"green success line", "\x1b[32m[1:23:45 PM] 200 ok\x1b[0m", "[1:23:45 PM] 200 ok"},
		{"no escapes", "plain line", "plain line"},
		{"multiple codes", "\x1b[1m\x1b[31merror\x1b[0m", "error"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripANSI(tc.in); got != tc.want {
				t.Errorf("StripANSI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The formatted success line must still carry colour for the terminal.
func TestSuccessLineKeepsColourForTerminal(t *testing.T) {
	line := FormatSuccessLine(SuccessLineParams{
		Elapsed: "1.2", Model: "m", Provider: "antigravity", Account: "a@b.c",
	})
	if StripANSI(line) == line {
		t.Error("success line should carry ANSI colour on stdout")
	}
	if stripped := StripANSI(line); stripped == "" {
		t.Error("stripping should leave the readable text behind")
	}
}
