// Package install wires fleet into coding agents (MCP server, hooks, skill).
package install

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joejenniges/fleet/internal/config"
)

//go:embed skill/SKILL.md
var SkillMD string

// Wake modes for the Stop hook (see hooks.WakeAsync / hooks.WakeBlock).
const (
	WakeAsync = "async"
	WakeBlock = "block"
)

// HookEntries returns the Claude Code hook configuration for a fleet binary.
// Timeouts are in seconds (Claude Code's unit).
//
// The Stop hook is what lets a waiting agent be woken by a signal:
//   - async (default): runs in the background with asyncRewake; exit 2 wakes the
//     idle agent and shows the signal data as a system reminder.
//   - block: a normal Stop hook that holds the turn open (up to signals.block_max)
//     and exits 2 so the agent continues. For Claude Code versions without asyncRewake.
func HookEntries(bin, wake string) map[string][]map[string]any {
	cmd := func(ev string) string { return quote(bin) + " hook " + ev + " --agent claude" }
	entry := func(matcher, ev string, timeout int) map[string]any {
		e := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd(ev), "timeout": timeout}}}
		if matcher != "" {
			e["matcher"] = matcher
		}
		return e
	}
	var stop map[string]any
	if wake == WakeBlock {
		stop = map[string]any{"hooks": []any{map[string]any{"type": "command",
			"command": cmd("stop") + " --wake block", "timeout": blockStopTimeout()}}}
	} else {
		stop = map[string]any{"hooks": []any{map[string]any{"type": "command",
			"command": cmd("stop") + " --wake async", "asyncRewake": true, "timeout": asyncStopTimeout()}}}
	}
	return map[string][]map[string]any{
		"SessionStart":     {entry("", "session-start", 10)},
		"UserPromptSubmit": {entry("", "user-prompt-submit", 5)},
		"PreToolUse":       {entry("Bash|Edit|Write|MultiEdit|NotebookEdit", "pre-tool-use", 5)},
		"PostToolUse":      {entry("Bash", "post-tool-use", 5)},
		"Stop":             {stop},
		"SessionEnd":       {entry("", "session-end", 30)},
	}
}

func quote(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// BinaryPath prefers plain "fleet" when it resolves to this executable, so upgrades
// that replace the binary on PATH keep working; otherwise the absolute path.
func BinaryPath() string {
	self, err := os.Executable()
	if err != nil {
		return "fleet"
	}
	self, _ = filepath.EvalSymlinks(self)
	if p, err := exec.LookPath("fleet"); err == nil {
		if rp, _ := filepath.EvalSymlinks(p); rp == self {
			return "fleet"
		}
	}
	return self
}

// isFleetHook reports whether a hook command was installed by fleet.
func isFleetHook(cmd string) bool {
	return strings.Contains(cmd, " hook ") && strings.Contains(cmd, "--agent claude") &&
		(strings.Contains(cmd, "fleet") || strings.Contains(cmd, "FLEET"))
}

// asyncStopTimeout is the Claude Code timeout (seconds) for the background Stop hook.
// WHY: Claude Code enforces `timeout` on asyncRewake hooks (unlike plain async ones),
// with a 600 s default. Without an explicit value the waiter is killed after 10
// minutes and an idle agent never wakes for a longer wait. The hook stops itself at
// signals.max_wait, so this only needs to be at least that, plus slack.
func asyncStopTimeout() int {
	maxWait := 24 * time.Hour
	if cfg, err := config.Load(); err == nil && cfg.Signals.MaxWait.Duration > maxWait {
		maxWait = cfg.Signals.MaxWait.Duration
	}
	return int((maxWait + time.Minute) / time.Second)
}

// blockStopTimeout keeps the blocking Stop hook's Claude Code timeout above
// signals.block_max, so a raised block_max isn't silently cut off at 600 s.
func blockStopTimeout() int {
	blockMax := 9 * time.Minute
	if cfg, err := config.Load(); err == nil && cfg.Signals.BlockMax.Duration > 0 {
		blockMax = cfg.Signals.BlockMax.Duration
	}
	if t := int((blockMax + time.Minute) / time.Second); t > 600 {
		return t
	}
	return 600
}

// MergeHooks returns settings with fleet's hooks replacing any earlier fleet hooks,
// leaving everything else untouched.
func MergeHooks(settings map[string]any, bin, wake string) map[string]any {
	if settings == nil {
		settings = map[string]any{}
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for ev, entries := range HookEntries(bin, wake) {
		var kept []any
		if cur, ok := hooks[ev].([]any); ok {
			for _, e := range cur {
				if !entryIsFleet(e) {
					kept = append(kept, e)
				}
			}
		}
		for _, e := range entries {
			kept = append(kept, e)
		}
		hooks[ev] = kept
	}
	settings["hooks"] = hooks
	return settings
}

// RemoveHooks strips fleet's hooks from settings.
func RemoveHooks(settings map[string]any) map[string]any {
	hooks, _ := settings["hooks"].(map[string]any)
	for ev, v := range hooks {
		cur, _ := v.([]any)
		var kept []any
		for _, e := range cur {
			if !entryIsFleet(e) {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = kept
		}
	}
	return settings
}

func entryIsFleet(e any) bool {
	m, _ := e.(map[string]any)
	hs, _ := m["hooks"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		if c, _ := hm["command"].(string); isFleetHook(c) {
			return true
		}
	}
	return false
}

type Plan struct {
	SettingsPath string
	Settings     []byte
	SkillPath    string
	MCPCommand   []string // claude mcp add …
	ProjectMD    string   // CLAUDE.md to append to (project scope)
}

const mdBegin = "<!-- fleet:begin -->"
const mdEnd = "<!-- fleet:end -->"

const projectBlurb = mdBegin + `
## Multi-agent coordination (fleet)

Other agents may be working on this machine. Follow the ` + "`fleet`" + ` skill: check
` + "`fleet status`" + ` before killing processes or taking ports, wrap migrations and deploys as
` + "`fleet run --critical -- <cmd>`" + `, prefer documented commands (` + "`fleet docs search`" + `), and
record what works or fails with ` + "`fleet docs add`" + `.
` + mdEnd + "\n"

// PlanClaude computes what `fleet install claude` will change.
func PlanClaude(scope, repoRoot, wake string) (*Plan, error) {
	if wake != WakeBlock {
		wake = WakeAsync
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	bin := BinaryPath()
	p := &Plan{SkillPath: filepath.Join(home, ".claude", "skills", "fleet", "SKILL.md")}
	switch scope {
	case "user":
		p.SettingsPath = filepath.Join(home, ".claude", "settings.json")
		p.MCPCommand = []string{"claude", "mcp", "add", "--scope", "user", "fleet", "--", bin, "mcp"}
	case "project":
		if repoRoot == "" {
			return nil, errors.New("--scope project must be run inside a git repository")
		}
		p.SettingsPath = filepath.Join(repoRoot, ".claude", "settings.json")
		p.MCPCommand = []string{"claude", "mcp", "add", "--scope", "project", "fleet", "--", bin, "mcp"}
		p.ProjectMD = filepath.Join(repoRoot, "CLAUDE.md")
	default:
		return nil, fmt.Errorf("scope must be user or project")
	}
	settings, err := readJSON(p.SettingsPath)
	if err != nil {
		return nil, err
	}
	p.Settings, _ = json.MarshalIndent(MergeHooks(settings, bin, wake), "", "  ")
	return p, nil
}

// Apply writes the plan. Settings are backed up first.
func (p *Plan) Apply(w io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(p.SettingsPath), 0o755); err != nil {
		return err
	}
	if old, err := os.ReadFile(p.SettingsPath); err == nil {
		bak := p.SettingsPath + ".fleet-backup-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(bak, old, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(w, "backed up %s -> %s\n", p.SettingsPath, bak)
	}
	if err := os.WriteFile(p.SettingsPath, append(p.Settings, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(w, "hooks written to %s\n", p.SettingsPath)

	if err := os.MkdirAll(filepath.Dir(p.SkillPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p.SkillPath, []byte(SkillMD), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(w, "skill written to %s\n", p.SkillPath)

	if p.ProjectMD != "" {
		if err := upsertBlock(p.ProjectMD); err != nil {
			return err
		}
		fmt.Fprintf(w, "coordination section added to %s\n", p.ProjectMD)
	}

	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintf(w, "`claude` not found on PATH. Register the MCP server yourself:\n  %s\n", strings.Join(p.MCPCommand, " "))
		return nil
	}
	// Re-adding fails if it already exists; remove first (ignore errors).
	_ = exec.Command("claude", "mcp", "remove", "--scope", p.MCPCommand[4], "fleet").Run()
	out, err := exec.Command(p.MCPCommand[0], p.MCPCommand[1:]...).CombinedOutput()
	if err != nil {
		fmt.Fprintf(w, "could not register MCP server (%v): %s\nRun it yourself:\n  %s\n", err, strings.TrimSpace(string(out)), strings.Join(p.MCPCommand, " "))
		return nil
	}
	fmt.Fprintf(w, "MCP server registered: %s\n", strings.Join(p.MCPCommand, " "))
	return nil
}

// Uninstall removes fleet hooks (and the skill for user scope).
func Uninstall(scope, repoRoot string, w io.Writer) error {
	p, err := PlanClaude(scope, repoRoot, WakeAsync)
	if err != nil {
		return err
	}
	settings, err := readJSON(p.SettingsPath)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(RemoveHooks(settings), "", "  ")
	if err := os.WriteFile(p.SettingsPath, append(b, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(w, "fleet hooks removed from %s\n", p.SettingsPath)
	if scope == "user" {
		_ = os.RemoveAll(filepath.Dir(p.SkillPath))
		fmt.Fprintf(w, "skill removed\n")
	}
	if _, err := exec.LookPath("claude"); err == nil {
		_ = exec.Command("claude", "mcp", "remove", "--scope", scope, "fleet").Run()
		fmt.Fprintf(w, "MCP server removed\n")
	}
	return nil
}

func readJSON(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if len(strings.TrimSpace(string(b))) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%v); fix it first", path, err)
	}
	return m, nil
}

func upsertBlock(path string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s := string(b)
	if i := strings.Index(s, mdBegin); i >= 0 {
		if j := strings.Index(s, mdEnd); j > i {
			s = s[:i] + strings.TrimSuffix(projectBlurb, "\n") + s[j+len(mdEnd):]
			return os.WriteFile(path, []byte(s), 0o644)
		}
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if s != "" {
		s += "\n"
	}
	return os.WriteFile(path, []byte(s+projectBlurb), 0o644)
}
