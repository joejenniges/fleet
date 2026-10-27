// Package store is fleet's SQLite persistence layer. Every fleet invocation opens
// the same database file; there is no daemon.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Store struct {
	DB  *sql.DB
	now func() time.Time
}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open(driverName, dsn(path))
	if err != nil {
		return nil, err
	}
	// One connection per process keeps per-connection PRAGMAs (busy_timeout) in force
	// and avoids self-contention; fleet processes are short-lived.
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	s := &Store{DB: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

// Now returns the store clock (overridable in tests).
func (s *Store) Now() time.Time { return s.now() }

// SetClock overrides the clock (tests).
func (s *Store) SetClock(f func() time.Time) { s.now = f }

func (s *Store) ms() int64 { return s.now().UnixMilli() }

func fromMS(v sql.NullInt64) time.Time {
	if !v.Valid || v.Int64 == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64)
}

func nullMS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// LogEvent appends to the audit log. Errors are ignored: auditing must never break callers.
func (s *Store) LogEvent(agentID, kind string, detail any) {
	_, _ = s.DB.Exec(`INSERT INTO events(at, agent_id, kind, detail) VALUES(?,?,?,?)`,
		s.ms(), nullStr(agentID), kind, mustJSON(detail))
}

type Event struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	AgentID string    `json:"agent_id,omitempty"`
	Kind    string    `json:"kind"`
	Detail  string    `json:"detail"`
}

func (s *Store) RecentEvents(limit int) ([]Event, error) {
	rows, err := s.DB.Query(`SELECT id, at, COALESCE(agent_id,''), kind, detail FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.AgentID, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Meta is a tiny key/value table for housekeeping timestamps and caches.
func (s *Store) MetaGet(key string) (string, bool) {
	var v string
	err := s.DB.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) MetaSet(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO meta(key, value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// DueEvery returns true (and records now) if key was last stamped more than every ago.
// Used for lazy housekeeping without a daemon.
func (s *Store) DueEvery(key string, every time.Duration) bool {
	now := s.now()
	if v, ok := s.MetaGet(key); ok {
		var last int64
		fmt.Sscan(v, &last)
		if now.Sub(time.UnixMilli(last)) < every {
			return false
		}
	}
	_ = s.MetaSet(key, fmt.Sprint(now.UnixMilli()))
	return true
}
