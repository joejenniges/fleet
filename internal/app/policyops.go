package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/joejenniges/fleet/internal/policy"
	"github.com/joejenniges/fleet/internal/procs"
	"github.com/joejenniges/fleet/internal/store"
)

type ProposeResult struct {
	ProposalID int64  `json:"proposal_id"`
	Summary    string `json:"summary"`
	Class      string `json:"class"`
	Status     string `json:"status"` // applied | approved | pending
	Applied    bool   `json:"applied"`
	File       string `json:"file"`
	Message    string `json:"message"`
}

// ProposePolicy validates and classifies a change. Restrictive changes (outside the
// global level) apply immediately; permissive ones wait for a human. When human is
// true (interactive terminal, no agent ancestor) the change applies directly.
func (a *App) ProposePolicy(c policy.Change, human bool) (*ProposeResult, error) {
	if c.Level == policy.LevelRepo {
		c.RepoRoot = a.Repo
	}
	if c.Reason == "" {
		return nil, errors.New("a --reason is required so reviewers know why")
	}
	path, err := policy.LevelPath(c.Level, a.Repo)
	if err != nil {
		return nil, err
	}
	cur, err := policy.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s is invalid (%v); a human must fix it before it can be changed", path, err)
	}
	if c.Rule != nil {
		c.Rule.AddedBy = a.Who()
		c.Rule.AddedAt = time.Now().UTC().Format(time.RFC3339)
	}
	class, err := policy.Classify(a.Policy(), cur, c)
	if err != nil {
		return nil, err
	}
	// Dry-apply now so invalid changes are rejected up front, not at approval time.
	next, err := policy.Apply(cur, c)
	if err != nil {
		return nil, err
	}
	res := &ProposeResult{Summary: c.Summary(), Class: class, File: path}
	prop := &store.Proposal{Level: c.Level, RepoRoot: c.RepoRoot, Change: c.JSON(), Class: class, ProposedBy: a.AgentID()}

	if human || !policy.NeedsApproval(class, c.Level) {
		if err := policy.WriteFile(path, next); err != nil {
			return nil, err
		}
		a.invalidatePolicy()
		prop.Status, prop.DecidedAt = store.PropApplied, time.Now()
		if human && policy.NeedsApproval(class, c.Level) {
			prop.Status, prop.DecidedNote = store.PropApproved, "applied directly by a human"
		}
		if _, err := a.St.AddProposal(prop); err != nil {
			return nil, err
		}
		res.ProposalID, res.Status, res.Applied = prop.ID, prop.Status, true
		res.Message = "applied"
		a.St.LogEvent(a.AgentID(), "policy.applied", map[string]any{"proposal": prop.ID, "summary": res.Summary})
		return res, nil
	}

	prop.Status = store.PropPending
	if _, err := a.St.AddProposal(prop); err != nil {
		return nil, err
	}
	res.ProposalID, res.Status = prop.ID, prop.Status
	res.Message = fmt.Sprintf("%s change queued as proposal #%d. Tell the user: review with `fleet policy pending`, "+
		"approve with `fleet policy approve %d` (in their own terminal).", class, prop.ID, prop.ID)
	_ = a.St.Broadcast(a.AgentID(), a.AgentID(), store.MsgPolicy, map[string]any{
		"proposal": prop.ID, "summary": res.Summary, "by": a.Who(),
		"ask": "review it; if it would affect your work, record your view with `fleet policy comment`",
	})
	a.St.LogEvent(a.AgentID(), "policy.proposed", map[string]any{"proposal": prop.ID, "summary": res.Summary, "class": class})
	return res, nil
}

// DecideProposal approves (applying the change) or rejects a pending proposal.
// Callers must ensure a human is deciding.
func (a *App) DecideProposal(id int64, approve bool, note string) (*store.Proposal, error) {
	p, err := a.St.GetProposal(id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("no proposal #%d", id)
	}
	if p.Status != store.PropPending {
		return nil, fmt.Errorf("proposal #%d is %s, not pending", id, p.Status)
	}
	if !approve {
		if err := a.St.DecideProposal(id, store.PropRejected, note); err != nil {
			return nil, err
		}
		a.St.LogEvent("", "policy.rejected", map[string]any{"proposal": id, "note": note})
		return a.St.GetProposal(id)
	}
	c, err := policy.ParseChange(p.Change)
	if err != nil {
		return nil, err
	}
	path, err := policy.LevelPath(c.Level, c.RepoRoot)
	if err != nil {
		return nil, err
	}
	cur, err := policy.ReadFile(path)
	if err != nil {
		return nil, err
	}
	next, err := policy.Apply(cur, c)
	if err != nil {
		return nil, fmt.Errorf("cannot apply #%d against the current file: %w", id, err)
	}
	if err := policy.WriteFile(path, next); err != nil {
		return nil, err
	}
	a.invalidatePolicy()
	if err := a.St.DecideProposal(id, store.PropApproved, note); err != nil {
		return nil, err
	}
	a.St.LogEvent("", "policy.approved", map[string]any{"proposal": id, "note": note})
	if p.ProposedBy != "" {
		_ = a.St.SendMessage("", p.ProposedBy, store.MsgPolicy, map[string]any{"proposal": id, "status": "approved", "note": note})
	}
	return a.St.GetProposal(id)
}

// ProposalView is a proposal enriched for reviewers.
type ProposalView struct {
	*store.Proposal
	Summary      string        `json:"summary"`
	ParsedChange policy.Change `json:"parsed_change"`
	ProposedName string        `json:"proposed_by_name"`
	IsMine       bool          `json:"is_mine"`
	HowToApprove string        `json:"how_to_approve,omitempty"`
}

func (a *App) ProposalViews(status string, limit int) ([]*ProposalView, error) {
	ps, err := a.St.ListProposals(status, limit)
	if err != nil {
		return nil, err
	}
	me := a.AgentID()
	out := make([]*ProposalView, 0, len(ps))
	for _, p := range ps {
		out = append(out, a.view(p, me))
	}
	return out, nil
}

func (a *App) ProposalView(id int64) (*ProposalView, error) {
	p, err := a.St.GetProposal(id)
	if err != nil || p == nil {
		return nil, err
	}
	return a.view(p, a.AgentID()), nil
}

func (a *App) view(p *store.Proposal, me string) *ProposalView {
	v := &ProposalView{Proposal: p, IsMine: me != "" && p.ProposedBy == me, ProposedName: "human"}
	if c, err := policy.ParseChange(p.Change); err == nil {
		v.ParsedChange, v.Summary = c, c.Summary()
	}
	if p.ProposedBy != "" {
		if ag, _ := a.St.GetAgent(p.ProposedBy); ag != nil {
			v.ProposedName = ag.Name
		} else {
			v.ProposedName = p.ProposedBy
		}
	}
	if p.Status == store.PropPending {
		v.HowToApprove = fmt.Sprintf("fleet policy approve %d   (or: fleet policy reject %d --note \"why\")", p.ID, p.ID)
	}
	return v
}

func (a *App) CommentProposal(id int64, stance, body string) error {
	switch stance {
	case "support", "oppose", "neutral":
	default:
		return errors.New("stance must be support, oppose or neutral")
	}
	p, err := a.St.GetProposal(id)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("no proposal #%d", id)
	}
	if err := a.St.AddComment(&store.Comment{ProposalID: id, AgentID: a.AgentID(), Stance: stance, Body: body}); err != nil {
		return err
	}
	if p.ProposedBy != "" && p.ProposedBy != a.AgentID() {
		_ = a.St.SendMessage(a.AgentID(), p.ProposedBy, store.MsgPolicy, map[string]any{
			"proposal": id, "comment_by": a.Who(), "stance": stance, "comment": body})
	}
	return nil
}

// TestCommand evaluates policy for a command line (no live process).
func (a *App) TestCommand(argv0, cmdline string) policy.Result {
	return a.Policy().Evaluate(policy.Input{Argv0: argv0, Cmdline: cmdline})
}

// TestPID evaluates policy for a live process including runtime marks and lineage.
func (a *App) TestPID(pid int32) (policy.Result, error) {
	snap, err := procs.TakeSnapshot()
	if err != nil {
		return policy.Result{}, err
	}
	info, err := procs.Get(pid)
	if err != nil {
		return policy.Result{}, fmt.Errorf("pid %d: %w", pid, err)
	}
	marks, _ := a.St.CriticalMarks()
	toIn := func(i *procs.Info) policy.Input {
		in := policy.Input{PID: i.PID, Argv0: i.Name, Cmdline: i.Cmdline}
		for k, reason := range marks {
			if int32(k[0]) == i.PID && (k[1]-i.Ctime < 1000 && i.Ctime-k[1] < 1000) {
				in.Critical, in.CriticalReason = true, reason
			}
		}
		return in
	}
	// Ancestors only up to the owning agent's process (shells above an agent are irrelevant).
	var stop int32
	if ps, err := a.St.ListProcesses(""); err == nil {
		for _, p := range ps {
			if p.PID == pid && p.AgentID != "" {
				if ag, _ := a.St.GetAgent(p.AgentID); ag != nil {
					stop = ag.PID
				}
			}
		}
	}
	var anc, desc []policy.Input
	for _, x := range snap.AncestorsWithin(pid, stop) {
		if stop == 0 && procs.IsShellLike(x.Name) {
			break
		}
		anc = append(anc, toIn(x))
	}
	for _, x := range snap.Descendants(pid) {
		desc = append(desc, toIn(x))
	}
	return a.Policy().EvaluateLineage(toIn(info), anc, desc), nil
}
