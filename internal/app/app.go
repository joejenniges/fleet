// Package app holds fleet's business logic. The CLI, the MCP server and the hooks
// are thin front-ends over it, so they always behave the same.
package app

import (
	"os"
	"time"

	"github.com/joejenniges/fleet/internal/agentid"
	"github.com/joejenniges/fleet/internal/cmdtext"
	"github.com/joejenniges/fleet/internal/config"
	"github.com/joejenniges/fleet/internal/paths"
	"github.com/joejenniges/fleet/internal/policy"
	"github.com/joejenniges/fleet/internal/procs"
	"github.com/joejenniges/fleet/internal/store"
)

type App struct {
	St   *store.Store
	Cfg  config.Config
	Red  *cmdtext.Redactor
	Cwd  string
	Repo string

	agent         *store.Agent
	agentResolved bool
	policySet     *policy.Set
}

// Open opens the database and config for the current working directory.
func Open() (*App, error) {
	cwd, _ := os.Getwd()
	return OpenAt(cwd)
}

func OpenAt(cwd string) (*App, error) {
	cfg, cfgErr := config.Load()
	red, _ := cmdtext.NewRedactor(cfg.Redact.Patterns)
	p, err := paths.DBPath()
	if err != nil {
		return nil, err
	}
	st, err := store.Open(p)
	if err != nil {
		return nil, err
	}
	a := &App{St: st, Cfg: cfg, Red: red, Cwd: paths.Canon(cwd), Repo: paths.RepoRoot(cwd)}
	if cfgErr != nil {
		st.LogEvent("", "config.invalid", map[string]string{"error": cfgErr.Error()})
	}
	return a, nil
}

func (a *App) Close() {
	if a.St != nil {
		a.St.Close()
	}
}

// SetAgent pins the calling agent (hooks know it from the payload).
func (a *App) SetAgent(ag *store.Agent) { a.agent, a.agentResolved = ag, true }

// Agent resolves the calling agent once. nil means a human (or unknown caller).
func (a *App) Agent() *store.Agent {
	if !a.agentResolved {
		a.agent, _ = agentid.Resolve(a.St)
		if a.agent != nil {
			a.agentResolved = true // cache hits only; a miss is retried (MCP may start before SessionStart)
		}
	}
	return a.agent
}

func (a *App) AgentID() string {
	if ag := a.Agent(); ag != nil {
		return ag.ID
	}
	return ""
}

// Who renders the caller for attribution: agent name, or "human".
func (a *App) Who() string {
	if ag := a.Agent(); ag != nil {
		return ag.Name
	}
	return "human"
}

// Policy loads (and caches for this invocation) the policy set for the repo.
func (a *App) Policy() *policy.Set {
	if a.policySet == nil {
		a.policySet = policy.Load(a.Repo, a.St)
	}
	return a.policySet
}

func (a *App) invalidatePolicy() { a.policySet = nil }

// Housekeep runs lazy maintenance. Cheap when nothing is due.
func (a *App) Housekeep() {
	if a.St.DueEvery("housekeep:reap", 30*time.Second) {
		_, _ = a.St.ReapAgents(procs.Alive)
		if ps, err := a.St.ListProcesses(""); err == nil {
			for _, p := range ps {
				if !procs.Alive(p.PID, p.PIDCtime) {
					_ = a.St.EndProcess(p.PID, p.PIDCtime)
				}
			}
		}
	}
	if a.St.DueEvery("housekeep:prune", 24*time.Hour) {
		_, _, _ = a.Prune()
	}
}

// Prune applies run retention from config.
func (a *App) Prune() (deleted, compacted int64, err error) {
	r := a.Cfg.Runs
	deleted, compacted, err = a.St.PruneRuns(r.MaxAge.Duration, r.CompactAfter.Duration, r.CompactKeepLines)
	if err == nil && (deleted > 0 || compacted > 0) {
		a.St.LogEvent("", "runs.pruned", map[string]int64{"deleted": deleted, "compacted": compacted})
	}
	return
}
