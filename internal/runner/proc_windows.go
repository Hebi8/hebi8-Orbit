//go:build windows

package runner

import "os/exec"

func setProcessGroup(_ *exec.Cmd) {}

func processGroupID(_ *exec.Cmd) int { return 0 }

func killProcessGroupByID(cmd *exec.Cmd, _ int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func killProcessGroup(cmd *exec.Cmd) {
	killProcessGroupByID(cmd, 0)
}
