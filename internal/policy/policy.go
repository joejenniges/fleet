// Package policy decides what fleet may do to a process: never touch it, ask its
// agent to stop it, or kill it.
//
// Semantics (see DESIGN.md §6):
//   - Rules: the most restrictive matching rule across ALL levels wins
//     (never > ask > kill). Order never matters.
//   - Defaults (unknown / grace / on_timeout): the most specific level that sets a
//     value wins (repo > user > global > builtin).
//   - Protection flows through lineage: a process cannot be killed if any ancestor
//     (within the agent's tree) or descendant resolves to never or is critical-marked.
package policy

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/joejenniges/fleet/internal/dur"
)

type Action string

const (
	Never Action = "never"
	Ask   Action = "ask"
	Kill  Action = "kill"
)

func (a Action) Rank() int {
	switch a {
	case Never:
		return 3
	case Ask:
		return 2
	case Kill:
		return 1
	}
	return 0
}

func (a Action) Valid() bool { return a.Rank() > 0 }

// OnTimeout values: what happens when an `ask` deadline passes.
const (
	TimeoutNotify = "notify"
	TimeoutKill   = "kill"
)

func timeoutRank(s string) int {
	switch s {
	case TimeoutNotify:
		return 2
	case TimeoutKill:
		return 1
	}
	return 0
}

// Level names, least to most specific.
const (
	LevelBuiltin = "builtin"
	LevelGlobal  = "global"
	LevelUser    = "user"
	LevelRepo    = "repo"
	LevelRuntime = "runtime"
)

var EditableLevels = []string{LevelGlobal, LevelUser, LevelRepo}

type Rule struct {
	ID           string   `toml:"id" json:"id"`
	Name         []string `toml:"name,omitempty" json:"name,omitempty"`
	ArgsContains string   `toml:"args_contains,omitempty" json:"args_contains,omitempty"`
	CmdlineRegex string   `toml:"cmdline_regex,omitempty" json:"cmdline_regex,omitempty"`
	Action       Action   `toml:"action" json:"action"`
	Grace        string   `toml:"grace,omitempty" json:"grace,omitempty"`
	OnTimeout    string   `toml:"on_timeout,omitempty" json:"on_timeout,omitempty"`
	Reason       string   `toml:"reason,omitempty" json:"reason,omitempty"`
	AddedBy      string   `toml:"added_by,omitempty" json:"added_by,omitempty"`
	AddedAt      string   `toml:"added_at,omitempty" json:"added_at,omitempty"`

	re *regexp.Regexp
}

type Defaults struct {
	Unknown   Action `toml:"unknown,omitempty" json:"unknown,omitempty"`
	Grace     string `toml:"grace,omitempty" json:"grace,omitempty"`
	OnTimeout string `toml:"on_timeout,omitempty" json:"on_timeout,omitempty"`
}

// File is the on-disk policy.toml shape.
type File struct {
	Defaults Defaults `toml:"defaults" json:"defaults"`
	Rules    []Rule   `toml:"rule" json:"rules"`
}

// Validate checks the file and compiles regexes.
func (f *File) Validate() error {
	if f.Defaults.Unknown != "" && !f.Defaults.Unknown.Valid() {
		return fmt.Errorf("defaults.unknown: invalid action %q", f.Defaults.Unknown)
	}
	if _, err := dur.Parse(f.Defaults.Grace); err != nil {
		return fmt.Errorf("defaults.grace: %w", err)
	}
	if f.Defaults.OnTimeout != "" && timeoutRank(f.Defaults.OnTimeout) == 0 {
		return fmt.Errorf("defaults.on_timeout: must be notify or kill")
	}
	seen := map[string]bool{}
	for i := range f.Rules {
		r := &f.Rules[i]
		if err := r.compile(); err != nil {
			return fmt.Errorf("rule %q: %w", r.ID, err)
		}
		if seen[r.ID] {
			return fmt.Errorf("duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
	}
	return nil
}

func (r *Rule) compile() error {
	if r.ID == "" {
		return fmt.Errorf("missing id")
	}
	if !r.Action.Valid() {
		return fmt.Errorf("invalid action %q (never|ask|kill)", r.Action)
	}
	if len(r.Name) == 0 && r.ArgsContains == "" && r.CmdlineRegex == "" {
		return fmt.Errorf("needs at least one of name, args_contains, cmdline_regex")
	}
	if _, err := dur.Parse(r.Grace); err != nil {
		return err
	}
	if r.OnTimeout != "" && timeoutRank(r.OnTimeout) == 0 {
		return fmt.Errorf("on_timeout must be notify or kill")
	}
	if r.CmdlineRegex != "" {
		re, err := regexp.Compile(r.CmdlineRegex)
		if err != nil {
			return fmt.Errorf("cmdline_regex: %w", err)
		}
		r.re = re
	}
	for i, n := range r.Name {
		r.Name[i] = NormName(n)
	}
	return nil
}

// Matches reports whether the rule applies to a process. All present fields must match.
func (r *Rule) Matches(in Input) bool {
	if len(r.Name) > 0 {
		n := NormName(in.Argv0)
		ok := false
		for _, want := range r.Name {
			if want == n {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if r.ArgsContains != "" && !strings.Contains(in.Cmdline, r.ArgsContains) {
		return false
	}
	if r.re != nil && !r.re.MatchString(in.Cmdline) {
		return false
	}
	return true
}

// NormName lower-cases, takes the basename and strips .exe so one rule works everywhere.
func NormName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ToLower(s)
	return strings.TrimSuffix(s, ".exe")
}

// Builtin is compiled into the binary. Its rules can never be removed, and because
// the most restrictive rule wins they act as a safety floor.
func Builtin() File {
	return File{
		Defaults: Defaults{Unknown: Ask, Grace: "10s", OnTimeout: TimeoutNotify},
		Rules: []Rule{
			{
				ID:     "builtin-db-cli",
				Name:   []string{"sqlcmd", "psql", "mysql", "mariadb", "mongosh", "sqlite3", "pg_dump", "pg_restore", "mysqldump", "flyway", "liquibase", "bcp"},
				Action: Never,
				Reason: "database clients may be mid-write or mid-transaction",
			},
			{
				ID:           "builtin-migrations",
				CmdlineRegex: `(?i)(\bmigrat(e|ion|ions)\b|db:(push|seed|migrate|reset)|database update|alembic upgrade|prisma (migrate|db push))`,
				Action:       Never,
				Reason:       "schema migrations must not be interrupted",
			},
		},
	}
}

func parseGrace(s string, def time.Duration) time.Duration { return dur.MustParse(s, def) }
