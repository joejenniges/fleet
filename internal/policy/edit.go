package policy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	OpAddRule    = "add-rule"
	OpRemoveRule = "remove-rule"
	OpSetDefault = "set-default"

	ClassRestrictive = "restrictive"
	ClassPermissive  = "permissive"
)

// Change is one requested policy edit. It is stored as JSON on a proposal.
type Change struct {
	Op       string    `json:"op"`
	Level    string    `json:"level"`
	RepoRoot string    `json:"repo_root,omitempty"`
	Rule     *Rule     `json:"rule,omitempty"`
	RuleID   string    `json:"rule_id,omitempty"`
	Defaults *Defaults `json:"defaults,omitempty"` // non-empty fields are set
	Reason   string    `json:"reason"`
}

func (c Change) JSON() string { b, _ := json.Marshal(c); return string(b) }

func ParseChange(s string) (Change, error) {
	var c Change
	err := json.Unmarshal([]byte(s), &c)
	return c, err
}

// Summary is a one-line human description.
func (c Change) Summary() string {
	switch c.Op {
	case OpAddRule:
		r := c.Rule
		var m []string
		if len(r.Name) > 0 {
			m = append(m, "name="+strings.Join(r.Name, ","))
		}
		if r.ArgsContains != "" {
			m = append(m, fmt.Sprintf("args_contains=%q", r.ArgsContains))
		}
		if r.CmdlineRegex != "" {
			m = append(m, fmt.Sprintf("cmdline_regex=%q", r.CmdlineRegex))
		}
		s := fmt.Sprintf("[%s] add rule %s: %s → %s", c.Level, r.ID, strings.Join(m, " "), r.Action)
		if r.Grace != "" {
			s += " (grace " + r.Grace + ")"
		}
		return s
	case OpRemoveRule:
		return fmt.Sprintf("[%s] remove rule %s", c.Level, c.RuleID)
	case OpSetDefault:
		var parts []string
		if c.Defaults.Unknown != "" {
			parts = append(parts, "unknown="+string(c.Defaults.Unknown))
		}
		if c.Defaults.Grace != "" {
			parts = append(parts, "grace="+c.Defaults.Grace)
		}
		if c.Defaults.OnTimeout != "" {
			parts = append(parts, "on_timeout="+c.Defaults.OnTimeout)
		}
		return fmt.Sprintf("[%s] set defaults %s", c.Level, strings.Join(parts, " "))
	}
	return c.Op
}

func (c Change) validateShape() error {
	if !slices.Contains(EditableLevels, c.Level) {
		return fmt.Errorf("level must be one of %v", EditableLevels)
	}
	switch c.Op {
	case OpAddRule:
		if c.Rule == nil {
			return fmt.Errorf("add-rule needs a rule")
		}
		r := *c.Rule
		r.Name = slices.Clone(r.Name)
		return r.compile()
	case OpRemoveRule:
		if c.RuleID == "" {
			return fmt.Errorf("remove-rule needs a rule id")
		}
		if strings.HasPrefix(c.RuleID, "builtin-") {
			return fmt.Errorf("builtin rules cannot be removed")
		}
	case OpSetDefault:
		if c.Defaults == nil || (*c.Defaults == Defaults{}) {
			return fmt.Errorf("set-default needs at least one field")
		}
		f := File{Defaults: *c.Defaults}
		return f.Validate()
	default:
		return fmt.Errorf("unknown op %q", c.Op)
	}
	return nil
}

// Apply returns f with the change applied.
func Apply(f File, c Change) (File, error) {
	if err := c.validateShape(); err != nil {
		return f, err
	}
	out := File{Defaults: f.Defaults, Rules: slices.Clone(f.Rules)}
	switch c.Op {
	case OpAddRule:
		for _, r := range out.Rules {
			if r.ID == c.Rule.ID {
				return f, fmt.Errorf("rule %q already exists at %s level; remove it first", r.ID, c.Level)
			}
		}
		r := *c.Rule
		if r.AddedAt == "" {
			r.AddedAt = time.Now().UTC().Format(time.RFC3339)
		}
		out.Rules = append(out.Rules, r)
	case OpRemoveRule:
		i := slices.IndexFunc(out.Rules, func(r Rule) bool { return r.ID == c.RuleID })
		if i < 0 {
			return f, fmt.Errorf("rule %q not found at %s level", c.RuleID, c.Level)
		}
		out.Rules = slices.Delete(out.Rules, i, i+1)
	case OpSetDefault:
		if c.Defaults.Unknown != "" {
			out.Defaults.Unknown = c.Defaults.Unknown
		}
		if c.Defaults.Grace != "" {
			out.Defaults.Grace = c.Defaults.Grace
		}
		if c.Defaults.OnTimeout != "" {
			out.Defaults.OnTimeout = c.Defaults.OnTimeout
		}
	}
	return out, out.Validate()
}

// Classify reports whether a change can only make outcomes more conservative
// (restrictive) or might make them less so (permissive). current is the level's file.
//
// Because the most restrictive rule always wins:
//   - adding a never/ask rule, or removing a kill rule, is restrictive;
//   - adding a kill rule, or removing a never/ask rule, is permissive.
//
// Defaults resolve by specificity, so a defaults change is classified by comparing
// effective defaults before and after.
func Classify(set *Set, current File, c Change) (string, error) {
	if err := c.validateShape(); err != nil {
		return "", err
	}
	switch c.Op {
	case OpAddRule:
		if c.Rule.Action == Kill {
			return ClassPermissive, nil
		}
		return ClassRestrictive, nil
	case OpRemoveRule:
		i := slices.IndexFunc(current.Rules, func(r Rule) bool { return r.ID == c.RuleID })
		if i < 0 {
			return "", fmt.Errorf("rule %q not found at %s level", c.RuleID, c.Level)
		}
		if current.Rules[i].Action == Kill {
			return ClassRestrictive, nil
		}
		return ClassPermissive, nil
	case OpSetDefault:
		before, _ := set.EffectiveDefaults()
		after := simulateDefaults(set, c)
		bg := parseGrace(before.Grace, 0)
		ag := parseGrace(after.Grace, 0)
		if after.Unknown.Rank() >= before.Unknown.Rank() &&
			ag >= bg &&
			timeoutRank(after.OnTimeout) >= timeoutRank(before.OnTimeout) {
			return ClassRestrictive, nil
		}
		return ClassPermissive, nil
	}
	return "", fmt.Errorf("unknown op %q", c.Op)
}

func simulateDefaults(set *Set, c Change) Defaults {
	sim := &Set{RepoRoot: set.RepoRoot}
	found := false
	for _, lv := range set.Levels {
		cp := *lv
		if lv.Name == c.Level {
			found = true
			cp.File.Defaults = mergeDefaults(lv.File.Defaults, *c.Defaults)
		}
		sim.Levels = append(sim.Levels, &cp)
	}
	if !found {
		sim.Levels = append(sim.Levels, &Level{Name: c.Level, File: File{Defaults: *c.Defaults}})
		order := map[string]int{LevelBuiltin: 0, LevelGlobal: 1, LevelUser: 2, LevelRepo: 3}
		slices.SortStableFunc(sim.Levels, func(a, b *Level) int { return order[a.Name] - order[b.Name] })
	}
	d, _ := sim.EffectiveDefaults()
	return d
}

func mergeDefaults(base, patch Defaults) Defaults {
	if patch.Unknown != "" {
		base.Unknown = patch.Unknown
	}
	if patch.Grace != "" {
		base.Grace = patch.Grace
	}
	if patch.OnTimeout != "" {
		base.OnTimeout = patch.OnTimeout
	}
	return base
}

// NeedsApproval: permissive changes always do; anything at the global level does too.
func NeedsApproval(class, level string) bool {
	return class == ClassPermissive || level == LevelGlobal
}

// LevelFile returns the loaded file for a level in the set (empty if absent).
func (s *Set) LevelFile(name string) File {
	for _, lv := range s.Levels {
		if lv.Name == name {
			return lv.File
		}
	}
	return File{}
}
