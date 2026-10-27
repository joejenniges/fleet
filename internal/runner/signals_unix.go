//go:build !windows

package runner

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// SIGINT from a terminal already reaches the child (same process group), so it is
// caught (to keep fleet alive until the child exits) but not forwarded twice.
// SIGTERM/SIGHUP are sent only to fleet, so they are forwarded.
var handledSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

func forward(p *os.Process, s os.Signal) {
	if s == syscall.SIGINT {
		return
	}
	_ = p.Signal(s)
}

func exitCode(cmd *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}
