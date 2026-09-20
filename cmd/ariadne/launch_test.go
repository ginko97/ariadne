package main

import "testing"

// A bare `ariadne` opens the browser only when Explorer started it. Anywhere
// else — a terminal, a pipe, a CI runner with no console at all — it prints
// usage, because starting a web server inside somebody's script is worse than
// a usage message.
func TestStartUIInsteadOnlyForExplorer(t *testing.T) {
	if startUIInstead(false) {
		t.Error("a launch that is not from Explorer must not start the UI")
	}
	if !startUIInstead(true) {
		t.Error("a double-click must start the UI")
	}
}

// The detection must not misfire where ariadne actually runs: a test binary,
// a shell, a CI runner. Each of those either shares a console with the process
// that started it or has no console, and neither is a double-click.
//
// This is the guard that matters. Explorer's case cannot be produced from
// inside `go test` — the test binary always has a parent on its console — so
// the half worth asserting is the half that would break everything else: a
// detection that says yes here would start a web server during `make check`,
// during CI, and inside any script that runs `ariadne` with no arguments.
func TestLaunchedFromExplorerIsFalseUnderTest(t *testing.T) {
	if launchedFromExplorer() {
		t.Error("launchedFromExplorer() is true under go test; a bare `ariadne` " +
			"would start a web server in scripts and in CI")
	}
}
