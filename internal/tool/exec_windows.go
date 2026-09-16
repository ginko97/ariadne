package tool

import (
	"os/exec"
	"strconv"
)

// killTreeOnCancel makes a cancelled context stop the process and everything
// it started.
//
// exec.CommandContext kills only the process it started. `go test` builds and
// runs a test binary as a child; killing go leaves that binary running, holding
// the output pipe and doing whatever it was doing. taskkill /T walks the tree.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
