//go:build !windows

package tool

import (
	"os/exec"
	"syscall"
)

// killTreeOnCancel makes a cancelled context stop the process and everything
// it started: the child leads its own process group, and the group is killed.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
