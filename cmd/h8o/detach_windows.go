//go:build windows

package main

import "os/exec"

func setDetachedProcAttr(_ *exec.Cmd) {}
