package store

import (
	"database/sql"
	"errors"
	"time"
)

// Proposal statuses.
const (
	PropPending  = "pending"
	PropApplied  = "applied"  // restrictive change, applied immediately
	PropApproved = "approved" // permissive change, approved by a human and applied
	PropRejected = "rejected"
)

type Proposal struct {
	ID          int64      `json:"id"`
	Level       string     `json:"level"`
	RepoRoot    string     `json:"repo_root,omitempty"`
	Change      string     `json:"change"` // JSON of policy.Change
	Class       string     `json:"class"`  // restrictive | permissive
	Status      string     `json:"status"`
	ProposedBy  string     `json:"proposed_by,omitempty"`
	ProposedAt  time.Time  `json:"proposed_at"`
	DecidedAt   time.Time  `json:"decided_at,omitzero"`
	DecidedNote string     `json:"decided_note,omitempty"`
	Comments    []*Comment `json:"comments,omitempty"`
}

type Comment struct {
	ID         int64     `json:"id"`
	ProposalID int64     `json:"proposal_id"`
	AgentID    string    `json:"agent_id,omitempty"`
	Stance     string    `json:"stance"`
	Body       string    `json:"body"`
	At         time.Time `json:"at"`
}

func (s *Store) AddProposal(p *Proposal) (int64, error) {
	res, err := s.DB.Exec(`INSERT INTO policy_proposals(level, repo_root, change, class, status, proposed_by,
			proposed_at, decided_at, decided_note) VALUES(?,?,?,?,?,?,?,?,?)`,
		p.Level, nullStr(p.RepoRoot), p.Change, p.Class, p.Status, nullStr(p.ProposedBy), s.ms(),
		nullMS(p.DecidedAt), nullStr(p.DecidedNote))
	if err != nil {
		return 0, err
	}
	p.ID, err = res.LastInsertId()
	return p.ID, err
}

const propCols = `id, level, COALESCE(repo_root,''), change, class, status, COALESCE(proposed_by,''),
	proposed_at, decided_at, COALESCE(decided_note,'')`

func scanProposal(r interface{ Scan(...any) error }) (*Proposal, error) {
	var p Proposal
	var at int64
	var dec sql.NullInt64
	if err := r.Scan(&p.ID, &p.Level, &p.RepoRoot, &p.Change, &p.Class, &p.Status, &p.ProposedBy, &at,
		&dec, &p.DecidedNote); err != nil {
		return nil, err
	}
	p.ProposedAt, p.DecidedAt = time.UnixMilli(at), fromMS(dec)
	return &p, nil
}

func (s *Store) GetProposal(id int64) (*Proposal, error) {
	p, err := scanProposal(s.DB.QueryRow(`SELECT `+propCols+` FROM policy_proposals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Comments, err = s.Comments(id)
	return p, err
}

// ListProposals lists proposals with the given status ("" = all), newest first, with comments.
func (s *Store) ListProposals(status string, limit int) ([]*Proposal, error) {
	q := `SELECT ` + propCols + ` FROM policy_proposals`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	if limit <= 0 {
		limit = 100
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var out []*Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	for _, p := range out {
		if p.Comments, err = s.Comments(p.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) DecideProposal(id int64, status, note string) error {
	res, err := s.DB.Exec(`UPDATE policy_proposals SET status = ?, decided_at = ?, decided_note = ?
		WHERE id = ? AND status = 'pending'`, status, s.ms(), nullStr(note), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("proposal is not pending")
	}
	return nil
}

func (s *Store) AddComment(c *Comment) error {
	_, err := s.DB.Exec(`INSERT INTO policy_comments(proposal_id, agent_id, stance, body, at) VALUES(?,?,?,?,?)`,
		c.ProposalID, nullStr(c.AgentID), c.Stance, c.Body, s.ms())
	return err
}

func (s *Store) Comments(proposalID int64) ([]*Comment, error) {
	rows, err := s.DB.Query(`SELECT id, proposal_id, COALESCE(agent_id,''), stance, body, at
		FROM policy_comments WHERE proposal_id = ? ORDER BY id`, proposalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Comment
	for rows.Next() {
		var c Comment
		var at int64
		if err := rows.Scan(&c.ID, &c.ProposalID, &c.AgentID, &c.Stance, &c.Body, &at); err != nil {
			return nil, err
		}
		c.At = time.UnixMilli(at)
		out = append(out, &c)
	}
	return out, rows.Err()
}
