//go:build !windows

package loop

import "os"

// readShared is os.ReadFile where a reader cannot block a rename over the
// file it is reading. See readshared_windows.go for where it can.
func readShared(name string) ([]byte, error) { return os.ReadFile(name) }

// replaceFile is os.Rename, which replaces an open file on these systems.
func replaceFile(from, to string) error { return os.Rename(from, to) }
