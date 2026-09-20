package main

import (
	"syscall"
	"unsafe"
)

// getConsoleProcessList is not in syscall, unlike GetConsoleMode next door, so
// it is resolved by name. kernel32 is already loaded in every process.
var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

// launchedFromExplorer reports whether this process owns its console alone,
// which on Windows means somebody double-clicked the binary.
//
// A program started from a terminal shares that terminal's console with the
// shell that started it, so the console's process list has at least two
// entries. Explorer instead creates a console for the new process and nothing
// else is attached to it, so the list has exactly one: us. That console is
// destroyed the moment the process exits, which is why printing usage and
// returning looks, from the outside, like nothing happened at all.
//
// A process with no console (a GUI subsystem parent, a service) gets 0 back
// and is not Explorer either, so anything but 1 is false.
func launchedFromExplorer() bool {
	var pids [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}
