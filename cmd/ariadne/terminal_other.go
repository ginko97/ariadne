//go:build !windows

package main

import "os"

// isTerminal reports whether f is a console rather than a pipe or a file.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
