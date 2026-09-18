//go:build !windows

package main

import (
	"os"
	"os/exec"
)

// echoOff stops the terminal echoing what is typed, and returns how to put it
// back. stty rather than termios ioctls: the ioctl numbers differ between
// Linux and macOS, and stty is on every system this runs on.
func echoOff(f *os.File) (restore func(), err error) {
	off := exec.Command("stty", "-echo")
	off.Stdin = f
	if err := off.Run(); err != nil {
		return nil, err
	}
	return func() {
		on := exec.Command("stty", "echo")
		on.Stdin = f
		_ = on.Run()
	}, nil
}
