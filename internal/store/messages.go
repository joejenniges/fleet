package store

import "time"

// Message types. Bodies are JSON and are always rendered to agents as quoted data,
// never as instructions.
const (
	MsgPreempt        = "preempt"
	MsgReleaseRequest = "release_request"
	MsgFYI            = "fyi"
	MsgPolicy         = "policy"
)

type Message struct {
	ID        int64     `json:"id"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to"`
	Type      string    `json:"type"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) SendMessage(from, to, typ string, body any) error {
	_, err := s.DB.Exec(`INSERT INTO messages(from_agent, to_agent, type, body, created_at) VALUES(?,?,?,?,?)`,
		nullStr(from), to, typ, mustJSON(body), s.ms())
	return err
}

// Broadcast sends one message per active agent (except `except`), so each is
// delivered exactly once to each agent.
func (s *Store) Broadcast(from, except, typ string, body any) error {
	agents, err := s.ListAgents(false)
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.ID == except {
			continue
		}
		if err := s.SendMessage(from, a.ID, typ, body); err != nil {
			return err
		}
	}
	return nil
}

// DrainInbox atomically marks and returns undelivered messages for agentID.
func (s *Store) DrainInbox(agentID string) ([]*Message, error) {
	rows, err := s.DB.Query(`UPDATE messages SET delivered_at = ?
		WHERE to_agent = ? AND delivered_at IS NULL
		RETURNING id, COALESCE(from_agent,''), to_agent, type, body, created_at`, s.ms(), agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		var m Message
		var at int64
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Type, &m.Body, &at); err != nil {
			return nil, err
		}
		m.CreatedAt = time.UnixMilli(at)
		out = append(out, &m)
	}
	return out, rows.Err()
}
