// Package fsx reads and replaces files that another part of ariadne may be
// reading at the same moment: a conversation's checkpoint, and MEMORY.md.
package fsx

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

// Replacing a file while something reads it.
//
// A checkpoint or MEMORY.md is saved by renaming a temp file over it. On
// Windows that rename fails with "Access is denied" while any other handle has
// the file open, and the page reads both while turns run: it reloads the
// conversation list, which reads every checkpoint, as a turn starts. A model that answered within milliseconds found its
// checkpoint open, and the turn died at "checkpoint failed at step 1"
// (TestSaveSucceedsWhileTheCheckpointIsBeingRead, found in the v0.6.9
// browser check). Two halves, both needed: readers open with
// FILE_SHARE_DELETE, and the rename uses POSIX semantics, which let the new
// file take the name at once while open readers keep the old contents. Share
// delete alone is not enough: the old file keeps its name until the last
// reader closes it, and an ordinary rename still finds the name taken.

// ReadShared reads a file without standing in the way of the next Replace.
func ReadShared(name string) ([]byte, error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		// A PathError around the Errno, as os.ReadFile returns, so
		// errors.Is(err, fs.ErrNotExist) still recognises a missing file.
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	f := os.NewFile(uintptr(h), name)
	defer f.Close()
	return io.ReadAll(f)
}

var procSetFileInformationByHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")

const (
	accessDelete                  = 0x00010000 // DELETE
	fileRenameInfoEx              = 22         // FILE_INFO_BY_HANDLE_CLASS
	fileRenameFlagReplaceIfExists = 0x1
	fileRenameFlagPOSIXSemantics  = 0x2
)

// fileRenameInfo is FILE_RENAME_INFO with its Flags member; FileName runs on
// past the end of the struct. Field types and order match the C layout on
// both 386 and amd64/arm64.
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  syscall.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// Replace renames from over to, even while readers have it open.
// Filesystems or Windows versions without FileRenameInfoEx (FAT, before
// Windows 10 1607) get os.Rename, which is correct and only loses the race.
func Replace(from, to string) error {
	target, err := syscall.UTF16FromString(to)
	if err != nil {
		return os.Rename(from, to)
	}
	p, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return os.Rename(from, to)
	}
	h, err := syscall.CreateFile(p, accessDelete,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return os.Rename(from, to)
	}

	var info fileRenameInfo
	nameOff := unsafe.Offsetof(info.FileName)
	nameBytes := uintptr(len(target)-1) * 2 // without the terminating NUL
	buf := make([]byte, nameOff+uintptr(len(target))*2)
	ri := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	ri.Flags = fileRenameFlagReplaceIfExists | fileRenameFlagPOSIXSemantics
	ri.FileNameLength = uint32(nameBytes)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[nameOff])), len(target)), target)

	r, _, callErr := procSetFileInformationByHandle.Call(uintptr(h), fileRenameInfoEx,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	syscall.CloseHandle(h)
	if r != 0 {
		return nil
	}
	if err := os.Rename(from, to); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: callErr}
	}
	return nil
}
