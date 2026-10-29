package hooks

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/joejenniges/fleet/internal/app"
	"github.com/joejenniges/fleet/internal/procs"
)

// runStop implements the Stop hook: if the agent is waiting on signals, wait for
// one and exit 2 with its data on stderr so Claude Code wakes (async mode) or
// continues (block mode) the agent. Returns 0 — letting the agent stop normally —
// when there is nothing to wait for, on any error, and when the wait gives up.
func runStop(p *Payload, kind, wake string, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			debugf("stop panic: %v", r)
			code = 0
		}
	}()
	if wake != WakeBlock {
		wake = WakeAsync
	}
	cwd := p.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	a, err := app.OpenAt(cwd)
	if err != nil {
		return 0
	}
	defer a.Close()
	if p.SessionID == "" {
		return 0
	}
	ag, _ := a.St.GetAgent(p.SessionID)
	if ag == nil || ag.Status == "ended" {
		return 0
	}
	a.SetAgent(ag)

	// Something may already have arrived (e.g. while the agent was mid-turn).
	if ds := a.DeliverPending(ag.ID); len(ds) > 0 {
		fmt.Fprint(stderr, app.RenderDeliveries(ds))
		return 2
	}
	if ok, _ := a.St.HasOpenWaits(ag.ID); !ok {
		return 0
	}

	limit := a.Cfg.Signals.MaxWait.Duration
	if wake == WakeBlock {
		limit = a.Cfg.Signals.BlockMax.Duration
	} else {
		// One background waiter per agent: a later turn's waiter takes over only if
		// the earlier one is gone.
		if !claimWaiter(a, ag.ID) {
			return 0
		}
		defer releaseWaiter(a, ag.ID)
	}
	poll := a.Cfg.Signals.PollInterval.Duration
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.Now().Add(limit)
	for i := 0; time.Now().Before(deadline); i++ {
		time.Sleep(poll)
		if ds := a.DeliverPending(ag.ID); len(ds) > 0 {
			a.St.LogEvent(ag.ID, "wait.woke", map[string]any{"mode": wake, "signals": len(ds)})
			fmt.Fprint(stderr, app.RenderDeliveries(ds))
			return 2
		}
		if i%10 == 0 {
			cur, _ := a.St.GetAgent(ag.ID)
			if cur == nil || cur.Status == "ended" {
				return 0
			}
			// If Claude Code exited without SessionEnd (e.g. headless mode), don't
			// linger: end the agent and stop waiting.
			if cur.PID != 0 && !procs.Alive(cur.PID, cur.PIDCtime) {
				_ = a.St.EndAgent(cur.ID)
				return 0
			}
			if ok, _ := a.St.HasOpenWaits(ag.ID); !ok {
				return 0 // waits were cancelled or expired
			}
			if wake == WakeAsync && !stillWaiter(a, ag.ID) {
				return 0 // another waiter took over
			}
		}
	}
	return 0
}

func waiterKey(agentID string) string { return "waiter:" + agentID }

func selfIdent() string {
	pid := int32(os.Getpid())
	var ct int64
	if in, err := procs.Get(pid); err == nil {
		ct = in.Ctime
	}
	return fmt.Sprintf("%d %d", pid, ct)
}

func claimWaiter(a *app.App, agentID string) bool {
	me := selfIdent()
	if v, ok := a.St.MetaGet(waiterKey(agentID)); ok && v != me {
		var pid int32
		var ct int64
		fmt.Sscan(v, &pid, &ct)
		if procs.Alive(pid, ct) {
			return false
		}
	}
	return a.St.MetaSet(waiterKey(agentID), me) == nil
}

func stillWaiter(a *app.App, agentID string) bool {
	v, _ := a.St.MetaGet(waiterKey(agentID))
	return v == selfIdent()
}

func releaseWaiter(a *app.App, agentID string) {
	if stillWaiter(a, agentID) {
		_, _ = a.St.DB.Exec(`DELETE FROM meta WHERE key = ?`, waiterKey(agentID))
	}
}
