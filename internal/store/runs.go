package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Run struct {
	ID              int64     `json:"id"`
	AgentID         string    `json:"agent_id,omitempty"`
	RepoRoot        string    `json:"repo_root,omitempty"`
	Cwd             string    `json:"cwd,omitempty"`
	Command         string    `json:"command"`
	NormCmd         string    `json:"norm_cmd"`
	Argv0           string    `json:"argv0,omitempty"`
	ExitCode        *int      `json:"exit_code"`
	DurationMS      int64     `json:"duration_ms,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at,omitzero"`
	Critical        bool      `json:"critical,omitempty"`
	Source          string    `json:"source"`
	OS              string    `json:"os,omitempty"`
	ToolVersion     string    `json:"tool_version,omitempty"`
	Output          string    `json:"output,omitempty"`
	OutputLines     int       `json:"output_lines,omitempty"`
	OutputCompacted bool      `json:"output_compacted,omitempty"`
}

func (r *Run) Succeeded() bool { return r.ExitCode != nil && *r.ExitCode == 0 }

// StartRun inserts a run before the command executes, so crashes stay visible.
func (s *Store) StartRun(r *Run) (int64, error) {
	if r.StartedAt.IsZero() {
		r.StartedAt = s.now()
	}
	res, err := s.DB.Exec(`INSERT INTO runs(agent_id, repo_root, cwd, command, norm_cmd, argv0, critical,
			source, os, tool_version, started_at, exit_code, duration_ms, ended_at, output, output_lines)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nullStr(r.AgentID), nullStr(r.RepoRoot), nullStr(r.Cwd), r.Command, r.NormCmd, nullStr(r.Argv0),
		boolInt(r.Critical), r.Source, nullStr(r.OS), nullStr(r.ToolVersion), r.StartedAt.UnixMilli(),
		intPtr(r.ExitCode), nullInt(r.DurationMS), nullMS(r.EndedAt), nullStr(r.Output), r.OutputLines)
	if err != nil {
		return 0, err
	}
	r.ID, err = res.LastInsertId()
	return r.ID, err
}

// FinishRun completes a run started with StartRun.
func (s *Store) FinishRun(id int64, exit int, dur time.Duration, output string, lines int, toolVersion string) error {
	_, err := s.DB.Exec(`UPDATE runs SET exit_code = ?, duration_ms = ?, ended_at = ?, output = ?,
		output_lines = ?, tool_version = COALESCE(?, tool_version) WHERE id = ?`,
		exit, dur.Milliseconds(), s.ms(), nullStr(output), lines, nullStr(toolVersion), id)
	return err
}

const runCols = `id, COALESCE(agent_id,''), COALESCE(repo_root,''), COALESCE(cwd,''), command, norm_cmd,
	COALESCE(argv0,''), exit_code, COALESCE(duration_ms,0), started_at, ended_at, critical, source,
	COALESCE(os,''), COALESCE(tool_version,''), COALESCE(output,''), COALESCE(output_lines,0), output_compacted`

func scanRun(r interface{ Scan(...any) error }) (*Run, error) {
	var x Run
	var exit sql.NullInt64
	var started int64
	var ended sql.NullInt64
	var crit, comp int
	if err := r.Scan(&x.ID, &x.AgentID, &x.RepoRoot, &x.Cwd, &x.Command, &x.NormCmd, &x.Argv0, &exit,
		&x.DurationMS, &started, &ended, &crit, &x.Source, &x.OS, &x.ToolVersion, &x.Output,
		&x.OutputLines, &comp); err != nil {
		return nil, err
	}
	if exit.Valid {
		v := int(exit.Int64)
		x.ExitCode = &v
	}
	x.StartedAt, x.EndedAt = time.UnixMilli(started), fromMS(ended)
	x.Critical, x.OutputCompacted = crit != 0, comp != 0
	return &x, nil
}

func (s *Store) GetRun(id int64) (*Run, error) {
	r, err := scanRun(s.DB.QueryRow(`SELECT `+runCols+` FROM runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

type RunFilter struct {
	AgentID  string
	RepoRoot string
	NormCmd  string
	Failed   bool
	Contains string
	Limit    int
}

func (s *Store) ListRuns(f RunFilter) ([]*Run, error) {
	var where []string
	var args []any
	if f.AgentID != "" {
		where, args = append(where, "agent_id = ?"), append(args, f.AgentID)
	}
	if f.RepoRoot != "" {
		where, args = append(where, "repo_root = ?"), append(args, f.RepoRoot)
	}
	if f.NormCmd != "" {
		where, args = append(where, "norm_cmd = ?"), append(args, f.NormCmd)
	}
	if f.Failed {
		where = append(where, "exit_code IS NOT NULL AND exit_code != 0")
	}
	if f.Contains != "" {
		where, args = append(where, "command LIKE ?"), append(args, "%"+f.Contains+"%")
	}
	q := `SELECT ` + runCols + ` FROM runs`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Tally summarizes the recorded history of one normalized command.
type Tally struct {
	Successes    int       `json:"successes"`
	Failures     int       `json:"failures"`
	Unknown      int       `json:"unknown"`
	LastSuccess  time.Time `json:"last_success,omitzero"`
	LastFailure  time.Time `json:"last_failure,omitzero"`
	ToolVersions []string  `json:"tool_versions,omitempty"`
	// Stale is true when the most recent runs with a known outcome all failed.
	Stale bool `json:"stale"`
}

// TallyFor computes a tally for norm. repoRoot "" = across all repos.
func (s *Store) TallyFor(norm, repoRoot string) (Tally, error) {
	var t Tally
	q := `SELECT exit_code, started_at, COALESCE(tool_version,'') FROM runs WHERE norm_cmd = ?`
	args := []any{norm}
	if repoRoot != "" {
		q += ` AND repo_root = ?`
		args = append(args, repoRoot)
	}
	rows, err := s.DB.Query(q+` ORDER BY started_at DESC`, args...)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	versions := map[string]bool{}
	var recent []bool // outcomes, newest first, known only
	for rows.Next() {
		var exit sql.NullInt64
		var at int64
		var ver string
		if err := rows.Scan(&exit, &at, &ver); err != nil {
			return t, err
		}
		if ver != "" && !versions[ver] {
			versions[ver] = true
			t.ToolVersions = append(t.ToolVersions, ver)
		}
		switch {
		case !exit.Valid:
			t.Unknown++
		case exit.Int64 == 0:
			t.Successes++
			if t.LastSuccess.IsZero() {
				t.LastSuccess = time.UnixMilli(at)
			}
			recent = append(recent, true)
		default:
			t.Failures++
			if t.LastFailure.IsZero() {
				t.LastFailure = time.UnixMilli(at)
			}
			recent = append(recent, false)
		}
	}
	if len(recent) > 0 && !recent[0] {
		n := min(3, len(recent))
		t.Stale = true
		for _, ok := range recent[:n] {
			if ok {
				t.Stale = false
			}
		}
		// A single failure after a long history of successes is not "stale" yet.
		if n < 3 && t.Successes > 0 {
			t.Stale = false
		}
	}
	return t, rows.Err()
}

// PruneRuns applies retention: delete runs older than maxAge (0 = never) and
// compact output of runs older than compactAfter to their last keepLines lines.
func (s *Store) PruneRuns(maxAge, compactAfter time.Duration, keepLines int) (deleted, compacted int64, err error) {
	now := s.now()
	if maxAge > 0 {
		res, err := s.DB.Exec(`DELETE FROM runs WHERE started_at < ? AND id NOT IN
			(SELECT evidence_run_id FROM docs WHERE evidence_run_id IS NOT NULL)`, now.Add(-maxAge).UnixMilli())
		if err != nil {
			return 0, 0, err
		}
		deleted, _ = res.RowsAffected()
	}
	if compactAfter > 0 && keepLines > 0 {
		rows, err := s.DB.Query(`SELECT id, output FROM runs WHERE output_compacted = 0
			AND output IS NOT NULL AND started_at < ?`, now.Add(-compactAfter).UnixMilli())
		if err != nil {
			return deleted, 0, err
		}
		type item struct {
			id  int64
			out string
		}
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.out); err != nil {
				rows.Close()
				return deleted, 0, err
			}
			items = append(items, it)
		}
		rows.Close()
		for _, it := range items {
			if _, err := s.DB.Exec(`UPDATE runs SET output = ?, output_compacted = 1 WHERE id = ?`,
				LastLines(it.out, keepLines), it.id); err != nil {
				return deleted, compacted, err
			}
			compacted++
		}
	}
	return deleted, compacted, nil
}

// LastLines returns the last n lines of s.
func LastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if n <= 0 || s == "" {
		return ""
	}
	idx := len(s)
	for i := 0; i < n; i++ {
		j := strings.LastIndexByte(s[:idx], '\n')
		if j < 0 {
			return s
		}
		idx = j
	}
	return s[idx+1:]
}

func intPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func unmarshalJSON(s string, v any) { _ = json.Unmarshal([]byte(s), v) }
