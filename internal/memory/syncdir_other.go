//go:build !windows

package memory

import (
	"errors"
	"os"
	"syscall"
)

// syncDirectory flushes a directory's own entries so a renamed note file
// survives a power cut.
func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()

	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EINVAL) {
			return nil
		}
		return err
	}
	return nil
}
