package main

import (
	"os"
	"testing"
)

func TestIsTerminalOnNULDevice(t *testing.T) {
	f, err := os.Open("NUL")
	if err != nil {
		t.Skip("cannot open NUL device:", err)
	}
	defer f.Close()

	if isTerminal(f) {
		t.Errorf("isTerminal(NUL) = true, want false")
	}
}
