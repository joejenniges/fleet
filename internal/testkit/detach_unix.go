//go:build !windows

package testkit

import (
	"os/exec"
	"syscall"
)

func detachAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
