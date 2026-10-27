// Package runner executes a child process transparently: same stdin, stdout/stderr
// passthrough, same exit code, signals forwarded, output tee'd into a bounded buffer.
package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

type Options struct {
	Args     []string
	Capture  bool          // tee output into the result
	MaxBytes int           // capture cap; the tail is kept
	OnStart  func(pid int) // called right after the child starts
	Stdout   io.Writer     // defaults to os.Stdout
	Stderr   io.Writer     // defaults to os.Stderr
	Env      []string      // nil = inherit
}

type Result struct {
	ExitCode int
	Duration time.Duration
	Output   string // captured tail (combined stdout+stderr)
	Lines    int    // total lines seen, including ones dropped from the tail
	StartErr error  // command could not be started
}

// StdoutIsTerminal reports whether fleet's own stdout is a TTY. When it is, capture
// is skipped so the child keeps its TTY (colors, progress bars, prompts).
func StdoutIsTerminal() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

func Run(o Options) Result {
	if len(o.Args) == 0 {
		return Result{ExitCode: 2, StartErr: errors.New("no command given")}
	}
	stdout, stderr := o.Stdout, o.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd := exec.Command(o.Args[0], o.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Env = o.Env
	var tail *Tail
	if o.Capture {
		tail = NewTail(o.MaxBytes)
		cmd.Stdout = io.MultiWriter(stdout, tail)
		cmd.Stderr = io.MultiWriter(stderr, tail)
	} else {
		cmd.Stdout, cmd.Stderr = stdout, stderr
	}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, handledSignals...)
	defer signal.Stop(sigs)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{ExitCode: 127, StartErr: err, Duration: time.Since(start)}
	}
	if o.OnStart != nil {
		o.OnStart(cmd.Process.Pid)
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				forward(cmd.Process, s)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	res := Result{Duration: time.Since(start), ExitCode: exitCode(cmd, err)}
	if tail != nil {
		res.Output, res.Lines = tail.String(), tail.Lines()
	}
	return res
}

// JoinArgs renders argv back into a single command line, quoting where needed.
func JoinArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'") {
			out[i] = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

// Tail keeps roughly the last max bytes written to it and counts lines.
type Tail struct {
	mu    sync.Mutex
	max   int
	buf   []byte
	lines int
	cut   bool
}

func NewTail(max int) *Tail {
	if max <= 0 {
		max = 256 * 1024
	}
	return &Tail{max: max}
}

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			t.lines++
		}
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
		t.cut = true
	}
	return len(p), nil
}

func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buf
	cut := t.cut
	if len(b) > t.max {
		b, cut = b[len(b)-t.max:], true
	}
	if cut {
		// Start at a line boundary so the first stored line is whole.
		if i := indexByte(b, '\n'); i >= 0 && i < len(b)-1 {
			b = b[i+1:]
		}
		return fmt.Sprintf("[… earlier output truncated …]\n%s", b)
	}
	return string(b)
}

func (t *Tail) Lines() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.lines
	if len(t.buf) > 0 && t.buf[len(t.buf)-1] != '\n' {
		n++
	}
	return n
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
