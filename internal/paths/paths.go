// Package paths resolves fleet's per-OS state, config and repo locations.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// Home returns FLEET_HOME if set (tests, isolated setups).
func Home() string { return os.Getenv("FLEET_HOME") }

// StateDir holds fleet.db.
func StateDir() (string, error) {
	if h := Home(); h != "" {
		return filepath.Join(h, "state"), nil
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "fleet"), nil
		}
	case "darwin":
		d, err := os.UserConfigDir() // ~/Library/Application Support
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "fleet"), nil
	default:
		if d := os.Getenv("XDG_STATE_HOME"); d != "" {
			return filepath.Join(d, "fleet"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "fleet"), nil
}

// UserConfigDir holds config.toml and the user-level policy.toml.
func UserConfigDir() (string, error) {
	if h := Home(); h != "" {
		return filepath.Join(h, "config"), nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "fleet"), nil
}

// GlobalConfigDir holds the machine-wide policy.toml. FLEET_GLOBAL_DIR overrides.
func GlobalConfigDir() string {
	if d := os.Getenv("FLEET_GLOBAL_DIR"); d != "" {
		return d
	}
	if h := Home(); h != "" {
		return filepath.Join(h, "global")
	}
	switch runtime.GOOS {
	case "windows":
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "fleet")
	case "darwin":
		return "/Library/Application Support/fleet"
	default:
		return "/etc/fleet"
	}
}

// DBPath is the SQLite database location.
func DBPath() (string, error) {
	d, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "fleet.db"), nil
}

// RepoRoot walks up from start looking for a .git directory or file (worktrees).
// Returns "" when start is not inside a repository.
func RepoRoot(start string) string {
	if start == "" {
		return ""
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return Canon(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// RepoPolicyPath returns <repo>/.fleet/policy.toml.
func RepoPolicyPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".fleet", "policy.toml")
}

// Canon normalizes a path for comparison: absolute, cleaned, and lower-cased on
// case-insensitive platforms.
func Canon(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return lower(p)
	}
	return p
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
