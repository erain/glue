package daemon

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type eventLog struct{ db *sql.DB }

func openEventLog(dir string) (*eventLog, error) {
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
 CREATE TABLE IF NOT EXISTS events (run_id TEXT NOT NULL, seq INTEGER NOT NULL, payload_json TEXT NOT NULL, PRIMARY KEY(run_id,seq));` + registrySchema + permissionSchema); err != nil {
		db.Close()
		return nil, err
	}
	return &eventLog{db}, nil
}
func (l *eventLog) append(e EventEnvelope) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := appendRunEvent(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}
func (l *eventLog) read(id string, after int64) ([]EventEnvelope, error) {
	rows, err := l.db.Query("SELECT payload_json FROM events WHERE run_id=? AND seq>? ORDER BY seq", id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []EventEnvelope
	for rows.Next() {
		var data string
		var e EventEnvelope
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
