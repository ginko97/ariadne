package tool

import "syscall"

// processAlive reports whether pid names a running process.
//
// os.FindProcess cannot answer this on Windows: it succeeds for any pid it can
// open, including one that has exited but whose handle something still holds.
// The exit code is the reliable signal.
func processAlive(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	const stillActive = 259
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
