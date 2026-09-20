//go:build !windows

package main

// launchedFromExplorer is Windows-only behaviour.
//
// The problem it solves does not exist here: a file manager on macOS or Linux
// does not run a bare console binary in a disposable window, and the usual way
// to start one is a terminal that stays open to show what it printed. Claiming
// to detect a double-click on these platforms would mean guessing.
func launchedFromExplorer() bool { return false }
