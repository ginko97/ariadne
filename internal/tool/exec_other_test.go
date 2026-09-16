//go:build !windows

package tool

import "syscall"

// processAlive reports whether pid names a running process: signal 0 checks
// existence without delivering anything.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
