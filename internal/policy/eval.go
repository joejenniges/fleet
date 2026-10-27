package policy

import (
	"fmt"
	"time"
)

// Input describes one process (or a command line being tested).
type Input struct {
	PID            int32  `json:"pid,omitempty"`
	Argv0          string `json:"argv0"`
	Cmdline        string `json:"cmdline"`
	Critical       bool   `json:"critical,omitempty"`
	CriticalReason string `json:"critical_reason,omitempty"`
}

type RuleMatch struct {
	Level     string `json:"level"`
	ID        string `json:"id"`
	Action    Action `json:"action"`
	Grace     string `json:"grace,omitempty"`
	OnTimeout string `json:"on_timeout,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type LevelResult struct {
	Level     string      `json:"level"`
	File      string      `json:"file"`
	Exists    bool        `json:"exists"`
	Error     string      `json:"error,omitempty"`
	FromCache bool        `json:"from_cache,omitempty"`
	Defaults  Defaults    `json:"defaults"`
	Matched   []RuleMatch `json:"matched"`
	Verdict   Action      `json:"verdict,omitempty"` // most restrictive match at this level
}

type RuntimeResult struct {
	Level    string `json:"level"`
	Critical bool   `json:"critical"`
	Reason   string `json:"reason,omitempty"`
}

type LineageHit struct {
	Relation string `json:"relation"` // ancestor | descendant
	PID      int32  `json:"pid"`
	Cmdline  string `json:"cmdline"`
	Action   Action `json:"action"`
	By       string `json:"by"`
}

type Decision struct {
	Action    Action        `json:"action"`
	Grace     string        `json:"grace"`
	GraceDur  time.Duration `json:"-"`
	OnTimeout string        `json:"on_timeout"`
	DecidedBy []string      `json:"decided_by"`
	Unmatched bool          `json:"unmatched,omitempty"`
	// Lineage lists relatives that raised the decision (protection inheritance).
	Lineage []LineageHit `json:"lineage,omitempty"`
}

type Result struct {
	Input   Input         `json:"input"`
	Levels  []LevelResult `json:"levels"`
	Runtime RuntimeResult `json:"runtime"`
	Final   Decision      `json:"final"`
}

// EffectiveDefaults resolves defaults: the most specific level that sets a field wins.
// The second return maps field -> level that supplied it.
func (s *Set) EffectiveDefaults() (Defaults, map[string]string) {
	var d Defaults
	from := map[string]string{}
	for _, lv := range s.Levels { // least -> most specific; later overrides
		f := lv.File.Defaults
		if f.Unknown != "" {
			d.Unknown, from["unknown"] = f.Unknown, lv.Name
		}
		if f.Grace != "" {
			d.Grace, from["grace"] = f.Grace, lv.Name
		}
		if f.OnTimeout != "" {
			d.OnTimeout, from["on_timeout"] = f.OnTimeout, lv.Name
		}
	}
	return d, from
}

// Evaluate decides for a single process, without lineage.
func (s *Set) Evaluate(in Input) Result {
	res := Result{Input: in, Runtime: RuntimeResult{Level: LevelRuntime, Critical: in.Critical, Reason: in.CriticalReason}}
	eff, from := s.EffectiveDefaults()
	defGrace := parseGrace(eff.Grace, 10*time.Second)

	var all []RuleMatch
	for _, lv := range s.Levels {
		lr := LevelResult{Level: lv.Name, File: lv.Path, Exists: lv.Exists, Error: lv.Error,
			FromCache: lv.FromCache, Defaults: lv.File.Defaults, Matched: []RuleMatch{}}
		for i := range lv.File.Rules {
			r := &lv.File.Rules[i]
			if !r.Matches(in) {
				continue
			}
			m := RuleMatch{Level: lv.Name, ID: r.ID, Action: r.Action, Grace: r.Grace, OnTimeout: r.OnTimeout, Reason: r.Reason}
			lr.Matched = append(lr.Matched, m)
			if r.Action.Rank() > lr.Verdict.Rank() {
				lr.Verdict = r.Action
			}
			all = append(all, m)
		}
		res.Levels = append(res.Levels, lr)
	}

	d := Decision{}
	if len(all) == 0 {
		d.Unmatched = true
		d.Action = eff.Unknown
		d.GraceDur = defGrace
		d.OnTimeout = eff.OnTimeout
		d.DecidedBy = []string{from["unknown"] + ":defaults.unknown"}
	} else {
		for _, m := range all {
			if m.Action.Rank() > d.Action.Rank() {
				d.Action = m.Action
			}
		}
		d.OnTimeout = ""
		for _, m := range all {
			if m.Action != d.Action {
				continue
			}
			d.DecidedBy = append(d.DecidedBy, m.Level+":"+m.ID)
			g := parseGrace(m.Grace, defGrace)
			if g > d.GraceDur {
				d.GraceDur = g
			}
			ot := m.OnTimeout
			if ot == "" {
				ot = eff.OnTimeout
			}
			if timeoutRank(ot) > timeoutRank(d.OnTimeout) {
				d.OnTimeout = ot
			}
		}
	}
	if in.Critical {
		d.Action = Never
		d.DecidedBy = append(d.DecidedBy, "runtime:critical")
	}
	if d.OnTimeout == "" {
		d.OnTimeout = TimeoutNotify
	}
	d.Grace = d.GraceDur.String()
	res.Final = d
	return res
}

// EvaluateLineage decides for target, considering its relatives:
//   - descendants die with the target, so the most restrictive action among the
//     target and all descendants applies;
//   - an ancestor (within the agent's tree) that resolves to never protects the target,
//     because killing part of a protected job leaves it half-done.
func (s *Set) EvaluateLineage(target Input, ancestors, descendants []Input) Result {
	res := s.Evaluate(target)
	raise := func(rel string, in Input, onlyNever bool) {
		r := s.Evaluate(in)
		a := r.Final.Action
		if onlyNever && a != Never {
			return
		}
		if a.Rank() <= res.Final.Action.Rank() {
			return
		}
		res.Final.Action = a
		by := "?"
		if len(r.Final.DecidedBy) > 0 {
			by = r.Final.DecidedBy[0]
		}
		res.Final.Lineage = append(res.Final.Lineage, LineageHit{Relation: rel, PID: in.PID, Cmdline: in.Cmdline, Action: a, By: by})
		res.Final.DecidedBy = append(res.Final.DecidedBy, fmt.Sprintf("%s:%d:%s", rel, in.PID, by))
	}
	for _, d := range descendants {
		raise("descendant", d, false)
	}
	for _, a := range ancestors {
		raise("ancestor", a, true)
	}
	return res
}
