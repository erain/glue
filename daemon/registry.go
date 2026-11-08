package daemon

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/erain/glue/internal/eventlog"
)

const registrySchema = `CREATE TABLE IF NOT EXISTS runs (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, client_id TEXT NOT NULL,
 status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 last_seq INTEGER NOT NULL DEFAULT 0, error_json TEXT NOT NULL DEFAULT 'null');
 CREATE INDEX IF NOT EXISTS runs_session_created ON runs(session_id,created_at);
 CREATE INDEX IF NOT EXISTS runs_status ON runs(status);`

type runRecord struct {
	ID                 string             `json:"id"`
	SessionID          string             `json:"session_id"`
	ClientID           string             `json:"client_id"`
	Status             string             `json:"status"`
	CreatedAt          string             `json:"created_at"`
	UpdatedAt          string             `json:"updated_at"`
	LastSeq            int64              `json:"last_seq"`
	Error              json.RawMessage    `json:"error"`
	PendingPermissions []permissionRecord `json:"pending_permissions"`
}

const runColumns = "id,session_id,client_id,status,created_at,updated_at,last_seq,error_json"

func timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func scanRecord(row interface{ Scan(...any) error }) (runRecord, error) {
	var r runRecord
	var data string
	err := row.Scan(&r.ID, &r.SessionID, &r.ClientID, &r.Status, &r.CreatedAt, &r.UpdatedAt, &r.LastSeq, &data)
	r.Error = json.RawMessage(data)
	r.PendingPermissions = []permissionRecord{}
	return r, err
}
func (l *eventLog) record(id string) (runRecord, error) {
	return scanRecord(l.db.QueryRow("SELECT "+runColumns+" FROM runs WHERE id=?", id))
}
func (l *eventLog) createRun(id, session, client string, now time.Time) error {
	_, err := l.db.Exec("INSERT INTO runs(id,session_id,client_id,status,created_at,updated_at) VALUES(?,?,?,'running',?,?)", id, session, client, timestamp(now), timestamp(now))
	return err
}
func appendRunEvent(tx *sql.Tx, e EventEnvelope) error {
	if err := eventlog.Insert(tx, eventlog.Envelope(e)); err != nil {
		return err
	}
	if e.Type == "permission_request" {
		if err := storePermission(tx, e); err != nil {
			return err
		}
	}
	status := "running"
	errData := "null"
	switch e.Type {
	case "run_done":
		status = "done"
	case "run_interrupted":
		status = "interrupted"
	case "run_error":
		status = "error"
		var payload struct {
			Error json.RawMessage `json:"error"`
		}
		p, err := json.Marshal(e.Payload)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(p, &payload); err != nil {
			return err
		}
		if len(payload.Error) > 0 {
			errData = string(payload.Error)
		}
		var detail struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(errData), &detail); err != nil {
			return err
		}
		if detail.Code == "canceled" {
			status = "canceled"
		}
	}
	_, err := tx.Exec("UPDATE runs SET status=?,updated_at=?,last_seq=?,error_json=? WHERE id=?", status, timestamp(e.Time), e.Seq, errData, e.RunID)
	return err
}
func (s *Server) recoverRuns() error {
	tx, err := s.log.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT " + runColumns + " FROM runs WHERE status='running'")
	if err != nil {
		return err
	}
	var runs []runRecord
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			rows.Close()
			return err
		}
		runs = append(runs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE permissions SET state='expired' WHERE state='pending' AND run_id IN (SELECT id FROM runs WHERE status='running')"); err != nil {
		return err
	}
	for _, r := range runs {
		e := EventEnvelope{Version: protocolVersion, ID: s.newID("evt"), Seq: r.LastSeq + 1, RunID: r.ID, SessionID: r.SessionID, Time: s.now().UTC(), Type: "run_interrupted"}
		if err := appendRunEvent(tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request, id string) {
	record, err := s.log.record(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "run not found", false)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	record.PendingPermissions, err = s.log.permissions(id, "pending")
	if err != nil {
		writeError(w, 500, "internal", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, record)
}
func (s *Server) handleRunList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 1000", false)
			return
		}
		limit = n
	}
	status := q.Get("status")
	switch status {
	case "", "running", "done", "error", "canceled", "interrupted":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid run status", false)
		return
	}
	query := "SELECT " + runColumns + " FROM runs WHERE 1=1"
	args := []any{}
	if session := q.Get("session_id"); session != "" {
		query += " AND session_id=?"
		args = append(args, session)
	}
	if status != "" {
		query += " AND status=?"
		args = append(args, status)
	}
	if before := q.Get("before"); before != "" {
		cursor, err := s.log.record(before)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "invalid_request", "unknown before cursor", false)
			return
		}
		if err != nil {
			writeError(w, 500, "internal", err.Error(), false)
			return
		}
		query += " AND (created_at,id)<(?,?)"
		args = append(args, cursor.CreatedAt, cursor.ID)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.log.db.Query(query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	defer rows.Close()
	runs := []runRecord{}
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
			return
		}
		runs = append(runs, record)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}
