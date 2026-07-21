//go:build unix

package capture

import (
	"os"
	"os/exec"
	"syscall"
)

type processController struct{}

func prepareProcess(cmd *exec.Cmd) *processController {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processController{}
}

func (*processController) afterStart(*exec.Cmd) error {
	return nil
}

func (*processController) terminate(cmd *exec.Cmd) error {
	// The child is its process-group leader, so a negative PID addresses the
	// entire group, including descendants that inherited the group.
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err != nil {
		// The leader may have exited just before cancellation. Kill is a safe
		// fallback when no process group remains.
		return cmd.Process.Kill()
	}
	return nil
}

func (*processController) close() {}

func processSignal(state *os.ProcessState) (string, int, bool) {
	if state == nil {
		return "", 0, false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return "", 0, false
	}
	signal := status.Signal()
	return signal.String(), int(signal), true
}
