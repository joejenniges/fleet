package testkit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joejenniges/fleet/internal/agentid"
	"github.com/joejenniges/fleet/internal/app"
	"github.com/joejenniges/fleet/internal/hooks"
	"github.com/joejenniges/fleet/internal/policy"
	"github.com/joejenniges/fleet/internal/procs"
	"github.com/joejenniges/fleet/internal/store"
)

// Selfcheck exercises fleet end to end without an agent: hooks are driven with
// simulated Claude Code payloads, `fleet run` and `fleet mcp` are executed as real
// subprocesses, and fixture processes are really killed. It runs in its own
// temporary FLEET_HOME and is deterministic. Returns the number of failures.
func Selfcheck(w io.Writer, keep bool) int {
	dir, err := os.MkdirTemp("", "fleet-selfcheck-")
	if err != nil {
		fmt.Fprintln(w, "FAIL setup:", err)
		return 1
	}
	prevHome, hadHome := os.LookupEnv("FLEET_HOME")
	prevAgent, hadAgent := os.LookupEnv("FLEET_AGENT_ID")
	os.Setenv("FLEET_HOME", filepath.Join(dir, "fleet-home"))
	os.Unsetenv("FLEET_AGENT_ID")
	defer func() {
		restoreEnv("FLEET_HOME", prevHome, hadHome)
		restoreEnv("FLEET_AGENT_ID", prevAgent, hadAgent)
		KillFixtures("selfcheck-")
		if keep {
			fmt.Fprintf(w, "\nkept %s\n", dir)
		} else {
			os.RemoveAll(dir)
		}
	}()
	bin, _ := os.Executable()
	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)

	sc := &selfcheck{w: w, repo: repo, bin: bin}
	sc.run()
	fmt.Fprintf(w, "\n%d passed, %d failed\n", sc.passed, sc.failed)
	return sc.failed
}

func restoreEnv(k, v string, had bool) {
	if had {
		os.Setenv(k, v)
	} else {
		os.Unsetenv(k)
	}
}

type selfcheck struct {
	w              io.Writer
	repo, bin      string
	passed, failed int
}

const (
	sessA = "selfcheck-session-a"
	sessB = "selfcheck-session-b"
	sessC = "selfcheck-session-c"
)

func (s *selfcheck) check(name string, fn func() error) {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return fn()
	}()
	if err != nil {
		s.failed++
		fmt.Fprintf(s.w, "FAIL  %s\n      %v\n", name, err)
		return
	}
	s.passed++
	fmt.Fprintf(s.w, "PASS  %s\n", name)
}

func (s *selfcheck) app() *app.App {
	a, err := app.OpenAt(s.repo)
	if err != nil {
		panic(err)
	}
	return a
}

// hook runs one hook event in-process and returns the raw output and, if present,
// hookSpecificOutput.
func (s *selfcheck) hook(event, session string, extra map[string]any) (string, map[string]any) {
	p := map[string]any{"session_id": session, "cwd": s.repo, "hook_event_name": event}
	for k, v := range extra {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	var out bytes.Buffer
	hooks.Main(event, "claude", hooks.WakeAsync, bytes.NewReader(b), &out, io.Discard)
	var parsed struct {
		HSO map[string]any `json:"hookSpecificOutput"`
	}
	_ = json.Unmarshal(out.Bytes(), &parsed)
	return out.String(), parsed.HSO
}

func bash(cmd string) map[string]any {
	return map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": cmd}}
}

func ctxOf(h map[string]any) string { s, _ := h["additionalContext"].(string); return s }

func denied(h map[string]any) (bool, string) {
	d, _ := h["permissionDecision"].(string)
	r, _ := h["permissionDecisionReason"].(string)
	return d == "deny", r
}

func (s *selfcheck) run() {
	nameA := agentid.NameFor(sessA)

	s.check("SessionStart registers the agent and tells it its name", func() error {
		_, h := s.hook(hooks.SessionStart, sessA, map[string]any{"source": "startup"})
		a := s.app()
		defer a.Close()
		ag, _ := a.St.GetAgent(sessA)
		if ag == nil || ag.Status != "active" {
			return errors.New("agent not registered")
		}
		if !strings.Contains(ctxOf(h), nameA) {
			return fmt.Errorf("context does not name the agent: %q", ctxOf(h))
		}
		return nil
	})

	s.check("A second agent sees the first one at SessionStart", func() error {
		_, h := s.hook(hooks.SessionStart, sessB, nil)
		if !strings.Contains(ctxOf(h), nameA) {
			return fmt.Errorf("B's context does not mention %s", nameA)
		}
		return nil
	})

	s.check("Known-bad command is blocked and the fix is suggested", func() error {
		a := s.app()
		defer a.Close()
		bad, err := a.AddDoc(app.AddDocInput{Kind: store.KindBad, Text: "git frobnicate", Note: "not a git command"})
		if err != nil {
			return err
		}
		if _, err := a.AddDoc(app.AddDocInput{Kind: store.KindValid, Text: "git status", Fixes: bad.Doc.ID}); err != nil {
			return err
		}
		_, h := s.hook(hooks.PreToolUse, sessA, bash("cd sub && git frobnicate --all"))
		ok, reason := denied(h)
		if !ok || !strings.Contains(reason, "git status") {
			return fmt.Errorf("expected deny mentioning the fix, got %v %q", ok, reason)
		}
		return nil
	})

	s.check("Ordinary commands are not blocked", func() error {
		out, _ := s.hook(hooks.PreToolUse, sessA, bash("git log --oneline"))
		if strings.TrimSpace(out) != "" {
			return fmt.Errorf("unexpected output %q", out)
		}
		return nil
	})

	s.check("Agents cannot approve policy (hook denies `fleet policy approve`)", func() error {
		_, h := s.hook(hooks.PreToolUse, sessA, bash("FOO=1 fleet policy approve 1"))
		if ok, _ := denied(h); !ok {
			return errors.New("not denied")
		}
		return nil
	})

	s.check("Direct edits to policy files are denied", func() error {
		_, h := s.hook(hooks.PreToolUse, sessA, map[string]any{"tool_name": "Edit",
			"tool_input": map[string]any{"file_path": filepath.Join(s.repo, ".fleet", "policy.toml")}})
		if ok, _ := denied(h); !ok {
			return errors.New("not denied")
		}
		return nil
	})

	var propID int64
	s.check("Permissive change from an agent queues for approval", func() error {
		a := s.app()
		defer a.Close()
		ag, _ := a.St.GetAgent(sessA)
		a.SetAgent(ag)
		res, err := a.ProposePolicy(policy.Change{Op: policy.OpAddRule, Level: policy.LevelRepo, Reason: "selfcheck",
			Rule: &policy.Rule{ID: "selfcheck-kill-vite", Name: []string{"vite"}, Action: policy.Kill}}, false)
		if err != nil {
			return err
		}
		if res.Status != store.PropPending || res.Applied {
			return fmt.Errorf("expected pending, got %s", res.Status)
		}
		propID = res.ProposalID
		return nil
	})

	s.check("Broadcast is delivered to other agents exactly once (not to the author)", func() error {
		_, h := s.hook(hooks.UserPromptSubmit, sessB, nil)
		if !strings.Contains(ctxOf(h), fmt.Sprintf(`"proposal":%d`, propID)) {
			return fmt.Errorf("B did not get the proposal message: %q", ctxOf(h))
		}
		if out, _ := s.hook(hooks.UserPromptSubmit, sessB, nil); strings.TrimSpace(out) != "" {
			return fmt.Errorf("message delivered twice: %q", out)
		}
		if out, _ := s.hook(hooks.UserPromptSubmit, sessA, nil); strings.Contains(out, `"proposal"`) {
			return errors.New("author received its own broadcast")
		}
		return nil
	})

	s.check("A new session is told about pending proposals and how to approve", func() error {
		_, h := s.hook(hooks.SessionStart, sessC, nil)
		c := ctxOf(h)
		if !strings.Contains(c, fmt.Sprintf("#%d", propID)) || !strings.Contains(c, "fleet policy approve") {
			return fmt.Errorf("context missing proposal guidance: %q", c)
		}
		return nil
	})

	s.check("Restrictive change applies immediately to the policy file", func() error {
		a := s.app()
		defer a.Close()
		res, err := a.ProposePolicy(policy.Change{Op: policy.OpAddRule, Level: policy.LevelRepo, Reason: "selfcheck",
			Rule: &policy.Rule{ID: "selfcheck-never-seed", CmdlineRegex: "seed-data", Action: policy.Never}}, false)
		if err != nil {
			return err
		}
		f, err := policy.ReadFile(res.File)
		if err != nil || !res.Applied || len(f.Rules) != 1 {
			return fmt.Errorf("applied=%v rules=%d err=%v", res.Applied, len(f.Rules), err)
		}
		return nil
	})

	s.check("policy test reports per-level verdicts (most restrictive wins)", func() error {
		a := s.app()
		defer a.Close()
		r := a.TestCommand("node", "node scripts/seed-data.js")
		if r.Final.Action != policy.Never {
			return fmt.Errorf("want never, got %s", r.Final.Action)
		}
		var repoMatched bool
		for _, lv := range r.Levels {
			if lv.Level == policy.LevelRepo && len(lv.Matched) == 1 {
				repoMatched = true
			}
		}
		if !repoMatched {
			return errors.New("repo level did not report its match")
		}
		return nil
	})

	s.check("PostToolUse records Bash runs (but not `fleet run`, which records itself)", func() error {
		for i := 0; i < 3; i++ {
			s.hook(hooks.PostToolUse, sessA, merge(bash("npm run lint"), map[string]any{"tool_response": map[string]any{"stdout": "ok", "exit_code": 0}}))
		}
		s.hook(hooks.PostToolUse, sessA, merge(bash("npm test"), map[string]any{"tool_response": map[string]any{"stdout": "ok", "exit_code": 0}}))
		s.hook(hooks.PostToolUse, sessA, bash("fleet run -- npm test"))
		a := s.app()
		defer a.Close()
		lint, _ := a.St.ListRuns(store.RunFilter{NormCmd: "npm run lint"})
		test, _ := a.St.ListRuns(store.RunFilter{NormCmd: "npm test"})
		if len(lint) != 3 || len(test) != 1 {
			return fmt.Errorf("lint=%d test=%d", len(lint), len(test))
		}
		if !test[0].Succeeded() || test[0].Source != "hook" {
			return fmt.Errorf("bad run row: %+v", test[0])
		}
		return nil
	})

	s.check("Top commands: ordered by executions, hidden when CLAUDE.md has them or marked known", func() error {
		a := s.app()
		defer a.Close()
		t, err := a.AddDoc(app.AddDocInput{Kind: store.KindValid, Text: "npm test"})
		if err != nil {
			return err
		}
		if !t.Doc.Verified {
			return errors.New("evidence was not found automatically")
		}
		if _, err := a.AddDoc(app.AddDocInput{Kind: store.KindValid, Text: "npm run lint"}); err != nil {
			return err
		}
		top, _ := a.TopDocs("claude", 10)
		if len(top.Top) < 2 || top.Top[0].Command != "npm run lint" {
			return fmt.Errorf("want lint first: %v", labels(top.Top))
		}
		_ = os.WriteFile(filepath.Join(s.repo, "CLAUDE.md"), []byte("Lint with `npm run lint`.\n"), 0o644)
		top, _ = a.TopDocs("claude", 10)
		if contains(labels(top.Top), "npm run lint") || len(top.Hidden) == 0 {
			return fmt.Errorf("lint should be hidden: top=%v", labels(top.Top))
		}
		if _, err := a.MarkKnown(t.Doc.ID, "CLAUDE.md"); err != nil {
			return err
		}
		top, _ = a.TopDocs("claude", 10)
		if contains(labels(top.Top), "npm test") {
			return errors.New("marked doc still shown")
		}
		top, _ = a.TopDocs("codex", 10) // codex reads AGENTS.md: CLAUDE.md marks don't apply
		if !contains(labels(top.Top), "npm test") {
			return errors.New("mark for CLAUDE.md leaked to AGENTS.md readers")
		}
		return nil
	})

	s.check("`fleet run` passes the exit code through, records it and redacts secrets", func() error {
		cmd := exec.Command(s.bin, "run", "--", s.bin, "test", "exit", "3", "--password=hunter2")
		cmd.Dir = s.repo
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 3 {
			return fmt.Errorf("exit code not passed through: %v", err)
		}
		if !strings.Contains(stderr.String(), "fleet docs add known-bad") {
			return fmt.Errorf("no recording hint on failure: %q", stderr.String())
		}
		a := s.app()
		defer a.Close()
		runs, _ := a.St.ListRuns(store.RunFilter{Contains: "test exit 3", Limit: 1})
		if len(runs) != 1 || runs[0].ExitCode == nil || *runs[0].ExitCode != 3 {
			return fmt.Errorf("run not recorded: %+v", runs)
		}
		if strings.Contains(runs[0].Command, "hunter2") || !strings.Contains(runs[0].Command, "***") {
			return fmt.Errorf("secret not redacted: %q", runs[0].Command)
		}
		return nil
	})

	s.check("Commands the policy protects run as critical automatically", func() error {
		cmd := exec.Command(s.bin, "run", "--", s.bin, "test", "exit", "0", "--label", "db:migrate")
		cmd.Dir = s.repo
		if err := cmd.Run(); err != nil {
			return err
		}
		a := s.app()
		defer a.Close()
		runs, _ := a.St.ListRuns(store.RunFilter{Contains: "db:migrate", Limit: 1})
		if len(runs) != 1 || !runs[0].Critical {
			return fmt.Errorf("not critical: %+v", runs)
		}
		return nil
	})

	s.check("SessionEnd kills processes policy allows, and spares protected ones", func() error {
		a := s.app()
		for _, r := range []policy.Rule{
			{ID: "selfcheck-kill-fixture", Name: []string{"fleet"}, ArgsContains: FixtureFlag + "selfcheck-kill", Action: policy.Kill, Grace: "2s"},
			{ID: "selfcheck-keep-fixture", ArgsContains: FixtureFlag + "selfcheck-keep", Action: policy.Never},
		} {
			r := r
			if _, err := a.ProposePolicy(policy.Change{Op: policy.OpAddRule, Level: policy.LevelUser, Rule: &r, Reason: "selfcheck"}, true); err != nil {
				a.Close()
				return err
			}
		}
		start := func(label string) (*exec.Cmd, *procs.Info, error) {
			c := exec.Command(s.bin, "test", "serve-port", FixtureFlag+label)
			if err := c.Start(); err != nil {
				return nil, nil, err
			}
			var info *procs.Info
			WaitFor(3*time.Second, func() bool {
				i, err := procs.Get(int32(c.Process.Pid))
				info = i
				return err == nil && strings.Contains(i.Cmdline, label)
			})
			if info == nil {
				return c, nil, errors.New("fixture did not start")
			}
			err := a.St.UpsertProcess(&store.Process{AgentID: sessA, PID: info.PID, PIDCtime: info.Ctime,
				Name: "fleet", Cmdline: info.Cmdline, Source: "run"})
			return c, info, err
		}
		kill, killInfo, err := start("selfcheck-kill")
		if err != nil {
			a.Close()
			return err
		}
		keep, keepInfo, err := start("selfcheck-keep")
		a.Close()
		if err != nil {
			return err
		}
		reap := func(c *exec.Cmd) { go c.Wait() } // avoid zombies being counted as alive
		reap(kill)
		reap(keep)
		defer keep.Process.Kill()

		s.hook(hooks.SessionEnd, sessA, map[string]any{"source": "exit"})
		if !WaitFor(5*time.Second, func() bool { return !procs.Alive(killInfo.PID, killInfo.Ctime) }) {
			return errors.New("kill-allowed fixture still running")
		}
		if !procs.Alive(keepInfo.PID, keepInfo.Ctime) {
			return errors.New("protected fixture was killed")
		}
		a2 := s.app()
		defer a2.Close()
		ag, _ := a2.St.GetAgent(sessA)
		if ag == nil || ag.Status != "ended" {
			return errors.New("agent not marked ended")
		}
		return nil
	})

	s.check("Agents whose process is gone are reaped", func() error {
		c := exec.Command(s.bin, "test", "exit", "0")
		if err := c.Start(); err != nil {
			return err
		}
		pid := int32(c.Process.Pid)
		info, _ := procs.Get(pid)
		_ = c.Wait()
		a := s.app()
		defer a.Close()
		var ct int64
		if info != nil {
			ct = info.Ctime
		}
		if err := a.St.UpsertAgent(&store.Agent{ID: "selfcheck-ghost", Name: "ghost", Kind: "claude", PID: pid, PIDCtime: ct, Cwd: s.repo}); err != nil {
			return err
		}
		if _, err := a.St.ReapAgents(procs.Alive); err != nil {
			return err
		}
		ag, _ := a.St.GetAgent("selfcheck-ghost")
		if ag.Status != "ended" {
			return errors.New("ghost agent still active")
		}
		return nil
	})

	s.check("Retention compacts old run output to the last N lines", func() error {
		a := s.app()
		defer a.Close()
		var out strings.Builder
		for i := 0; i < 100; i++ {
			fmt.Fprintf(&out, "line %d\n", i)
		}
		r := &store.Run{Command: "old thing", NormCmd: "old thing", Source: "run", StartedAt: time.Now().Add(-30 * 24 * time.Hour), Output: out.String()}
		id, err := a.St.StartRun(r)
		if err != nil {
			return err
		}
		if _, _, err := a.St.PruneRuns(0, 14*24*time.Hour, 5); err != nil {
			return err
		}
		got, _ := a.St.GetRun(id)
		if !got.OutputCompacted || strings.Count(got.Output, "\n") != 4 || !strings.HasSuffix(got.Output, "line 99") {
			return fmt.Errorf("not compacted: %q", got.Output)
		}
		return nil
	})

	s.runSignals()

	s.check("MCP server starts over stdio and lists its tools", func() error {
		names, err := mcpListTools(s.bin, s.repo)
		if err != nil {
			return err
		}
		for _, want := range []string{"status", "policy_test", "docs_add", "policy_propose",
			"list_waits", "send_signal", "wait_for_signal", "register_wait"} {
			if !contains(names, want) {
				return fmt.Errorf("missing tool %s in %v", want, names)
			}
		}
		for _, forbidden := range []string{"policy_approve", "preempt"} {
			if contains(names, forbidden) {
				return fmt.Errorf("human-only tool %s exposed", forbidden)
			}
		}
		return nil
	})
}

// mcpListTools runs `fleet mcp`, performs the MCP handshake and returns tool names.
func mcpListTools(bin, cwd string) ([]string, error) {
	cmd := exec.Command(bin, "mcp")
	cmd.Dir = cwd
	in, _ := cmd.StdinPipe()
	outPipe, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
	send := func(v any) { b, _ := json.Marshal(v); in.Write(append(b, '\n')) }
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "fleet-selfcheck", "version": "0"}}})
	type resp struct {
		ID     int `json:"id"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error any `json:"error"`
	}
	lines := make(chan []byte)
	go func() {
		sc := bufio.NewScanner(outPipe)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			lines <- append([]byte(nil), sc.Bytes()...)
		}
		close(lines)
	}()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return nil, errors.New("server exited")
			}
			var r resp
			if json.Unmarshal(l, &r) != nil {
				continue
			}
			if r.Error != nil {
				return nil, fmt.Errorf("error: %v", r.Error)
			}
			switch r.ID {
			case 1:
				send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
				send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
			case 2:
				var names []string
				for _, t := range r.Result.Tools {
					names = append(names, t.Name)
				}
				return names, nil
			}
		case <-timeout:
			return nil, errors.New("timed out waiting for MCP responses")
		}
	}
}

func merge(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

func labels(ds []*store.Doc) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Label())
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
