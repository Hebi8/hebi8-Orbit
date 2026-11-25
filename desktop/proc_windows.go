//go:build windows

package main

import (
	"os/exec"
	"time"
)

func setChildProcGroup(_ *exec.Cmd) {}

func stopChildProcess(cmd *exec.Cmd, done <-chan struct{}) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}
