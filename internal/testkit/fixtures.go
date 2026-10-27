package testkit

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"github.com/joejenniges/fleet/internal/procs"
)

// FixtureFlag marks fixture processes so tests (and teardown) can find them, and so
// policy rules can target them with args_contains.
const FixtureFlag = "--fleet-test-fixture="

// ServePort listens on 127.0.0.1:port until SIGTERM/SIGINT. With detach, it re-execs
// itself in the background (new session/process group), prints the child pid and
// returns immediately — like a dev server started with `&`.
func ServePort(port int, label string, detach bool) error {
	if detach {
		args := []string{"test", "serve-port", fmt.Sprintf("--port=%d", port), FixtureFlag + label}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(self, args...)
		cmd.Env = os.Environ()
		detachAttrs(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		fmt.Printf("started fixture %q in background, pid %d\n", label, cmd.Process.Pid)
		return cmd.Process.Release()
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	fmt.Printf("fixture %q listening on %s, pid %d\n", label, ln.Addr(), os.Getpid())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("fleet test fixture\n"))
			c.Close()
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	ln.Close()
	return nil
}

// FindFixtures returns running fixture processes whose label starts with prefix.
func FindFixtures(prefix string) []*procs.Info {
	ps, err := process.Processes()
	if err != nil {
		return nil
	}
	var out []*procs.Info
	self := int32(os.Getpid())
	for _, p := range ps {
		if p.Pid == self {
			continue
		}
		// Match on argv, not the flattened command line: a shell that merely *launched*
		// a fixture also has the marker in its command line and must never be matched.
		if args, err := p.CmdlineSlice(); err != nil || !isFixtureArgv(args, prefix) {
			continue
		}
		in, err := procs.Get(p.Pid)
		if err == nil {
			out = append(out, in)
		}
	}
	return out
}

// KillFixtures kills fixture processes whose label starts with prefix ("" = all).
func KillFixtures(prefix string) int {
	n := 0
	for _, f := range FindFixtures(prefix) {
		if p, err := process.NewProcess(f.PID); err == nil && p.Kill() == nil {
			n++
		}
	}
	return n
}

func sleepMS(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

// WaitFor polls cond until it is true or timeout passes.
func WaitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return cond()
}

// isFixtureArgv reports whether argv is `<fleet> test serve-port … --fleet-test-fixture=<prefix>…`.
func isFixtureArgv(args []string, prefix string) bool {
	if len(args) < 3 || args[1] != "test" || args[2] != "serve-port" {
		return false
	}
	for _, a := range args[3:] {
		if strings.HasPrefix(a, FixtureFlag+prefix) {
			return true
		}
	}
	return false
}
