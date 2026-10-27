package runner

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestTailKeepsEnd(t *testing.T) {
	tl := NewTail(16)
	for i := 0; i < 10; i++ {
		tl.Write([]byte("line-abcdef\n"))
	}
	s := tl.String()
	if !strings.Contains(s, "truncated") || !strings.HasSuffix(s, "line-abcdef\n") {
		t.Fatalf("tail: %q", s)
	}
	if tl.Lines() != 10 {
		t.Fatalf("lines = %d", tl.Lines())
	}
}

func TestRunPassthroughAndExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	var out bytes.Buffer
	r := Run(Options{Args: []string{"sh", "-c", "echo hello; echo oops >&2; exit 3"}, Capture: true, Stdout: &out, Stderr: &out})
	if r.ExitCode != 3 {
		t.Fatalf("exit = %d", r.ExitCode)
	}
	if !strings.Contains(out.String(), "hello") || !strings.Contains(r.Output, "oops") {
		t.Fatalf("passthrough %q / captured %q", out.String(), r.Output)
	}
}

func TestRunMissingBinary(t *testing.T) {
	r := Run(Options{Args: []string{"definitely-not-a-real-binary-xyz"}})
	if r.StartErr == nil || r.ExitCode != 127 {
		t.Fatalf("got %+v", r)
	}
}

func TestJoinArgs(t *testing.T) {
	if got := JoinArgs([]string{"git", "commit", "-m", "two words"}); got != `git commit -m "two words"` {
		t.Fatal(got)
	}
}
