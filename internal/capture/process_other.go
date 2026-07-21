//go:build !unix && !windows

package capture

import (
	"os"
	"os/exec"
)

type processController struct{}

func prepareProcess(*exec.Cmd) *processController {
	return &processController{}
}

func (*processController) afterStart(*exec.Cmd) error {
	return nil
}

func (*processController) terminate(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}

func (*processController) close() {}

func processSignal(*os.ProcessState) (string, int, bool) {
	return "", 0, false
}
