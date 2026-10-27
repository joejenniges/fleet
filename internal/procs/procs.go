// Package procs wraps gopsutil for cross-platform process trees, identity and ports.
// A process is always identified by (pid, create time) because PIDs get reused.
package procs

import (
	"context"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

type Info struct {
	PID     int32  `json:"pid"`
	PPID    int32  `json:"ppid"`
	Ctime   int64  `json:"ctime"` // ms since epoch
	Name    string `json:"name"`
	Cmdline string `json:"cmdline"`
}

func get(pid int32) (*Info, error) {
	p, err := process.NewProcess(pid)
	if err != nil {
		return nil, err
	}
	return fill(p), nil
}

func fill(p *process.Process) *Info {
	in := &Info{PID: p.Pid}
	in.PPID, _ = p.Ppid()
	in.Ctime, _ = p.CreateTime()
	in.Name, _ = p.Name()
	in.Cmdline, _ = p.Cmdline()
	if in.Cmdline == "" {
		in.Cmdline = in.Name
	}
	return in
}

// Get returns info for pid.
func Get(pid int32) (*Info, error) { return get(pid) }

// Alive reports whether (pid, ctime) still refers to the same running process.
func Alive(pid int32, ctime int64) bool {
	if pid <= 0 {
		return false
	}
	p, err := process.NewProcess(pid)
	if err != nil {
		return false
	}
	ct, err := p.CreateTime()
	if err != nil {
		return false
	}
	if ctime != 0 && !sameCtime(ct, ctime) {
		return false // PID reused
	}
	if st, err := p.Status(); err == nil && len(st) > 0 && st[0] == process.Zombie {
		return false
	}
	return true
}

// Create times can differ by rounding between reads on some platforms.
func sameCtime(a, b int64) bool {
	d := a - b
	return d > -1000 && d < 1000
}

// Ancestors returns the chain from pid's parent upward (nearest first).
func Ancestors(pid int32) []*Info {
	var out []*Info
	seen := map[int32]bool{pid: true}
	cur, err := get(pid)
	for err == nil && cur.PPID > 0 && !seen[cur.PPID] && len(out) < 64 {
		seen[cur.PPID] = true
		var parent *Info
		parent, err = get(cur.PPID)
		if err != nil {
			break
		}
		// A parent cannot be younger than its child; if it is, the PPID was reused.
		if parent.Ctime > cur.Ctime+1000 {
			break
		}
		out = append(out, parent)
		cur = parent
	}
	return out
}

var shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "tcsh": true,
	"csh": true, "nu": true, "cmd": true, "powershell": true, "pwsh": true, "conhost": true,
	"env": true, "timeout": true, "nohup": true, "fleet": true, "sudo": true, "doas": true,
	"winpty": true, "wsl": true, "script": true,
}

// IsShellLike reports whether a process name is a shell/wrapper rather than an agent.
func IsShellLike(name string) bool { return shells[normName(name)] }

// AgentProcess walks up from this process and returns the nearest ancestor that is
// not a shell or wrapper. When called from a hook, that is the agent itself.
func AgentProcess() (*Info, bool) {
	for _, a := range Ancestors(int32(os.Getpid())) {
		if !IsShellLike(a.Name) {
			return a, true
		}
	}
	return nil, false
}

type tableEntry struct {
	PPID int32
	Name string
}

// Snapshot is a one-shot view of the process table, used to walk trees cheaply.
type Snapshot struct {
	ByPID    map[int32]*Info
	Children map[int32][]int32
}

func TakeSnapshot() (*Snapshot, error) {
	ps, err := process.Processes()
	if err != nil {
		return nil, err
	}
	table, fast := processTable()
	s := &Snapshot{ByPID: map[int32]*Info{}, Children: map[int32][]int32{}}
	for _, p := range ps {
		in := &Info{PID: p.Pid}
		if e, ok := table[p.Pid]; fast && ok {
			in.PPID, in.Name = e.PPID, e.Name
		} else {
			in.PPID, _ = p.Ppid()
			in.Name, _ = p.Name()
		}
		in.Ctime, _ = p.CreateTime()
		s.ByPID[p.Pid] = in
		s.Children[in.PPID] = append(s.Children[in.PPID], p.Pid)
	}
	return s, nil
}

// Descendants returns every descendant of pid (breadth-first), filling Cmdline lazily.
func (s *Snapshot) Descendants(pid int32) []*Info {
	var out []*Info
	queue := append([]int32(nil), s.Children[pid]...)
	seen := map[int32]bool{pid: true}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if seen[c] {
			continue
		}
		seen[c] = true
		in := s.ByPID[c]
		if in == nil {
			continue
		}
		if in.Cmdline == "" {
			if p, err := process.NewProcess(c); err == nil {
				in.Cmdline, _ = p.Cmdline()
			}
			if in.Cmdline == "" {
				in.Cmdline = in.Name
			}
		}
		out = append(out, in)
		queue = append(queue, s.Children[c]...)
	}
	return out
}

// AncestorsWithin returns pid's ancestors up to (excluding) stopAt, nearest first.
func (s *Snapshot) AncestorsWithin(pid, stopAt int32) []*Info {
	var out []*Info
	cur := s.ByPID[pid]
	for cur != nil && cur.PPID > 0 && cur.PPID != stopAt && len(out) < 64 {
		parent := s.ByPID[cur.PPID]
		if parent == nil {
			break
		}
		if parent.Cmdline == "" {
			if p, err := process.NewProcess(parent.PID); err == nil {
				parent.Cmdline, _ = p.Cmdline()
			}
		}
		out = append(out, parent)
		cur = parent
	}
	return out
}

// ListeningPorts maps pid -> listening TCP ports for all processes of this user.
// On macOS this shells out to lsof and can take a few hundred milliseconds.
func ListeningPorts(ctx context.Context) (map[int32][]uint32, error) {
	conns, err := gnet.ConnectionsWithContext(ctx, "tcp")
	if err != nil {
		return nil, err
	}
	out := map[int32][]uint32{}
	seen := map[[2]uint32]bool{}
	for _, c := range conns {
		if c.Status != "LISTEN" || c.Pid == 0 {
			continue
		}
		k := [2]uint32{uint32(c.Pid), c.Laddr.Port}
		if seen[k] {
			continue
		}
		seen[k] = true
		out[c.Pid] = append(out[c.Pid], c.Laddr.Port)
	}
	for pid := range out {
		sort.Slice(out[pid], func(i, j int) bool { return out[pid][i] < out[pid][j] })
	}
	return out, nil
}

// Terminate stops (pid, ctime) and its descendants, children first.
// Unix: SIGTERM, wait up to grace, then SIGKILL. Windows: no graceful signal exists
// for arbitrary processes, so this is a hard kill.
func Terminate(pid int32, ctime int64, grace time.Duration) error {
	if !Alive(pid, ctime) {
		return nil
	}
	snap, err := TakeSnapshot()
	if err != nil {
		return err
	}
	tree := snap.Descendants(pid)
	targets := make([]*Info, 0, len(tree)+1)
	for i := len(tree) - 1; i >= 0; i-- { // deepest first
		targets = append(targets, tree[i])
	}
	targets = append(targets, &Info{PID: pid, Ctime: ctime})

	for _, t := range targets {
		p, err := process.NewProcess(t.PID)
		if err != nil {
			continue
		}
		if runtime.GOOS == "windows" {
			_ = p.Kill()
		} else {
			_ = p.Terminate()
		}
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !Alive(pid, ctime) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, t := range targets {
		if Alive(t.PID, t.Ctime) {
			if p, err := process.NewProcess(t.PID); err == nil {
				_ = p.Kill()
			}
		}
	}
	return nil
}

func normName(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(s), ".exe")
}
