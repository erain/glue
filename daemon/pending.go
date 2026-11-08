package daemon

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const permissionSchema = `CREATE TABLE IF NOT EXISTS permissions (
 run_id TEXT NOT NULL,id TEXT NOT NULL,request_json TEXT NOT NULL,
 created_at TEXT NOT NULL,expires_at TEXT NOT NULL,state TEXT NOT NULL,
 PRIMARY KEY(run_id,id));`

type permissionRecord struct {
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	Request   json.RawMessage `json:"request"`
	CreatedAt string          `json:"created_at"`
	ExpiresAt string          `json:"expires_at"`
	State     string          `json:"state"`
}

func (l *eventLog) permissions(runID, state string) ([]permissionRecord, error) {
	query := "SELECT id,run_id,request_json,created_at,expires_at,state FROM permissions WHERE run_id=?"
	args := []any{runID}
	if state != "" {
		query += " AND state=?"
		args = append(args, state)
	}
	rows, err := l.db.Query(query+" ORDER BY created_at,id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []permissionRecord{}
	for rows.Next() {
		var r permissionRecord
		var data string
		if err := rows.Scan(&r.ID, &r.RunID, &data, &r.CreatedAt, &r.ExpiresAt, &r.State); err != nil {
			return nil, err
		}
		r.Request = json.RawMessage(data)
		result = append(result, r)
	}
	return result, rows.Err()
}
func storePermission(tx *sql.Tx, e EventEnvelope) error {
	data, err := json.Marshal(e.Payload)
	if err != nil {
		return err
	}
	var p permissionRequestPayload
	if err = json.Unmarshal(data, &p); err != nil {
		return err
	}
	request, err := json.Marshal(p.Request)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO permissions(run_id,id,request_json,created_at,expires_at,state) VALUES(?,?,?,?,?,'pending')", e.RunID, p.PermissionID, string(request), timestamp(e.Time), timestamp(p.ExpiresAt))
	return err
}
func (l *eventLog) transitionPermission(runID, id, state string, now time.Time) (bool, error) {
	query := "UPDATE permissions SET state=? WHERE run_id=? AND id=? AND state='pending'"
	args := []any{state, runID, id}
	if state != "expired" {
		query += " AND expires_at>?"
		args = append(args, timestamp(now))
	}
	result, err := l.db.Exec(query, args...)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
func (s *Server) handleRunPermissions(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := s.log.record(id); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "run not found", false)
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	state := r.URL.Query().Get("state")
	switch state {
	case "", "pending", "allowed", "denied", "expired":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid permission state", false)
		return
	}
	records, err := s.log.permissions(id, state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"permissions": records})
}
func parseRunPermissionsPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 4 && parts[0] == "v1" && parts[1] == "runs" && parts[2] != "" && parts[3] == "permissions" {
		return parts[2], true
	}
	return "", false
}
