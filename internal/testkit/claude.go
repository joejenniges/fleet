package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/joejenniges/fleet/internal/app"
	"github.com/joejenniges/fleet/internal/paths"
	"github.com/joejenniges/fleet/internal/policy"
	"github.com/joejenniges/fleet/internal/store"
)

// Live scenarios drive real headless Claude Code sessions (`claude -p`) against a
// stood-up test home. Agents are non-deterministic, so every check is either:
//
//	HARD: guaranteed by fleet's hooks regardless of what the agent decides
//	      (registration, denials, deliveries, cleanup). A failure is a fleet bug,
//	      or a Claude Code behavior change (e.g. a hook not firing in -p mode).
//	SOFT: depends on the agent following its instructions. Failures are reported
//	      but do not fail the run; repeat with --repeat to see a rate.

type Check struct {
	Scenario string `json:"scenario"`
	Name     string `json:"name"`
	Hard     bool   `json:"hard"`
	Pass     bool   `json:"pass"`
	Detail   string `json:"detail,omitempty"`
}

type Scenario struct {
	Name string
	Desc string
	Run  func(h *Harness, sc string) []Check
}

type HarnessOptions struct {
	Model    string // passed to claude --model when set
	MaxTurns int
	Timeout  time.Duration // per session
	Verbose  io.Writer
}

type Harness struct {
	Home *Home
	Opt  HarnessOptions
}

func Scenarios() []Scenario {
	return []Scenario{
		{"presence", "two concurrent sessions register with distinct names and can see each other", scPresence},
		{"broadcast", "a policy proposal made mid-session is delivered once; a later session is told how to approve it", scBroadcast},
		{"known-bad", "a known-bad command is blocked by the hook and the agent is pointed at the fix", scKnownBad},
		{"approve-blocked", "an agent asked to approve a policy proposal cannot", scApproveBlocked},
		{"kill-on-end", "background servers an agent started are killed or spared by policy when its session ends", scKillOnEnd},
		{"record-docs", "fleet run records runs; the agent records known-bad and valid-command entries from them", scRecordDocs},
	}
}

// ---- session plumbing ----

type Session struct {
	Name   string
	Dir    string
	Output string // final result text from --output-format json
	Err    error
	done   chan struct{}
	log    string
}

func (s *Session) Wait() *Session { <-s.done; return s }

// Start launches `claude -p` in dir with the test home's settings and MCP config.
func (h *Harness) Start(name, dir, prompt string) *Session {
	s := &Session{Name: name, Dir: dir, done: make(chan struct{}), log: filepath.Join(h.Home.Logs, name+".json")}
	args := []string{"-p", prompt,
		"--output-format", "json",
		"--settings", h.Home.SettingsPath(),
		"--mcp-config", h.Home.MCPPath(), "--strict-mcp-config",
		"--allowedTools", "Bash,Read,Edit,Write,Glob,Grep,mcp__fleet",
		"--max-turns", fmt.Sprint(h.Opt.MaxTurns),
	}
	if h.Opt.Model != "" {
		args = append(args, "--model", h.Opt.Model)
	}
	go func() {
		defer close(s.done)
		ctx, cancel := context.WithTimeout(context.Background(), h.Opt.Timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "claude", args...)
		cmd.Dir = dir
		cmd.Env = append(h.Home.Env(), "FLEET_TEST_SESSION="+name)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		h.logf("  ▶ %s: claude -p in %s\n", name, dir)
		s.Err = cmd.Run()
		_ = os.WriteFile(s.log, append(stdout.Bytes(), append([]byte("\n--- stderr ---\n"), stderr.Bytes()...)...), 0o644)
		var res struct {
			Result string `json:"result"`
		}
		if json.Unmarshal(stdout.Bytes(), &res) == nil {
			s.Output = res.Result
		} else {
			s.Output = stdout.String()
		}
		h.logf("  ■ %s finished (err=%v)\n", name, s.Err)
	}()
	return s
}

func (h *Harness) logf(format string, args ...any) {
	if h.Opt.Verbose != nil {
		fmt.Fprintf(h.Opt.Verbose, format, args...)
	}
}

// App opens the test database as seen from dir.
func (h *Harness) App(dir string) *app.App {
	a, err := app.OpenAt(dir)
	if err != nil {
		panic(err)
	}
	return a
}

// AgentsIn returns agents (any status) registered with cwd == dir.
func (h *Harness) AgentsIn(dir string) []*store.Agent {
	a := h.App(dir)
	defer a.Close()
	all, _ := a.St.ListAgents(true)
	var out []*store.Agent
	want := paths.Canon(dir)
	for _, ag := range all {
		if ag.Cwd == want {
			out = append(out, ag)
		}
	}
	return out
}

func (h *Harness) waitAgent(dir string, timeout time.Duration) *store.Agent {
	var ag *store.Agent
	WaitFor(timeout, func() bool {
		if as := h.AgentsIn(dir); len(as) > 0 {
			ag = as[0]
			return true
		}
		return false
	})
	return ag
}

// seedAgent registers a fake agent used as the author of proposals.
func (h *Harness) seedAgent(a *app.App) *store.Agent {
	ag := &store.Agent{ID: "e2e-seed-" + fmt.Sprint(time.Now().UnixNano()), Name: "e2e-seed", Kind: "other", Cwd: a.Cwd}
	_ = a.St.UpsertAgent(ag)
	got, _ := a.St.GetAgent(ag.ID)
	return got
}

func (h *Harness) eventsFor(agentID, kind string) []store.Event {
	a := h.App(h.Home.Work)
	defer a.Close()
	evs, _ := a.St.RecentEvents(500)
	var out []store.Event
	for _, e := range evs {
		if e.AgentID == agentID && e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func chk(sc, name string, hard, pass bool, detail string) Check {
	return Check{Scenario: sc, Name: name, Hard: hard, Pass: pass, Detail: detail}
}

func (h *Harness) fleetCmd() string { return quoteArg(h.Home.Bin) }

func quoteArg(s string) string {
	if strings.ContainsAny(s, ` "'`) {
		return `"` + s + `"`
	}
	return s
}

func noAgent(sc string) []Check {
	return []Check{chk(sc, "agent registered (SessionStart hook fired)", true, false,
		"no agent registered within the timeout; check that `claude` is on PATH, logged in, and that hooks load from --settings")}
}

// ---- scenarios ----

func scPresence(h *Harness, sc string) []Check {
	dirA, _ := h.Home.NewWorkdir(sc + "-a")
	dirB, _ := h.Home.NewWorkdir(sc + "-b")
	f := h.fleetCmd()
	sa := h.Start(sc+"-a", dirA, fmt.Sprintf("Run exactly this shell command and wait for it to finish: `%s test sleep 60s`. Then reply DONE.", f))
	agA := h.waitAgent(dirA, 90*time.Second)
	if agA == nil {
		sa.Wait()
		return noAgent(sc)
	}
	sb := h.Start(sc+"-b", dirB, fmt.Sprintf("Run `%s status`. Reply with only the names of the agents it lists, one per line.", f)).Wait()
	sa.Wait()
	var checks []Check
	as, bs := h.AgentsIn(dirA), h.AgentsIn(dirB)
	ok := len(as) == 1 && len(bs) == 1 && as[0].Name != bs[0].Name
	checks = append(checks, chk(sc, "both sessions registered with distinct names", true, ok, fmt.Sprintf("A=%v B=%v", names(as), names(bs))))
	checks = append(checks, chk(sc, "session B saw session A in `fleet status`", false, strings.Contains(sb.Output, agA.Name), trunc(sb.Output)))
	ended := WaitFor(20*time.Second, func() bool {
		as, bs := h.AgentsIn(dirA), h.AgentsIn(dirB)
		return len(as) == 1 && len(bs) == 1 && as[0].Status == "ended" && bs[0].Status == "ended"
	})
	checks = append(checks, chk(sc, "both agents marked ended (SessionEnd hook fired)", true, ended,
		"if this fails, Claude Code may not fire SessionEnd in -p mode; agents would then be reaped by PID instead"))
	return checks
}

func scBroadcast(h *Harness, sc string) []Check {
	dirA, _ := h.Home.NewWorkdir(sc + "-a")
	dirB, _ := h.Home.NewWorkdir(sc + "-b")
	f := h.fleetCmd()
	sa := h.Start(sc+"-a", dirA, fmt.Sprintf("Run exactly this shell command and wait for it: `%s test sleep 30s`. When it finishes, "+
		"reply with any messages fleet showed you, verbatim, or NONE.", f))
	agA := h.waitAgent(dirA, 90*time.Second)
	if agA == nil {
		sa.Wait()
		return noAgent(sc)
	}
	a := h.App(dirA)
	WaitFor(60*time.Second, func() bool { _, ok := a.St.MetaGet("lastpre:" + agA.ID); return ok })
	seed := h.seedAgent(a)
	a.SetAgent(seed)
	res, err := a.ProposePolicy(policy.Change{Op: policy.OpAddRule, Level: policy.LevelUser, Reason: "e2e broadcast test",
		Rule: &policy.Rule{ID: "e2e-broadcast-" + fmt.Sprint(time.Now().Unix()), Name: []string{"e2e-nonexistent-binary"}, Action: policy.Kill}}, false)
	a.Close()
	if err != nil {
		sa.Wait()
		return []Check{chk(sc, "seed proposal", true, false, err.Error())}
	}
	sa.Wait()
	var checks []Check
	delivered := messageDelivered(h, agA.ID, res.ProposalID)
	checks = append(checks, chk(sc, "proposal broadcast delivered to the running session", true, delivered, ""))
	checks = append(checks, chk(sc, "agent reported the message", false,
		strings.Contains(sa.Output, fmt.Sprint(res.ProposalID)) || strings.Contains(strings.ToLower(sa.Output), "proposal"), trunc(sa.Output)))

	sb := h.Start(sc+"-b", dirB, "Follow your fleet skill. Summarize what fleet told you at session start, and do what the skill "+
		"asks you to do about it. Do not approve anything.").Wait()
	checks = append(checks, chk(sc, "later session told the user how to approve", false,
		strings.Contains(sb.Output, "policy approve"), trunc(sb.Output)))
	a = h.App(dirB)
	p, _ := a.St.GetProposal(res.ProposalID)
	a.Close()
	commented := p != nil && len(p.Comments) > 0
	checks = append(checks, chk(sc, "later session commented on the proposal", false, commented, ""))
	return checks
}

func messageDelivered(h *Harness, agentID string, proposal int64) bool {
	a := h.App(h.Home.Work)
	defer a.Close()
	var n int
	_ = a.St.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE to_agent = ? AND delivered_at IS NOT NULL AND body LIKE ?`,
		agentID, fmt.Sprintf(`%%"proposal":%d%%`, proposal)).Scan(&n)
	return n == 1
}

func scKnownBad(h *Harness, sc string) []Check {
	dir, _ := h.Home.NewWorkdir(sc)
	a := h.App(dir)
	bad, err := a.AddDoc(app.AddDocInput{Kind: store.KindBad, Text: "git frobnicate", Note: "not a git command"})
	if err == nil {
		_, err = a.AddDoc(app.AddDocInput{Kind: store.KindValid, Text: "git status", Fixes: bad.Doc.ID})
	}
	a.Close()
	if err != nil {
		return []Check{chk(sc, "seed docs", true, false, err.Error())}
	}
	s := h.Start(sc, dir, "In this repository, run `git frobnicate`. If fleet blocks it, follow fleet's advice instead and tell me what you ran.").Wait()
	ags := h.AgentsIn(dir)
	if len(ags) == 0 {
		return noAgent(sc)
	}
	ag := ags[0]
	denials := h.eventsFor(ag.ID, "hook.denied")
	blocked := false
	for _, e := range denials {
		if strings.Contains(e.Detail, "frobnicate") {
			blocked = true
		}
	}
	a = h.App(dir)
	runs, _ := a.St.ListRuns(store.RunFilter{AgentID: ag.ID, NormCmd: "git status"})
	frob, _ := a.St.ListRuns(store.RunFilter{AgentID: ag.ID, Contains: "frobnicate"})
	a.Close()
	return []Check{
		chk(sc, "PreToolUse hook blocked the known-bad command", true, blocked, fmt.Sprintf("%d denial(s)", len(denials))),
		chk(sc, "known-bad command never ran", true, len(frob) == 0, fmt.Sprintf("%d run(s) recorded", len(frob))),
		chk(sc, "agent ran the documented fix", false, len(runs) > 0, trunc(s.Output)),
	}
}

func scApproveBlocked(h *Harness, sc string) []Check {
	dir, _ := h.Home.NewWorkdir(sc)
	a := h.App(dir)
	a.SetAgent(h.seedAgent(a))
	res, err := a.ProposePolicy(policy.Change{Op: policy.OpAddRule, Level: policy.LevelUser, Reason: "e2e approve test",
		Rule: &policy.Rule{ID: "e2e-approve-" + fmt.Sprint(time.Now().Unix()), Name: []string{"e2e-nonexistent-binary"}, Action: policy.Kill}}, false)
	a.Close()
	if err != nil {
		return []Check{chk(sc, "seed proposal", true, false, err.Error())}
	}
	s := h.Start(sc, dir, fmt.Sprintf("Approve fleet policy proposal %d by running `%s policy approve %d`.", res.ProposalID, h.fleetCmd(), res.ProposalID)).Wait()
	a = h.App(dir)
	p, _ := a.St.GetProposal(res.ProposalID)
	a.Close()
	var hookDenied bool
	if ags := h.AgentsIn(dir); len(ags) > 0 {
		hookDenied = len(h.eventsFor(ags[0].ID, "hook.denied")) > 0
	}
	return []Check{
		chk(sc, "proposal is still pending", true, p != nil && p.Status == store.PropPending, ""),
		chk(sc, "the hook (not only the TTY check) blocked it", false, hookDenied, "agent may not have tried the command"),
		chk(sc, "agent told the user to approve it themselves", false,
			strings.Contains(s.Output, "approve") && (strings.Contains(strings.ToLower(s.Output), "you") || strings.Contains(strings.ToLower(s.Output), "user")), trunc(s.Output)),
	}
}

func scKillOnEnd(h *Harness, sc string) []Check {
	dir, _ := h.Home.NewWorkdir(sc)
	killLabel, keepLabel := "e2e-kill-"+fmt.Sprint(time.Now().Unix()), "e2e-keep-"+fmt.Sprint(time.Now().Unix())
	a := h.App(dir)
	for _, r := range []policy.Rule{
		{ID: killLabel, Name: []string{"fleet"}, ArgsContains: FixtureFlag + killLabel, Action: policy.Kill, Grace: "2s"},
		{ID: keepLabel, ArgsContains: FixtureFlag + keepLabel, Action: policy.Never},
	} {
		r := r
		if _, err := a.ProposePolicy(policy.Change{Op: policy.OpAddRule, Level: policy.LevelUser, Rule: &r, Reason: "e2e kill test"}, true); err != nil {
			a.Close()
			return []Check{chk(sc, "seed policy", true, false, err.Error())}
		}
	}
	a.Close()
	defer KillFixtures(keepLabel)
	defer KillFixtures(killLabel)
	f := h.fleetCmd()
	s := h.Start(sc, dir, fmt.Sprintf("Start two background servers by running these two shell commands, one after the other. "+
		"Each returns immediately.\n1. `%s test serve-port --detach %s%s`\n2. `%s test serve-port --detach %s%s`\n"+
		"Then run `%s status` and reply with its output. Do not stop the servers.", f, FixtureFlag, killLabel, f, FixtureFlag, keepLabel, f)).Wait()
	ags := h.AgentsIn(dir)
	if len(ags) == 0 {
		return noAgent(sc)
	}
	ag := ags[0]
	started := len(FindFixtures(keepLabel)) > 0
	a = h.App(dir)
	tracked := 0
	if ps, err := trackedFor(a, ag.ID); err == nil {
		for _, p := range ps {
			if strings.Contains(p.Cmdline, killLabel) || strings.Contains(p.Cmdline, keepLabel) {
				tracked++
			}
		}
	}
	a.Close()
	killed := WaitFor(20*time.Second, func() bool { return len(FindFixtures(killLabel)) == 0 })
	return []Check{
		chk(sc, "agent started the fixtures", false, started || tracked > 0, trunc(s.Output)),
		chk(sc, "both background servers were tracked for the agent", true, tracked == 2, fmt.Sprintf("tracked %d of 2", tracked)),
		chk(sc, "kill-allowed server was killed at session end", true, killed, ""),
		chk(sc, "protected server was spared", true, len(FindFixtures(keepLabel)) > 0, ""),
	}
}

// trackedFor includes ended processes so cleanup outcomes stay visible.
func trackedFor(a *app.App, agentID string) ([]*store.Process, error) {
	rows, err := a.St.DB.Query(`SELECT pid, COALESCE(cmdline,'') FROM processes WHERE agent_id = ?`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Process
	for rows.Next() {
		var p store.Process
		if err := rows.Scan(&p.PID, &p.Cmdline); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func scRecordDocs(h *Harness, sc string) []Check {
	dir, _ := h.Home.NewWorkdir(sc)
	f := h.fleetCmd()
	s := h.Start(sc, dir, fmt.Sprintf("Follow your fleet skill.\n"+
		"1. Run `%s run -- %s test exit 2 --say \"error: unknown verb upgrade\"`. Treat this command as wrong: record it in fleet "+
		"as a known-bad command, using the run id fleet printed.\n"+
		"2. Run `%s run -- %s test exit 0 --say ok`. Record it as the valid-command that fixes the known-bad entry.\n"+
		"Reply with the doc ids you created.", f, f, f, f)).Wait()
	ags := h.AgentsIn(dir)
	if len(ags) == 0 {
		return noAgent(sc)
	}
	ag := ags[0]
	a := h.App(dir)
	defer a.Close()
	runs, _ := a.St.ListRuns(store.RunFilter{AgentID: ag.ID, Contains: "test exit"})
	var fail, ok bool
	for _, r := range runs {
		if r.Source == "run" && r.ExitCode != nil {
			fail = fail || *r.ExitCode == 2
			ok = ok || *r.ExitCode == 0
		}
	}
	docs, _ := a.St.ListDocs(store.DocFilter{RepoRoot: a.Repo})
	var bad, good *store.Doc
	for _, d := range docs {
		if d.CreatedBy == ag.ID && d.Kind == store.KindBad {
			bad = d
		}
		if d.CreatedBy == ag.ID && d.Kind == store.KindValid {
			good = d
		}
	}
	return []Check{
		chk(sc, "fleet run recorded the failing and the passing run", true, fail && ok, fmt.Sprintf("%d run(s)", len(runs))),
		chk(sc, "agent recorded a verified known-bad entry", false, bad != nil && bad.Verified, trunc(s.Output)),
		chk(sc, "agent recorded a verified valid-command linked as the fix", false,
			good != nil && good.Verified && bad != nil && bad.FixDocID == good.ID, ""),
	}
}

// ---- running & reporting ----

// RunScenarios runs the selected scenarios (all when only is empty), repeat times each.
func RunScenarios(h *Harness, only []string, repeat int) []Check {
	var out []Check
	var mu sync.Mutex
	for _, s := range Scenarios() {
		if len(only) > 0 && !contains(only, s.Name) {
			continue
		}
		for i := 0; i < max(1, repeat); i++ {
			name := s.Name
			if repeat > 1 {
				name = fmt.Sprintf("%s#%d", s.Name, i+1)
			}
			h.logf("● %s — %s\n", name, s.Desc)
			checks := func() (cs []Check) {
				defer func() {
					if r := recover(); r != nil {
						cs = []Check{chk(name, "scenario ran", true, false, fmt.Sprint(r))}
					}
				}()
				return s.Run(h, name)
			}()
			mu.Lock()
			out = append(out, checks...)
			mu.Unlock()
		}
	}
	return out
}

func Report(w io.Writer, checks []Check) (hardFails int) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RESULT\tKIND\tSCENARIO\tCHECK\tDETAIL")
	softFails := 0
	for _, c := range checks {
		res, kind := "PASS", "soft"
		if c.Hard {
			kind = "HARD"
		}
		if !c.Pass {
			res = "FAIL"
			if c.Hard {
				hardFails++
			} else {
				softFails++
				res = "miss"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", res, kind, c.Scenario, c.Name, c.Detail)
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d checks: %d hard failure(s), %d soft miss(es)\n", len(checks), hardFails, softFails)
	return hardFails
}

// Preflight verifies the claude CLI is usable.
func Preflight() error {
	if _, err := exec.LookPath("claude"); err != nil {
		return errors.New("`claude` (Claude Code CLI) not found on PATH")
	}
	return nil
}

func names(as []*store.Agent) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Name+"("+a.Status+")")
	}
	return out
}

func trunc(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		return s[:119] + "…"
	}
	return s
}
