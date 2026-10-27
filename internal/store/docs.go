package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Doc kinds.
const (
	KindValid    = "valid-command"
	KindBad      = "known-bad"
	KindBehavior = "behavior"
	KindRecipe   = "recipe"
	KindNote     = "note"
)

var DocKinds = []string{KindValid, KindBad, KindBehavior, KindRecipe, KindNote}

type Doc struct {
	ID            int64     `json:"id"`
	Kind          string    `json:"kind"`
	Scope         string    `json:"scope"` // repo | machine
	RepoRoot      string    `json:"repo_root,omitempty"`
	Title         string    `json:"title,omitempty"`
	Command       string    `json:"command,omitempty"`
	NormCmd       string    `json:"norm_cmd,omitempty"`
	Body          string    `json:"body,omitempty"`
	FixDocID      int64     `json:"fix_doc_id,omitempty"`
	EvidenceRunID int64     `json:"evidence_run_id,omitempty"`
	Verified      bool      `json:"verified"`
	CreatedBy     string    `json:"created_by,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	RetiredAt     time.Time `json:"retired_at,omitzero"`
	RetiredReason string    `json:"retired_reason,omitempty"`

	// Derived, filled by callers that need them.
	Execs int    `json:"execs,omitempty"`
	Tally *Tally `json:"tally,omitempty"`
}

func (d *Doc) Label() string {
	if d.Command != "" {
		return d.Command
	}
	return d.Title
}

var docCols = docColsP("")

func docColsP(p string) string {
	return p + `id, ` + p + `kind, ` + p + `scope, COALESCE(` + p + `repo_root,''), COALESCE(` + p + `title,''),
	COALESCE(` + p + `command,''), COALESCE(` + p + `norm_cmd,''), COALESCE(` + p + `body,''),
	COALESCE(` + p + `fix_doc_id,0), COALESCE(` + p + `evidence_run_id,0), ` + p + `verified,
	COALESCE(` + p + `created_by,''), ` + p + `created_at, ` + p + `updated_at, ` + p + `retired_at,
	COALESCE(` + p + `retired_reason,'')`
}

func scanDoc(r interface{ Scan(...any) error }, extra ...any) (*Doc, error) {
	var d Doc
	var ver int
	var created, updated int64
	var retired sql.NullInt64
	dest := []any{&d.ID, &d.Kind, &d.Scope, &d.RepoRoot, &d.Title, &d.Command, &d.NormCmd, &d.Body,
		&d.FixDocID, &d.EvidenceRunID, &ver, &d.CreatedBy, &created, &updated, &retired, &d.RetiredReason}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	d.Verified = ver != 0
	d.CreatedAt, d.UpdatedAt, d.RetiredAt = time.UnixMilli(created), time.UnixMilli(updated), fromMS(retired)
	return &d, nil
}

func (s *Store) AddDoc(d *Doc) (int64, error) {
	now := s.ms()
	res, err := s.DB.Exec(`INSERT INTO docs(kind, scope, repo_root, title, command, norm_cmd, body, fix_doc_id,
			evidence_run_id, verified, created_by, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.Kind, d.Scope, nullStr(d.RepoRoot), nullStr(d.Title), nullStr(d.Command), nullStr(d.NormCmd),
		nullStr(d.Body), nullInt(d.FixDocID), nullInt(d.EvidenceRunID), boolInt(d.Verified),
		nullStr(d.CreatedBy), now, now)
	if err != nil {
		return 0, err
	}
	d.ID, err = res.LastInsertId()
	return d.ID, err
}

func (s *Store) GetDoc(id int64) (*Doc, error) {
	d, err := scanDoc(s.DB.QueryRow(`SELECT `+docCols+` FROM docs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// FindDoc returns an active doc of kind with the same normalized command in scope.
func (s *Store) FindDoc(kind, norm, repoRoot string) (*Doc, error) {
	d, err := scanDoc(s.DB.QueryRow(`SELECT `+docCols+` FROM docs WHERE kind = ? AND norm_cmd = ?
		AND retired_at IS NULL AND (scope = 'machine' OR repo_root = ?) LIMIT 1`, kind, norm, repoRoot))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

func (s *Store) LinkFix(badID, goodID int64) error {
	_, err := s.DB.Exec(`UPDATE docs SET fix_doc_id = ?, updated_at = ? WHERE id = ?`, goodID, s.ms(), badID)
	return err
}

func (s *Store) MarkVerified(id, runID int64) error {
	_, err := s.DB.Exec(`UPDATE docs SET verified = 1, evidence_run_id = COALESCE(evidence_run_id, ?),
		updated_at = ? WHERE id = ?`, runID, s.ms(), id)
	return err
}

func (s *Store) RetireDoc(id int64, reason string) error {
	_, err := s.DB.Exec(`UPDATE docs SET retired_at = ?, retired_reason = ?, updated_at = ? WHERE id = ?`,
		s.ms(), reason, s.ms(), id)
	return err
}

type DocFilter struct {
	RepoRoot       string // include repo docs for this repo plus machine docs; "" = all
	Kinds          []string
	Query          string
	IncludeRetired bool
	Limit          int
}

func (s *Store) ListDocs(f DocFilter) ([]*Doc, error) {
	var where []string
	var args []any
	if f.RepoRoot != "" {
		where, args = append(where, "(scope = 'machine' OR repo_root = ?)"), append(args, f.RepoRoot)
	}
	if len(f.Kinds) > 0 {
		where = append(where, "kind IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Kinds)), ",")+")")
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if f.Query != "" {
		for _, term := range strings.Fields(f.Query) {
			where = append(where, "(command LIKE ? OR title LIKE ? OR body LIKE ?)")
			p := "%" + term + "%"
			args = append(args, p, p, p)
		}
	}
	if !f.IncludeRetired {
		where = append(where, "retired_at IS NULL")
	}
	q := `SELECT ` + docCols + ` FROM docs`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Doc
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DocsByExecutions returns active valid-command/recipe docs visible from repoRoot,
// ordered by how often their command ran since `since` (most executed first).
func (s *Store) DocsByExecutions(repoRoot string, since time.Time) ([]*Doc, error) {
	rows, err := s.DB.Query(`SELECT `+docColsP("d.")+`, COUNT(r.id) AS execs
		FROM docs d
		LEFT JOIN runs r ON r.norm_cmd = d.norm_cmd AND r.started_at >= ?
			AND (d.scope = 'machine' OR r.repo_root = d.repo_root)
		WHERE d.kind IN ('valid-command','recipe') AND d.retired_at IS NULL
			AND (d.scope = 'machine' OR d.repo_root = ?)
		GROUP BY d.id
		ORDER BY execs DESC, d.updated_at DESC`, since.UnixMilli(), repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Doc
	for rows.Next() {
		var execs int
		d, err := scanDoc(rows, &execs)
		if err != nil {
			return nil, err
		}
		d.Execs = execs
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- "already in my instructions file" marks ----

func (s *Store) MarkKnown(docID int64, repoRoot, file, agentID string, ttl time.Duration) error {
	now := s.now()
	_, err := s.DB.Exec(`INSERT INTO doc_marks(doc_id, repo_root, file, agent_id, marked_at, expires_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(doc_id, repo_root, file) DO UPDATE SET agent_id = excluded.agent_id,
			marked_at = excluded.marked_at, expires_at = excluded.expires_at`,
		docID, repoRoot, file, nullStr(agentID), now.UnixMilli(), now.Add(ttl).UnixMilli())
	return err
}

// ActiveKnownMarks returns doc IDs marked known (unexpired) for repoRoot and any of files.
func (s *Store) ActiveKnownMarks(repoRoot string, files []string) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(files) == 0 {
		return out, nil
	}
	q := `SELECT doc_id FROM doc_marks WHERE repo_root = ? AND expires_at > ? AND file IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(files)), ",") + `)`
	args := []any{repoRoot, s.ms()}
	for _, f := range files {
		args = append(args, f)
	}
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
