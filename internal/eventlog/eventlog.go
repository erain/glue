// Package eventlog stores ordered durable event envelopes in shared SQLite state.
package eventlog

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/erain/glue/internal/redact"
	_ "modernc.org/sqlite"
)

// Envelope is the common daemon/job event representation. RunID keys the stream.
type Envelope struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Seq       int64     `json:"seq"`
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	Payload   any       `json:"payload,omitempty"`
	Phase     string    `json:"phase,omitempty"`
	Iteration *int      `json:"iteration,omitempty"`
}

// Store exposes the database so owners can commit metadata with event insertion.
// The owner closes DB after stopping its writers and readers.
type Store struct{ DB *sql.DB }

// Open initializes event storage and owner schema. Empty dir uses isolated memory.
// Schema must be trusted application SQL, never user input.
func Open(dir, schema string) (*Store, error) {
	path := ":memory:"
	if dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		path = filepath.Join(dir, "events.sqlite")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
 CREATE TABLE IF NOT EXISTS events (run_id TEXT NOT NULL, seq INTEGER NOT NULL, payload_json TEXT NOT NULL, PRIMARY KEY(run_id,seq));` + schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

// Insert writes an event inside the owner's transaction, before publication.
func Insert(tx *sql.Tx, e Envelope) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = redact.Bytes(data)
	_, err = tx.Exec("INSERT INTO events(run_id,seq,payload_json) VALUES(?,?,?)", e.RunID, e.Seq, string(data))
	return err
}

// Read returns strictly later events in sequence order.
func (s *Store) Read(id string, after int64) ([]Envelope, error) { return read(s.DB, id, after) }

// ReadTx reads events in the owner's existing snapshot transaction.
func ReadTx(tx *sql.Tx, id string, after int64) ([]Envelope, error) { return read(tx, id, after) }

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

func read(source queryer, id string, after int64) ([]Envelope, error) {
	rows, err := source.Query("SELECT payload_json FROM events WHERE run_id=? AND seq>? ORDER BY seq", id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Envelope
	for rows.Next() {
		var data string
		var e Envelope
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
