package main

import (
	"os"
	"syscall"
)

// isTerminal reports whether f is attached to an interactive Windows console.
//
// os.ModeCharDevice is true for NUL as well as the console, which caused
// unattended runs with redirected stdin (< NUL) to be mistaken for an interactive terminal.
// syscall rather than golang.org/x/sys: the standard library has the call.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}
