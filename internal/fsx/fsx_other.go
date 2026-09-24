//go:build !windows

package fsx

import "os"

// ReadShared is os.ReadFile where a reader cannot block a rename over the
// file it is reading. See fsx_windows.go for where it can.
func ReadShared(name string) ([]byte, error) { return os.ReadFile(name) }

// Replace is os.Rename, which replaces an open file on these systems.
func Replace(from, to string) error { return os.Rename(from, to) }
