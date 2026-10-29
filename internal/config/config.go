// Package config loads <user config>/config.toml (non-policy settings).
package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/joejenniges/fleet/internal/dur"
	"github.com/joejenniges/fleet/internal/paths"
)

type Config struct {
	Runs    Runs    `toml:"runs"`
	Docs    Docs    `toml:"docs"`
	Hooks   Hooks   `toml:"hooks"`
	Redact  Redact  `toml:"redact"`
	Signals Signals `toml:"signals"`
}

type Signals struct {
	MaxWait      dur.Duration `toml:"max_wait"`       // waits expire after this; also caps the background waiter
	BlockMax     dur.Duration `toml:"block_max"`      // longest a blocking Stop hook (--wake block) holds a turn
	PollInterval dur.Duration `toml:"poll_interval"`  // how often waiters check the database
	MaxDataBytes int          `toml:"max_data_bytes"` // cap on signal data
}

type Runs struct {
	MaxAge           dur.Duration `toml:"max_age"`            // 0 = keep forever
	MaxOutputBytes   int          `toml:"max_output_bytes"`   // cap at record time (keeps the tail)
	CompactAfter     dur.Duration `toml:"compact_after"`      // older runs keep only the last N lines
	CompactKeepLines int          `toml:"compact_keep_lines"` // N
}

type Docs struct {
	TopN           int          `toml:"top_n"`
	TopWindow      dur.Duration `toml:"top_window"`       // execution-count window
	KnownMarkTTL   dur.Duration `toml:"known_mark_ttl"`   // "already in CLAUDE.md" marks expire
	KnownBadAction string       `toml:"known_bad_action"` // block | off
}

type Hooks struct {
	Timeout          dur.Duration `toml:"timeout"`
	SessionStartTime dur.Duration `toml:"session_start_timeout"`
	ScanProcesses    bool         `toml:"scan_processes"`
	ScanMinInterval  dur.Duration `toml:"scan_min_interval"`
}

type Redact struct {
	Patterns []string `toml:"patterns"` // extra regexes; whole match is replaced
}

func Defaults() Config {
	return Config{
		Runs: Runs{
			MaxOutputBytes:   256 * 1024,
			CompactAfter:     dur.Duration{Duration: 14 * 24 * time.Hour},
			CompactKeepLines: 40,
		},
		Docs: Docs{
			TopN:           10,
			TopWindow:      dur.Duration{Duration: 90 * 24 * time.Hour},
			KnownMarkTTL:   dur.Duration{Duration: 14 * 24 * time.Hour},
			KnownBadAction: "block",
		},
		Hooks: Hooks{
			Timeout:          dur.Duration{Duration: 2 * time.Second},
			SessionStartTime: dur.Duration{Duration: 5 * time.Second},
			ScanProcesses:    true,
			ScanMinInterval:  dur.Duration{Duration: 3 * time.Second},
		},
		Signals: Signals{
			MaxWait:      dur.Duration{Duration: 24 * time.Hour},
			BlockMax:     dur.Duration{Duration: 9 * time.Minute},
			PollInterval: dur.Duration{Duration: 500 * time.Millisecond},
			MaxDataBytes: 16 * 1024,
		},
	}
}

// Path returns <user config>/config.toml.
func Path() (string, error) {
	d, err := paths.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.toml"), nil
}

// Load reads config.toml over the defaults. A missing file is not an error.
func Load() (Config, error) {
	c := Defaults()
	p, err := Path()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := toml.Unmarshal(b, &c); err != nil {
		return Defaults(), err
	}
	return c, nil
}
