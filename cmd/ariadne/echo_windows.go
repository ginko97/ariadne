package main

import (
	"os"
	"syscall"
)

// setConsoleMode is not in package syscall, so it is loaded from kernel32 the
// way syscall loads its own procedures. Standard library only, like
// isTerminal.
var setConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

const enableEchoInput = 0x0004

// echoOff stops the console echoing what is typed, and returns how to put it
// back, so a key typed into `ariadne setup` does not stay on the screen.
func echoOff(f *os.File) (restore func(), err error) {
	h := syscall.Handle(f.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return nil, err
	}
	if r, _, e := setConsoleMode.Call(uintptr(h), uintptr(mode&^enableEchoInput)); r == 0 {
		return nil, e
	}
	return func() { setConsoleMode.Call(uintptr(h), uintptr(mode)) }, nil
}
