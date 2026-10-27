package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/joejenniges/fleet/internal/policy"
	"github.com/joejenniges/fleet/internal/procs"
	"github.com/joejenniges/fleet/internal/store"
)

// ScanAgentProcesses records long-lived processes an agent started: descendants of
// the agent process that listen on a port, plus listeners created since the agent's
// last PreToolUse (catches `cmd &` servers already re-parented away from the agent).
func (a *App) ScanAgentProcesses(ag *store.Agent) {
	if ag == nil || ag.PID == 0 || !a.Cfg.Hooks.ScanProcesses {
		return
	}
	if !a.St.DueEvery("scan:"+ag.ID, a.Cfg.Hooks.ScanMinInterval.Duration) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	ports, err := procs.ListeningPorts(ctx)
	if err != nil || len(ports) == 0 {
		return
	}
	snap, err := procs.TakeSnapshot()
	if err != nil {
		return
	}
	self := int32(os.Getpid())
	cands := map[int32]*procs.Info{}
	for _, d := range snap.Descendants(ag.PID) {
		if _, ok := ports[d.PID]; ok && d.PID != self {
			cands[d.PID] = d
		}
	}
	var since int64
	if v, ok := a.St.MetaGet("lastpre:" + ag.ID); ok {
		fmt.Sscan(v, &since)
	}
	if since > 0 {
		owned := a.ownedByOthers(ag.ID)
		for pid := range ports {
			in := snap.ByPID[pid]
			if in == nil || cands[pid] != nil || owned[pid] {
				continue
			}
			if in.Ctime >= since-1000 && in.Ctime <= time.Now().UnixMilli() {
				if full, err := procs.Get(pid); err == nil {
					cands[pid] = full
				}
			}
		}
	}
	for pid, in := range cands {
		_ = a.St.UpsertProcess(&store.Process{AgentID: ag.ID, PID: pid, PIDCtime: in.Ctime, Name: policy.NormName(in.Name),
			Cmdline: a.Red.Command(in.Cmdline), Ports: ports[pid], Source: "scan"})
	}
}

func (a *App) ownedByOthers(me string) map[int32]bool {
	out := map[int32]bool{}
	ps, _ := a.St.ListProcesses("")
	for _, p := range ps {
		if p.AgentID != "" && p.AgentID != me {
			out[p.PID] = true
		}
	}
	return out
}

type CleanupAction struct {
	PID      int32           `json:"pid"`
	Cmdline  string          `json:"cmdline"`
	Decision policy.Decision `json:"decision"`
	Done     string          `json:"done"` // killed | left | gone
}

// CleanupAgent runs at SessionEnd: each still-running tracked process is handled by
// policy. Only `kill` decisions are acted on; with nobody left to ask, everything
// else is left running and shows up as an orphan in `fleet status`.
func (a *App) CleanupAgent(ag *store.Agent) []CleanupAction {
	ps, err := a.St.ListProcesses(ag.ID)
	if err != nil {
		return nil
	}
	var out []CleanupAction
	for _, p := range ps {
		act := CleanupAction{PID: p.PID, Cmdline: p.Cmdline}
		if !procs.Alive(p.PID, p.PIDCtime) {
			_ = a.St.EndProcess(p.PID, p.PIDCtime)
			act.Done = "gone"
			out = append(out, act)
			continue
		}
		res, err := a.TestPID(p.PID)
		if err != nil {
			continue
		}
		act.Decision = res.Final
		if res.Final.Action == policy.Kill {
			_ = procs.Terminate(p.PID, p.PIDCtime, res.Final.GraceDur)
			_ = a.St.EndProcess(p.PID, p.PIDCtime)
			act.Done = "killed"
		} else {
			act.Done = "left"
		}
		a.St.LogEvent(ag.ID, "cleanup."+act.Done, act)
		out = append(out, act)
	}
	return out
}
