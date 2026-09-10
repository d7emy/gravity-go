package main

import (
	"os/exec"
	"strings"
	"testing"
)

// `go run .` passes no arguments. That has to start the server rather than
// print usage and exit non-zero — it is the most obvious way to run the project
// and it used to fail.
func TestNoArgumentsStartsTheServer(t *testing.T) {
	// -h is handled before any listening happens, so this exercises the
	// dispatcher without binding a port.
	out, err := exec.Command("go", "run", ".", "-h").CombinedOutput()
	if err != nil {
		t.Fatalf("go run . -h failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Start the proxy server") {
		t.Errorf("help text does not describe starting the server:\n%s", out)
	}
	// The bare form must be documented, or nobody knows it exists.
	if !strings.Contains(string(out), `gravity-go                        Start the proxy server`) {
		t.Errorf("usage does not document the no-argument form:\n%s", out)
	}
}
