// Package testkit implements `fleet test …`: isolated test homes, fixture processes,
// a deterministic hook-level selfcheck, and live Claude Code scenarios.
//
// Isolation is by environment: every fleet process launched for a test gets
// FLEET_HOME=<test home>, which redirects the database, user config and global
// policy. Nothing global is switched, so real agents running at the same time keep
// using the real database.
package testkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/joejenniges/fleet/internal/install"
)

type Home struct {
	Dir       string `json:"dir"`        // root of everything below
	FleetHome string `json:"fleet_home"` // FLEET_HOME for test processes
	Work      string `json:"work"`       // scenario working directories live here
	ClaudeDir string `json:"claude_dir"` // settings.json + mcp.json for test sessions
	Logs      string `json:"logs"`
	Bin       string `json:"bin"` // fleet binary used by hooks/MCP in test sessions
}

func (h *Home) SettingsPath() string { return filepath.Join(h.ClaudeDir, "settings.json") }
func (h *Home) MCPPath() string      { return filepath.Join(h.ClaudeDir, "mcp.json") }

// Env returns os.Environ() with FLEET_HOME pointed at the test home.
func (h *Home) Env() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "FLEET_HOME=") && !strings.HasPrefix(e, "FLEET_AGENT_ID=") &&
			!strings.HasPrefix(e, "FLEET_GLOBAL_DIR=") {
			env = append(env, e)
		}
	}
	return append(env, "FLEET_HOME="+h.FleetHome)
}

func pointerPath() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "fleet", "test-home.json"), nil
}

// Current returns the stood-up test home, or nil.
func Current() (*Home, error) {
	p, err := pointerPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h Home
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	if _, err := os.Stat(h.Dir); err != nil {
		return nil, nil
	}
	return &h, nil
}

// Standup creates an isolated test home. dir "" = a new temp directory.
func Standup(dir string) (*Home, error) {
	if cur, _ := Current(); cur != nil {
		return nil, fmt.Errorf("a test home is already stood up at %s; run `fleet test teardown` first", cur.Dir)
	}
	var err error
	if dir == "" {
		dir, err = os.MkdirTemp("", "fleet-test-")
		if err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dir, _ = filepath.Abs(dir)
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	bin, _ = filepath.EvalSymlinks(bin)
	h := &Home{Dir: dir, FleetHome: filepath.Join(dir, "fleet-home"), Work: filepath.Join(dir, "work"),
		ClaudeDir: filepath.Join(dir, "claude"), Logs: filepath.Join(dir, "logs"), Bin: bin}
	for _, d := range []string{h.FleetHome, h.Work, h.ClaudeDir, h.Logs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := h.writeClaudeConfig(); err != nil {
		return nil, err
	}
	if err := h.writeEnvFiles(); err != nil {
		return nil, err
	}
	p, err := pointerPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(h, "", "  ")
	return h, os.WriteFile(p, b, 0o644)
}

// writeClaudeConfig writes the settings (hooks) and MCP config that test sessions
// load with --settings / --mcp-config. If fleet hooks are already installed in the
// user's own Claude settings, they would run as well (and, inheriting FLEET_HOME,
// hit the test DB), so the test settings leave hooks out to avoid double-firing.
func (h *Home) writeClaudeConfig() error {
	settings := map[string]any{}
	if !UserHasFleetHooks() {
		settings = install.MergeHooks(settings, h.Bin, install.WakeAsync)
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(h.SettingsPath(), b, 0o644); err != nil {
		return err
	}
	mcp := map[string]any{"mcpServers": map[string]any{
		"fleet": map[string]any{"command": h.Bin, "args": []string{"mcp"}, "env": map[string]string{"FLEET_HOME": h.FleetHome}},
	}}
	b, _ = json.MarshalIndent(mcp, "", "  ")
	return os.WriteFile(h.MCPPath(), b, 0o644)
}

// UserHasFleetHooks reports whether ~/.claude/settings.json already runs fleet hooks.
func UserHasFleetHooks() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), " hook session-start --agent claude")
}

func (h *Home) writeEnvFiles() error {
	sh := fmt.Sprintf("# source this to point fleet commands at the test home\nexport FLEET_HOME=%q\n", h.FleetHome)
	ps := fmt.Sprintf("# dot-source this in PowerShell\n$env:FLEET_HOME = %q\n", h.FleetHome)
	if err := os.WriteFile(filepath.Join(h.Dir, "env.sh"), []byte(sh), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.Dir, "env.ps1"), []byte(ps), 0o644)
}

// NewWorkdir creates a fresh repository directory for a scenario.
func (h *Home) NewWorkdir(name string) (string, error) {
	d := filepath.Join(h.Work, name)
	_ = os.RemoveAll(d)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	if git, err := exec.LookPath("git"); err == nil {
		if exec.Command(git, "-C", d, "init", "-q").Run() == nil {
			return d, nil
		}
	}
	return d, os.MkdirAll(filepath.Join(d, ".git"), 0o755) // enough for fleet's repo detection
}

// Teardown kills fixture processes, deletes the test home and clears the pointer.
func Teardown() (*Home, int, error) {
	h, err := Current()
	if err != nil {
		return nil, 0, err
	}
	killed := KillFixtures("")
	if p, err := pointerPath(); err == nil {
		_ = os.Remove(p)
	}
	if h == nil {
		return nil, killed, nil
	}
	if runtime.GOOS == "windows" {
		// The WAL/SHM files can stay locked briefly after processes exit.
		for i := 0; i < 5; i++ {
			if os.RemoveAll(h.Dir) == nil {
				break
			}
			sleepMS(300)
		}
	}
	return h, killed, os.RemoveAll(h.Dir)
}
