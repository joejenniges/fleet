// Package hooks implements `fleet hook <event>`: agent lifecycle hooks that read a JSON
// payload on stdin. Every hook fails open: on any error or timeout it exits 0 with no
// output, so a broken fleet never stalls an agent.
package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joejenniges/fleet/internal/agentid"
	"github.com/joejenniges/fleet/internal/app"
	"github.com/joejenniges/fleet/internal/cmdtext"
	"github.com/joejenniges/fleet/internal/config"
	"github.com/joejenniges/fleet/internal/paths"
	"github.com/joejenniges/fleet/internal/procs"
	"github.com/joejenniges/fleet/internal/store"
)

// Events, as passed to `fleet hook <event>`.
const (
	SessionStart     = "session-start"
	UserPromptSubmit = "user-prompt-submit"
	PreToolUse       = "pre-tool-use"
	PostToolUse      = "post-tool-use"
	SessionEnd       = "session-end"
	Stop             = "stop"
)

var Events = []string{SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop, SessionEnd}

// Wake modes for the Stop hook.
const (
	// WakeAsync: installed with asyncRewake. The hook runs in the background after
	// the agent's turn ends and exits 2 when a signal arrives, which wakes the agent.
	WakeAsync = "async"
	// WakeBlock: a normal Stop hook that holds the turn open until a signal arrives
	// (up to signals.block_max), then exits 2 so the agent continues.
	WakeBlock = "block"
)

// claudeEventName maps our event names to Claude Code's hookEventName values.
var claudeEventName = map[string]string{
	SessionStart: "SessionStart", UserPromptSubmit: "UserPromptSubmit",
	PreToolUse: "PreToolUse", PostToolUse: "PostToolUse", SessionEnd: "SessionEnd", Stop: "Stop",
}

// Payload is the subset of the Claude Code hook input fleet uses. Other agents'
// adapters translate into this shape.
type Payload struct {
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	Source        string          `json:"source,omitempty"`
	ToolName      string          `json:"tool_name,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse  json.RawMessage `json:"tool_response,omitempty"`
}

type toolInput struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
}

// Main runs a hook and returns its exit code. Every event fails open (exit 0, no
// output) on errors. The only non-zero exit is the Stop hook's deliberate 2, which
// tells Claude Code to wake/continue the agent with the signal data on stderr.
func Main(event, kind, wake string, stdin io.Reader, stdout, stderr io.Writer) int {
	raw, err := io.ReadAll(io.LimitReader(stdin, 8<<20))
	if err != nil {
		return 0
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		debugf("bad payload: %v", err)
		return 0
	}
	if event == Stop {
		return runStop(&p, kind, wake, stderr)
	}
	timeout := 2 * time.Second
	if cfg, err := config.Load(); err == nil {
		timeout = cfg.Hooks.Timeout.Duration
		if event == SessionStart {
			timeout = cfg.Hooks.SessionStartTime.Duration
		}
		if event == SessionEnd {
			timeout += 10 * time.Second // cleanup may wait for graceful shutdowns
		}
	}
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				debugf("panic: %v", r)
				out.Reset()
			}
			close(done)
		}()
		handle(event, kind, &p, &out)
	}()
	select {
	case <-done:
		_, _ = stdout.Write(out.Bytes())
	case <-time.After(timeout):
		debugf("%s timed out after %s", event, timeout)
	}
	return 0
}

func debugf(format string, args ...any) {
	if os.Getenv("FLEET_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "fleet hook: "+format+"\n", args...)
	}
}

func handle(event, kind string, p *Payload, out *bytes.Buffer) {
	cwd := p.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	a, err := app.OpenAt(cwd)
	if err != nil {
		debugf("open: %v", err)
		return
	}
	defer a.Close()

	ag := lookupOrRegister(a, p, kind, event == SessionStart)
	if ag == nil {
		return
	}
	a.SetAgent(ag)
	if event != SessionEnd {
		a.St.TouchAgent(ag.ID)
	}

	switch event {
	case SessionStart:
		a.Housekeep()
		writeContext(out, event, a.SessionContext(ag))
	case UserPromptSubmit:
		if msg := a.RenderInbox(ag.ID); msg != "" {
			writeContext(out, event, msg)
		}
	case PreToolUse:
		_ = a.St.MetaSet("lastpre:"+ag.ID, fmt.Sprint(time.Now().UnixMilli()))
		if reason := preToolDeny(a, p); reason != "" {
			a.St.LogEvent(ag.ID, "hook.denied", map[string]string{"tool": p.ToolName, "reason": reason})
			writeDeny(out, reason)
		}
	case PostToolUse:
		if p.ToolName == "Bash" {
			recordBash(a, ag, p)
			a.ScanAgentProcesses(ag)
		}
		if msg := a.RenderInbox(ag.ID); msg != "" {
			writeContext(out, event, msg)
		}
	case SessionEnd:
		a.CleanupAgent(ag)
		_ = a.St.EndAgent(ag.ID)
		a.St.LogEvent(ag.ID, "agent.ended", map[string]string{"reason": p.Source})
	}
}

// lookupOrRegister finds the agent for this session, registering it at SessionStart
// (or lazily, when fleet was installed mid-session).
func lookupOrRegister(a *app.App, p *Payload, kind string, register bool) *store.Agent {
	id := p.SessionID
	if id != "" && !register {
		if ag, _ := a.St.GetAgent(id); ag != nil && ag.Status != "ended" {
			return ag
		}
	}
	if id == "" {
		if ag := a.Agent(); ag != nil {
			return ag
		}
		id = agentid.NewID()
	}
	ag := &store.Agent{ID: id, Name: agentid.NameFor(id), Kind: kind, SessionID: p.SessionID,
		Cwd: a.Cwd, RepoRoot: a.Repo}
	if proc, ok := procs.AgentProcess(); ok {
		ag.PID, ag.PIDCtime = proc.PID, proc.Ctime
	}
	if err := a.St.UpsertAgent(ag); err != nil {
		debugf("register: %v", err)
		return nil
	}
	a.St.LogEvent(ag.ID, "agent.registered", map[string]any{"name": ag.Name, "pid": ag.PID, "cwd": ag.Cwd})
	got, _ := a.St.GetAgent(id)
	return got
}

func preToolDeny(a *app.App, p *Payload) string {
	var ti toolInput
	_ = json.Unmarshal(p.ToolInput, &ti)
	switch p.ToolName {
	case "Bash":
		if ti.Command == "" {
			return ""
		}
		for _, seg := range cmdtext.Segments(ti.Command) {
			if sub := cmdtext.FleetSubcommand(seg); sub == "policy approve" || sub == "policy reject" {
				return "Only the user can approve or reject fleet policy changes. Tell the user to run " +
					"`fleet policy pending` and `fleet policy approve <id>` in their own terminal, and give " +
					"your opinion; use `fleet policy comment` to record it."
			}
		}
		if a.Cfg.Docs.KnownBadAction == "off" {
			return ""
		}
		m, err := a.MatchKnownBad(ti.Command)
		if err != nil || m == nil {
			return ""
		}
		var b strings.Builder
		fmt.Fprintf(&b, "fleet: `%s` is a known-bad command (doc #%d)", m.Segment, m.Bad.ID)
		if m.Bad.Body != "" {
			fmt.Fprintf(&b, ": %s", m.Bad.Body)
		}
		b.WriteString(".")
		if m.Fix != nil {
			fmt.Fprintf(&b, " Use instead: `%s` (doc #%d).", m.Fix.Label(), m.Fix.ID)
		}
		fmt.Fprintf(&b, " If you are sure this entry is wrong now, tell the user and retire it with "+
			"`fleet docs retire %d --reason \"…\"`.", m.Bad.ID)
		return b.String()
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		path := ti.FilePath
		if path == "" {
			path = ti.NotebookPath
		}
		if isManagedConfig(path, a.Cwd) {
			return "fleet policy and config files are changed through commands so they stay valid: use " +
				"`fleet policy add|remove|set-default --level user|repo … --reason \"…\"`. Permissive changes " +
				"queue for the user's approval."
		}
	}
	return ""
}

func isManagedConfig(path, cwd string) bool {
	if path == "" {
		return false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	c := paths.Canon(path)
	base := filepath.Base(c)
	if base == "policy.toml" && filepath.Base(filepath.Dir(c)) == ".fleet" {
		return true
	}
	ucd, _ := paths.UserConfigDir()
	for _, dir := range []string{ucd, paths.GlobalConfigDir()} {
		if dir == "" {
			continue
		}
		d := paths.Canon(dir)
		if (base == "policy.toml" || base == "config.toml") && filepath.Dir(c) == d {
			return true
		}
	}
	return false
}

func recordBash(a *app.App, ag *store.Agent, p *Payload) {
	var ti toolInput
	_ = json.Unmarshal(p.ToolInput, &ti)
	if ti.Command == "" || cmdtext.IsFleetRun(ti.Command) {
		return // `fleet run` records itself
	}
	exit, output := parseToolResponse(p.ToolResponse)
	r := &store.Run{AgentID: ag.ID, RepoRoot: a.Repo, Cwd: a.Cwd, Command: a.Red.Command(ti.Command),
		NormCmd: cmdtext.Normalize(ti.Command, a.Red), Argv0: cmdtext.Argv0(ti.Command), Source: "hook",
		ExitCode: exit, EndedAt: time.Now(), OS: osName()}
	if output != "" {
		max := a.Cfg.Runs.MaxOutputBytes
		if len(output) > max {
			output = "[… earlier output truncated …]\n" + output[len(output)-max:]
		}
		r.Output = a.Red.Text(output)
		r.OutputLines = strings.Count(output, "\n") + 1
	}
	_, _ = a.St.StartRun(r)
}

// parseToolResponse extracts an exit code and output from an agent's Bash tool
// response. The shape varies by agent and version, so this is best-effort; an
// unknown exit code is stored as NULL.
func parseToolResponse(raw json.RawMessage) (*int, string) {
	if len(raw) == 0 {
		return nil, ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return nil, s
		}
		return nil, ""
	}
	var exit *int
	for _, k := range []string{"exit_code", "exitCode", "returnCode", "return_code", "code", "status"} {
		if v, ok := m[k].(float64); ok {
			e := int(v)
			exit = &e
			break
		}
	}
	var parts []string
	for _, k := range []string{"stdout", "stderr", "output"} {
		if s, ok := m[k].(string); ok && s != "" {
			parts = append(parts, s)
		}
	}
	if exit == nil {
		if b, ok := m["interrupted"].(bool); ok && b {
			e := 130
			exit = &e
		}
	}
	return exit, strings.Join(parts, "\n")
}

func writeContext(out *bytes.Buffer, event, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	_ = json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{"hookEventName": claudeEventName[event], "additionalContext": text},
	})
}

func writeDeny(out *bytes.Buffer, reason string) {
	_ = json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": reason,
		},
	})
}
