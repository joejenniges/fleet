package store

import (
	"database/sql"
	"errors"
	"time"
)

type Agent struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	SessionID string    `json:"session_id,omitempty"`
	PID       int32     `json:"pid"`
	PIDCtime  int64     `json:"pid_ctime"`
	Cwd       string    `json:"cwd"`
	RepoRoot  string    `json:"repo_root,omitempty"`
	Priority  int       `json:"priority"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	Note      string    `json:"note,omitempty"`
}

const agentCols = `id, name, kind, COALESCE(session_id,''), COALESCE(pid,0), COALESCE(pid_ctime,0),
	COALESCE(cwd,''), COALESCE(repo_root,''), priority, status, started_at, last_seen, ended_at, COALESCE(note,'')`

func scanAgent(r interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var started, seen int64
	var ended sql.NullInt64
	if err := r.Scan(&a.ID, &a.Name, &a.Kind, &a.SessionID, &a.PID, &a.PIDCtime, &a.Cwd,
		&a.RepoRoot, &a.Priority, &a.Status, &started, &seen, &ended, &a.Note); err != nil {
		return nil, err
	}
	a.StartedAt, a.LastSeen, a.EndedAt = time.UnixMilli(started), time.UnixMilli(seen), fromMS(ended)
	return &a, nil
}

// UpsertAgent registers an agent, or refreshes it (SessionStart fires again on
// resume/clear/compact).
func (s *Store) UpsertAgent(a *Agent) error {
	now := s.ms()
	_, err := s.DB.Exec(`INSERT INTO agents(id, name, kind, session_id, pid, pid_ctime, cwd, repo_root,
			status, started_at, last_seen)
		VALUES(?,?,?,?,?,?,?,?,'active',?,?)
		ON CONFLICT(id) DO UPDATE SET
			pid = excluded.pid, pid_ctime = excluded.pid_ctime, cwd = excluded.cwd,
			repo_root = excluded.repo_root, status = 'active', last_seen = excluded.last_seen,
			ended_at = NULL`,
		a.ID, a.Name, a.Kind, nullStr(a.SessionID), a.PID, a.PIDCtime, a.Cwd, nullStr(a.RepoRoot), now, now)
	return err
}

func (s *Store) GetAgent(id string) (*Agent, error) {
	a, err := scanAgent(s.DB.QueryRow(`SELECT `+agentCols+` FROM agents WHERE id = ? OR name = ?`, id, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// AgentByProcess finds the active agent whose process is (pid, ctime). Create times are
// compared with 1s tolerance because some platforms round them differently between reads.
func (s *Store) AgentByProcess(pid int32, ctime int64) (*Agent, error) {
	a, err := scanAgent(s.DB.QueryRow(`SELECT `+agentCols+` FROM agents
		WHERE pid = ? AND ABS(pid_ctime - ?) < 1000 AND status != 'ended' ORDER BY last_seen DESC LIMIT 1`, pid, ctime))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (s *Store) ListAgents(includeEnded bool) ([]*Agent, error) {
	q := `SELECT ` + agentCols + ` FROM agents`
	if !includeEnded {
		q += ` WHERE status != 'ended'`
	}
	q += ` ORDER BY started_at`
	rows, err := s.DB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) TouchAgent(id string) {
	_, _ = s.DB.Exec(`UPDATE agents SET last_seen = ? WHERE id = ?`, s.ms(), id)
}

func (s *Store) EndAgent(id string) error {
	now := s.ms()
	if _, err := s.DB.Exec(`UPDATE agents SET status = 'ended', ended_at = ? WHERE id = ?`, now, id); err != nil {
		return err
	}
	_, err := s.DB.Exec(`DELETE FROM leases WHERE agent_id = ?`, id)
	return err
}

func (s *Store) SetPriority(id string, p int) error {
	_, err := s.DB.Exec(`UPDATE agents SET priority = ? WHERE id = ?`, p, id)
	return err
}

// ReapAgents ends active agents whose process is gone.
func (s *Store) ReapAgents(alive func(pid int32, ctime int64) bool) ([]string, error) {
	agents, err := s.ListAgents(false)
	if err != nil {
		return nil, err
	}
	var reaped []string
	for _, a := range agents {
		if a.PID == 0 || alive(a.PID, a.PIDCtime) {
			continue
		}
		if err := s.EndAgent(a.ID); err == nil {
			reaped = append(reaped, a.ID)
			s.LogEvent(a.ID, "agent.reaped", map[string]any{"pid": a.PID})
		}
	}
	return reaped, nil
}

// ---- processes ----

type Process struct {
	ID             int64     `json:"id"`
	AgentID        string    `json:"agent_id,omitempty"`
	PID            int32     `json:"pid"`
	PIDCtime       int64     `json:"pid_ctime"`
	Name           string    `json:"name"`
	Cmdline        string    `json:"cmdline"`
	Cwd            string    `json:"cwd,omitempty"`
	Ports          []uint32  `json:"ports,omitempty"`
	Purpose        string    `json:"purpose,omitempty"`
	Critical       bool      `json:"critical"`
	CriticalReason string    `json:"critical_reason,omitempty"`
	Source         string    `json:"source"`
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
	EndedAt        time.Time `json:"ended_at,omitzero"`
}

// UpsertProcess records or refreshes a tracked process. Purpose/critical are only
// overwritten when set on p.
func (s *Store) UpsertProcess(p *Process) error {
	now := s.ms()
	_, err := s.DB.Exec(`INSERT INTO processes(agent_id, pid, pid_ctime, name, cmdline, cwd, ports, purpose,
			critical, critical_reason, source, first_seen, last_seen)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(pid, pid_ctime) DO UPDATE SET
			ports = excluded.ports, last_seen = excluded.last_seen, ended_at = NULL,
			agent_id = COALESCE(processes.agent_id, excluded.agent_id),
			purpose = COALESCE(excluded.purpose, processes.purpose),
			critical = MAX(processes.critical, excluded.critical),
			critical_reason = COALESCE(excluded.critical_reason, processes.critical_reason)`,
		nullStr(p.AgentID), p.PID, p.PIDCtime, p.Name, p.Cmdline, nullStr(p.Cwd), mustJSON(p.Ports),
		nullStr(p.Purpose), boolInt(p.Critical), nullStr(p.CriticalReason), p.Source, now, now)
	return err
}

func (s *Store) EndProcess(pid int32, ctime int64) error {
	_, err := s.DB.Exec(`UPDATE processes SET ended_at = ?, critical = 0 WHERE pid = ? AND pid_ctime = ?`, s.ms(), pid, ctime)
	return err
}

func (s *Store) SetCritical(pid int32, ctime int64, critical bool, reason string) error {
	_, err := s.DB.Exec(`UPDATE processes SET critical = ?, critical_reason = ? WHERE pid = ? AND pid_ctime = ?`,
		boolInt(critical), nullStr(reason), pid, ctime)
	return err
}

// ListProcesses returns tracked processes that have not ended. agentID "" = all.
func (s *Store) ListProcesses(agentID string) ([]*Process, error) {
	q := `SELECT id, COALESCE(agent_id,''), pid, pid_ctime, COALESCE(name,''), COALESCE(cmdline,''),
		COALESCE(cwd,''), COALESCE(ports,'[]'), COALESCE(purpose,''), critical, COALESCE(critical_reason,''),
		source, first_seen, last_seen FROM processes WHERE ended_at IS NULL`
	var args []any
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	rows, err := s.DB.Query(q+` ORDER BY first_seen`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Process
	for rows.Next() {
		var p Process
		var ports string
		var crit int
		var first, last int64
		if err := rows.Scan(&p.ID, &p.AgentID, &p.PID, &p.PIDCtime, &p.Name, &p.Cmdline, &p.Cwd, &ports,
			&p.Purpose, &crit, &p.CriticalReason, &p.Source, &first, &last); err != nil {
			return nil, err
		}
		p.Critical = crit != 0
		p.FirstSeen, p.LastSeen = time.UnixMilli(first), time.UnixMilli(last)
		unmarshalJSON(ports, &p.Ports)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// CriticalMarks returns the set of (pid, ctime) currently marked critical.
func (s *Store) CriticalMarks() (map[[2]int64]string, error) {
	rows, err := s.DB.Query(`SELECT pid, pid_ctime, COALESCE(critical_reason,'') FROM processes
		WHERE critical = 1 AND ended_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]int64]string{}
	for rows.Next() {
		var pid, ct int64
		var reason string
		if err := rows.Scan(&pid, &ct, &reason); err != nil {
			return nil, err
		}
		out[[2]int64{pid, ct}] = reason
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
