package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Signal is a named, latched event. It stays set until cleared, so a signal sent
// before anyone waits on it is not lost.
type Signal struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Data      string    `json:"data,omitempty"`
	From      string    `json:"from,omitempty"` // agent id ("" = human)
	To        string    `json:"to,omitempty"`   // target agent id ("" = anyone)
	CreatedAt time.Time `json:"created_at"`
	ClearedAt time.Time `json:"cleared_at,omitzero"`
}

// Wait is an agent's declared need for a signal. The description is what other
// agents read to decide whether their work provides it.
type Wait struct {
	ID          int64     `json:"id"`
	AgentID     string    `json:"agent_id,omitempty"`
	Signal      string    `json:"signal"`
	Description string    `json:"description"`
	RepoRoot    string    `json:"repo_root,omitempty"`
	Cwd         string    `json:"cwd,omitempty"`
	Mode        string    `json:"mode"` // block | detach
	Fresh       bool      `json:"fresh,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	SatisfiedBy int64     `json:"satisfied_by,omitempty"`
	SatisfiedAt time.Time `json:"satisfied_at,omitzero"`
	DeliveredAt time.Time `json:"delivered_at,omitzero"`
	CancelledAt time.Time `json:"cancelled_at,omitzero"`
}

func (w *Wait) Open() bool { return w.SatisfiedAt.IsZero() && w.CancelledAt.IsZero() }

const signalCols = `id, name, COALESCE(data,''), COALESCE(from_agent,''), COALESCE(to_agent,''), created_at, cleared_at`

func scanSignal(r interface{ Scan(...any) error }) (*Signal, error) {
	var s Signal
	var at int64
	var cl sql.NullInt64
	if err := r.Scan(&s.ID, &s.Name, &s.Data, &s.From, &s.To, &at, &cl); err != nil {
		return nil, err
	}
	s.CreatedAt, s.ClearedAt = time.UnixMilli(at), fromMS(cl)
	return &s, nil
}

const waitCols = `id, COALESCE(agent_id,''), signal, description, COALESCE(repo_root,''), COALESCE(cwd,''), mode, fresh,
	created_at, expires_at, COALESCE(satisfied_by,0), satisfied_at, delivered_at, cancelled_at`

func scanWait(r interface{ Scan(...any) error }) (*Wait, error) {
	var w Wait
	var fresh int
	var created int64
	var exp, sat, del, can sql.NullInt64
	if err := r.Scan(&w.ID, &w.AgentID, &w.Signal, &w.Description, &w.RepoRoot, &w.Cwd, &w.Mode, &fresh,
		&created, &exp, &w.SatisfiedBy, &sat, &del, &can); err != nil {
		return nil, err
	}
	w.Fresh = fresh != 0
	w.CreatedAt, w.ExpiresAt = time.UnixMilli(created), fromMS(exp)
	w.SatisfiedAt, w.DeliveredAt, w.CancelledAt = fromMS(sat), fromMS(del), fromMS(can)
	return &w, nil
}

func (s *Store) GetSignal(id int64) (*Signal, error) {
	sg, err := scanSignal(s.DB.QueryRow(`SELECT `+signalCols+` FROM signals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sg, err
}

func (s *Store) GetWait(id int64) (*Wait, error) {
	w, err := scanWait(s.DB.QueryRow(`SELECT `+waitCols+` FROM waits WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return w, err
}

// openWaitCond selects waits that are neither satisfied, cancelled nor expired.
const openWaitCond = `satisfied_at IS NULL AND cancelled_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`

// AddWait registers a wait. An agent's existing open wait on the same signal is
// reused (description, mode and expiry refreshed), so re-running a timed-out
// `fleet wait` does not create duplicates. Returns the wait and whether it is new.
// If a matching signal is already set, the wait is satisfied immediately.
func (s *Store) AddWait(w *Wait) (*Wait, bool, error) {
	now := s.ms()
	if w.AgentID != "" {
		ex, err := scanWait(s.DB.QueryRow(`SELECT `+waitCols+` FROM waits WHERE agent_id = ? AND signal = ? AND `+
			openWaitCond+` ORDER BY id DESC LIMIT 1`, w.AgentID, w.Signal, now))
		if err == nil {
			if _, err := s.DB.Exec(`UPDATE waits SET description = ?, mode = ?, expires_at = ? WHERE id = ?`,
				w.Description, w.Mode, nullMS(w.ExpiresAt), ex.ID); err != nil {
				return nil, false, err
			}
			got, err := s.GetWait(ex.ID)
			return got, false, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
	}
	res, err := s.DB.Exec(`INSERT INTO waits(agent_id, signal, description, repo_root, cwd, mode, fresh, created_at, expires_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, nullStr(w.AgentID), w.Signal, w.Description, nullStr(w.RepoRoot), nullStr(w.Cwd),
		w.Mode, boolInt(w.Fresh), now, nullMS(w.ExpiresAt))
	if err != nil {
		return nil, false, err
	}
	id, _ := res.LastInsertId()
	if err := s.trySatisfy(id); err != nil {
		return nil, true, err
	}
	got, err := s.GetWait(id)
	return got, true, err
}

// trySatisfy satisfies a new wait from an already-set signal, if one matches.
func (s *Store) trySatisfy(waitID int64) error {
	_, err := s.DB.Exec(`UPDATE waits SET satisfied_at = ?, satisfied_by = (
			SELECT g.id FROM signals g WHERE g.name = waits.signal AND g.cleared_at IS NULL
				AND (g.to_agent IS NULL OR g.to_agent = waits.agent_id)
				AND (waits.fresh = 0 OR g.created_at >= waits.created_at)
			ORDER BY g.id DESC LIMIT 1)
		WHERE id = ? AND EXISTS (
			SELECT 1 FROM signals g WHERE g.name = waits.signal AND g.cleared_at IS NULL
				AND (g.to_agent IS NULL OR g.to_agent = waits.agent_id)
				AND (waits.fresh = 0 OR g.created_at >= waits.created_at))`, s.ms(), waitID)
	return err
}

// FireSignal records a signal and satisfies every matching open wait. Returns the
// waits it satisfied.
func (s *Store) FireSignal(sg *Signal) ([]*Wait, error) {
	now := s.ms()
	res, err := s.DB.Exec(`INSERT INTO signals(name, data, from_agent, to_agent, created_at) VALUES(?,?,?,?,?)`,
		sg.Name, nullStr(sg.Data), nullStr(sg.From), nullStr(sg.To), now)
	if err != nil {
		return nil, err
	}
	sg.ID, _ = res.LastInsertId()
	sg.CreatedAt = time.UnixMilli(now)
	q := `UPDATE waits SET satisfied_by = ?, satisfied_at = ? WHERE signal = ? AND ` + openWaitCond
	args := []any{sg.ID, now, sg.Name, now}
	if sg.To != "" {
		q += ` AND agent_id = ?`
		args = append(args, sg.To)
	}
	rows, err := s.DB.Query(q+` RETURNING `+waitCols, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Wait
	for rows.Next() {
		w, err := scanWait(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ClearSignal un-sets a signal name so later waits block again.
func (s *Store) ClearSignal(name string) (int64, error) {
	res, err := s.DB.Exec(`UPDATE signals SET cleared_at = ? WHERE name = ? AND cleared_at IS NULL`, s.ms(), name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type WaitFilter struct {
	AgentID      string
	ExcludeAgent string
	Signal       string
	OpenOnly     bool
	Limit        int
}

func (s *Store) ListWaits(f WaitFilter) ([]*Wait, error) {
	var where []string
	var args []any
	if f.AgentID != "" {
		where, args = append(where, "agent_id = ?"), append(args, f.AgentID)
	}
	if f.ExcludeAgent != "" {
		where, args = append(where, "(agent_id IS NULL OR agent_id != ?)"), append(args, f.ExcludeAgent)
	}
	if f.Signal != "" {
		where, args = append(where, "signal = ?"), append(args, f.Signal)
	}
	if f.OpenOnly {
		where, args = append(where, openWaitCond), append(args, s.ms())
	}
	q := `SELECT ` + waitCols + ` FROM waits`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 {
		f.Limit = 100
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Wait
	for rows.Next() {
		w, err := scanWait(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) ListSignals(limit int) ([]*Signal, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.Query(`SELECT `+signalCols+` FROM signals ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Signal
	for rows.Next() {
		sg, err := scanSignal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sg)
	}
	return out, rows.Err()
}

// DeliverWaits atomically marks an agent's satisfied-but-undelivered waits as
// delivered and returns them, so each satisfied wait reaches the agent exactly once
// whichever path (blocking `fleet wait`, Stop hook, inbox) gets there first.
func (s *Store) DeliverWaits(agentID string) ([]*Wait, error) {
	return s.deliver(`agent_id = ?`, agentID)
}

// DeliverWait is DeliverWaits for a single wait.
func (s *Store) DeliverWait(waitID int64) (*Wait, error) {
	ws, err := s.deliver(`id = ?`, waitID)
	if err != nil || len(ws) == 0 {
		return nil, err
	}
	return ws[0], nil
}

func (s *Store) deliver(cond string, arg any) ([]*Wait, error) {
	rows, err := s.DB.Query(`UPDATE waits SET delivered_at = ? WHERE `+cond+`
		AND satisfied_at IS NOT NULL AND delivered_at IS NULL AND cancelled_at IS NULL RETURNING `+waitCols, s.ms(), arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Wait
	for rows.Next() {
		w, err := scanWait(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CancelWaits cancels an agent's open waits (signal "" = all). Returns the count.
func (s *Store) CancelWaits(agentID, signal string) (int64, error) {
	q := `UPDATE waits SET cancelled_at = ? WHERE agent_id = ? AND satisfied_at IS NULL AND cancelled_at IS NULL`
	args := []any{s.ms(), agentID}
	if signal != "" {
		q += ` AND signal = ?`
		args = append(args, signal)
	}
	res, err := s.DB.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// HasOpenWaits reports whether an agent has any open (or satisfied-undelivered) wait.
func (s *Store) HasOpenWaits(agentID string) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM waits WHERE agent_id = ? AND cancelled_at IS NULL AND
		((`+openWaitCond+`) OR (satisfied_at IS NOT NULL AND delivered_at IS NULL))`, agentID, s.ms()).Scan(&n)
	return n > 0, err
}
