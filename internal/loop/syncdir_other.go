//go:build !windows

package loop

import (
	"errors"
	"os"
	"syscall"
)

// syncDirectory flushes a directory's own entries, so a rename into it
// survives a power cut.
//
// fsync on the file is not enough. rename(2) returning means the new name is
// visible to anything reading the filesystem now; it does not mean the
// directory entry has reached the disk. A machine that loses power in between
// can come back with the file's bytes safely written under a name nothing
// points at, and the old checkpoint — or no checkpoint — in its place. The
// file sync protects the contents, this protects the name.
//
// ENOTSUP and EINVAL are not failures: some filesystems do not implement fsync
// on a directory and say so, and a checkpoint that is merely as durable as
// that filesystem allows is not an error this process can fix or should abort
// a turn over.
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
