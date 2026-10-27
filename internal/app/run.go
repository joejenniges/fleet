package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/joejenniges/fleet/internal/cmdtext"
	"github.com/joejenniges/fleet/internal/policy"
	"github.com/joejenniges/fleet/internal/procs"
	"github.com/joejenniges/fleet/internal/runner"
	"github.com/joejenniges/fleet/internal/store"
)

type RunOpts struct {
	Args     []string
	Critical bool
	Reason   string
	Purpose  string
	NoRecord bool
}

// RunCommand executes a command transparently, records it, and returns its exit code.
// If the database is unavailable the command still runs (fail open).
func (a *App) RunCommand(o RunOpts) int {
	cmdStr := runner.JoinArgs(o.Args)
	argv0 := cmdtext.Argv0(cmdStr)
	ag := a.Agent()

	// A command the policy already protects is critical for its lifetime.
	critical, reason := o.Critical, o.Reason
	if !critical {
		dec := a.Policy().Evaluate(policy.Input{Argv0: argv0, Cmdline: cmdStr}).Final
		if dec.Action == policy.Never && !dec.Unmatched {
			critical, reason = true, "policy:"+strings.Join(dec.DecidedBy, ",")
		}
	}
	if critical && reason == "" {
		reason = "fleet run --critical"
	}

	record := !o.NoRecord
	var runID int64
	if record {
		r := &store.Run{Command: a.Red.Command(cmdStr), NormCmd: cmdtext.Normalize(cmdStr, a.Red), Argv0: argv0,
			Cwd: a.Cwd, RepoRoot: a.Repo, Critical: critical, Source: "run", OS: runtime.GOOS}
		if ag != nil {
			r.AgentID = ag.ID
		}
		if id, err := a.St.StartRun(r); err == nil {
			runID = id
		}
	}

	var pid int32
	var ctime int64
	res := runner.Run(runner.Options{
		Args:     o.Args,
		Capture:  record && !runner.StdoutIsTerminal(),
		MaxBytes: a.Cfg.Runs.MaxOutputBytes,
		OnStart: func(p int) {
			pid = int32(p)
			if info, err := procs.Get(pid); err == nil {
				ctime = info.Ctime
			}
			proc := &store.Process{PID: pid, PIDCtime: ctime, Name: argv0, Cmdline: a.Red.Command(cmdStr),
				Cwd: a.Cwd, Purpose: o.Purpose, Critical: critical, CriticalReason: reason, Source: "run"}
			if ag != nil {
				proc.AgentID = ag.ID
			}
			_ = a.St.UpsertProcess(proc)
		},
	})
	if pid != 0 {
		_ = a.St.EndProcess(pid, ctime)
	}
	if res.StartErr != nil {
		fmt.Fprintf(os.Stderr, "fleet: %v\n", res.StartErr)
	}
	if runID != 0 {
		out := a.Red.Text(res.Output)
		if res.StartErr != nil {
			out = res.StartErr.Error()
		}
		_ = a.St.FinishRun(runID, res.ExitCode, res.Duration, out, res.Lines, a.ToolVersion(argv0))
		if res.ExitCode != 0 {
			fmt.Fprintf(os.Stderr, "[fleet] run #%d failed (exit %d). If the command itself is wrong, record it: "+
				"fleet docs add known-bad %q --from-run %d --note \"why\"\n", runID, res.ExitCode, cmdtext.Normalize(cmdStr, a.Red), runID)
		}
	}
	return res.ExitCode
}

// Tools whose version is worth recording. Only these are ever invoked: running
// `<arbitrary> --version` could execute a script with side effects.
var versionArgs = map[string][]string{
	"dotnet": {"--version"}, "node": {"--version"}, "npm": {"--version"}, "pnpm": {"--version"},
	"yarn": {"--version"}, "bun": {"--version"}, "deno": {"--version"}, "go": {"version"},
	"python": {"--version"}, "python3": {"--version"}, "pip": {"--version"}, "uv": {"--version"},
	"cargo": {"--version"}, "rustc": {"--version"}, "java": {"-version"}, "mvn": {"-v"},
	"gradle": {"--version"}, "ruby": {"--version"}, "php": {"--version"}, "git": {"--version"},
	"docker": {"--version"}, "kubectl": {"version", "--client"}, "terraform": {"version"},
	"psql": {"--version"},
}

// ToolVersion returns a cached (per day) version string for allow-listed tools.
func (a *App) ToolVersion(argv0 string) string {
	args, ok := versionArgs[argv0]
	if !ok {
		return ""
	}
	key := "toolver:" + argv0 + ":" + a.St.Now().Format("2006-01-02")
	if v, ok := a.St.MetaGet(key); ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv0, args...).CombinedOutput()
	v := ""
	if err == nil || len(out) > 0 {
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				v = line
				break
			}
		}
	}
	if len(v) > 80 {
		v = v[:80]
	}
	_ = a.St.MetaSet(key, v)
	return v
}
