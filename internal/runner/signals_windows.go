//go:build windows

package runner

import (
	"errors"
	"os"
	"os/exec"
)

// On Windows, Ctrl-C/Ctrl-Break are delivered to every process attached to the
// console, including the child. fleet only needs to stay alive until it exits.
var handledSignals = []os.Signal{os.Interrupt}

func forward(p *os.Process, s os.Signal) {}

func exitCode(cmd *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}
