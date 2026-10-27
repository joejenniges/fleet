package store

import "fmt"

// migrations are applied in order; PRAGMA user_version tracks progress.
// Never edit a released migration — append a new one.
var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE agents (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	kind        TEXT NOT NULL DEFAULT 'other',
	session_id  TEXT,
	pid         INTEGER,
	pid_ctime   INTEGER,
	cwd         TEXT,
	repo_root   TEXT,
	priority    INTEGER NOT NULL DEFAULT 0,
	status      TEXT NOT NULL DEFAULT 'active',
	started_at  INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL,
	ended_at    INTEGER,
	note        TEXT
);
CREATE INDEX agents_proc ON agents(pid, pid_ctime);

CREATE TABLE processes (
	id              INTEGER PRIMARY KEY,
	agent_id        TEXT,
	pid             INTEGER NOT NULL,
	pid_ctime       INTEGER NOT NULL,
	name            TEXT,
	cmdline         TEXT,
	cwd             TEXT,
	ports           TEXT,
	purpose         TEXT,
	critical        INTEGER NOT NULL DEFAULT 0,
	critical_reason TEXT,
	source          TEXT NOT NULL,
	first_seen      INTEGER NOT NULL,
	last_seen       INTEGER NOT NULL,
	ended_at        INTEGER,
	UNIQUE(pid, pid_ctime)
);
CREATE INDEX processes_agent ON processes(agent_id);

CREATE TABLE runs (
	id               INTEGER PRIMARY KEY,
	agent_id         TEXT,
	repo_root        TEXT,
	cwd              TEXT,
	command          TEXT NOT NULL,
	norm_cmd         TEXT NOT NULL,
	argv0            TEXT,
	exit_code        INTEGER,
	duration_ms      INTEGER,
	started_at       INTEGER NOT NULL,
	ended_at         INTEGER,
	critical         INTEGER NOT NULL DEFAULT 0,
	source           TEXT NOT NULL,
	os               TEXT,
	tool_version     TEXT,
	output           TEXT,
	output_lines     INTEGER,
	output_compacted INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX runs_norm ON runs(norm_cmd, repo_root);
CREATE INDEX runs_started ON runs(started_at);

CREATE TABLE docs (
	id              INTEGER PRIMARY KEY,
	kind            TEXT NOT NULL,
	scope           TEXT NOT NULL,
	repo_root       TEXT,
	title           TEXT,
	command         TEXT,
	norm_cmd        TEXT,
	body            TEXT,
	fix_doc_id      INTEGER REFERENCES docs(id),
	evidence_run_id INTEGER REFERENCES runs(id) ON DELETE SET NULL,
	verified        INTEGER NOT NULL DEFAULT 0,
	created_by      TEXT,
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	retired_at      INTEGER,
	retired_reason  TEXT
);
CREATE INDEX docs_norm ON docs(norm_cmd);

CREATE TABLE doc_marks (
	doc_id     INTEGER NOT NULL REFERENCES docs(id) ON DELETE CASCADE,
	repo_root  TEXT NOT NULL DEFAULT '',
	file       TEXT NOT NULL,
	agent_id   TEXT,
	marked_at  INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	PRIMARY KEY (doc_id, repo_root, file)
);

CREATE TABLE policy_proposals (
	id           INTEGER PRIMARY KEY,
	level        TEXT NOT NULL,
	repo_root    TEXT,
	change       TEXT NOT NULL,
	class        TEXT NOT NULL,
	status       TEXT NOT NULL,
	proposed_by  TEXT,
	proposed_at  INTEGER NOT NULL,
	decided_at   INTEGER,
	decided_note TEXT
);

CREATE TABLE policy_comments (
	id          INTEGER PRIMARY KEY,
	proposal_id INTEGER NOT NULL REFERENCES policy_proposals(id) ON DELETE CASCADE,
	agent_id    TEXT,
	stance      TEXT NOT NULL,
	body        TEXT NOT NULL,
	at          INTEGER NOT NULL
);

CREATE TABLE leases (
	resource   TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	agent_id   TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	meta       TEXT
);

CREATE TABLE messages (
	id           INTEGER PRIMARY KEY,
	from_agent   TEXT,
	to_agent     TEXT NOT NULL,
	type         TEXT NOT NULL,
	body         TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	delivered_at INTEGER
);
CREATE INDEX messages_to ON messages(to_agent, delivered_at);

CREATE TABLE board (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	agent_id   TEXT,
	updated_at INTEGER NOT NULL
);

CREATE TABLE events (
	id       INTEGER PRIMARY KEY,
	at       INTEGER NOT NULL,
	agent_id TEXT,
	kind     TEXT NOT NULL,
	detail   TEXT
);

CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`,
}

func (s *Store) migrate() error {
	var v int
	if err := s.DB.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
