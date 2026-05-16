package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
	"github.com/erain/glue"
)

// Options configures a Store.
type Options struct {
	// Path is the SQLite file path. ":memory:" is accepted and useful
	// in tests. Required.
	Path string

	// Timeout sets the SQLite busy_timeout. Default 5s.
	Timeout time.Duration
}

const defaultTimeout = 5 * time.Second

// Store implements glue.Store against a SQLite file with FTS5 over
// message text.
type Store struct {
	db *sql.DB
}

// Open creates or opens a SQLite store at opts.Path, applies WAL +
// related PRAGMAs, and runs the idempotent schema. It is safe to call
// Open multiple times against the same path from different processes;
// WAL mode permits concurrent reads with a single writer.
func Open(opts Options) (*Store, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("sqlite: Path is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	dsn := buildDSN(opts.Path, timeout)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	// modernc.org/sqlite supports concurrent reads through WAL; we
	// nevertheless want strict serialization of writes inside a single
	// process, so cap MaxOpenConns at 1 for now. A later optimization
	// can use a separate read pool.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func buildDSN(path string, timeout time.Duration) string {
	if path == ":memory:" {
		return ":memory:?_pragma=journal_mode(MEMORY)&_pragma=synchronous(OFF)&_pragma=foreign_keys(ON)"
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", timeout.Milliseconds()))
	return "file:" + path + "?" + q.Encode()
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Load implements glue.Store.
func (s *Store) Load(ctx context.Context, id string) (glue.SessionState, bool, error) {
	if s == nil || s.db == nil {
		return glue.SessionState{}, false, errors.New("sqlite: nil store")
	}
	if strings.TrimSpace(id) == "" {
		return glue.SessionState{}, false, errors.New("sqlite: session id is required")
	}

	row := s.db.QueryRowContext(ctx,
		`SELECT id, created_at, updated_at, COALESCE(metadata_json, '')
		 FROM sessions WHERE id = ?`, id)
	var (
		gotID       string
		createdAtUx int64
		updatedAtUx int64
		metaJSON    string
	)
	if err := row.Scan(&gotID, &createdAtUx, &updatedAtUx, &metaJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return glue.SessionState{}, false, nil
		}
		return glue.SessionState{}, false, fmt.Errorf("sqlite: load session %q: %w", id, err)
	}

	state := glue.SessionState{
		Version:   glue.SessionStateVersion,
		ID:        gotID,
		CreatedAt: time.Unix(createdAtUx, 0).UTC(),
		UpdatedAt: time.Unix(updatedAtUx, 0).UTC(),
	}
	if metaJSON != "" {
		if err := json.Unmarshal([]byte(metaJSON), &state.Metadata); err != nil {
			return glue.SessionState{}, false, fmt.Errorf("sqlite: load session %q metadata: %w", id, err)
		}
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT content_json FROM messages WHERE session_id = ? ORDER BY ord ASC`, id)
	if err != nil {
		return glue.SessionState{}, false, fmt.Errorf("sqlite: load messages for %q: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return glue.SessionState{}, false, fmt.Errorf("sqlite: scan message: %w", err)
		}
		var msg glue.Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			return glue.SessionState{}, false, fmt.Errorf("sqlite: decode message for %q: %w", id, err)
		}
		state.Messages = append(state.Messages, msg)
	}
	if err := rows.Err(); err != nil {
		return glue.SessionState{}, false, err
	}
	return state, true, nil
}

// Save implements glue.Store. Atomic: a single BEGIN IMMEDIATE
// transaction upserts the session row, deletes any prior messages for
// the id, and re-inserts the supplied messages in order.
func (s *Store) Save(ctx context.Context, id string, state glue.SessionState) error {
	if s == nil || s.db == nil {
		return errors.New("sqlite: nil store")
	}
	if strings.TrimSpace(id) == "" {
		return errors.New("sqlite: session id is required")
	}

	now := time.Now().UTC()
	if state.ID == "" {
		state.ID = id
	}
	if state.Version == 0 {
		state.Version = glue.SessionStateVersion
	}
	if state.CreatedAt.IsZero() {
		state.CreatedAt = now
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = now
	}

	metaJSON := ""
	if len(state.Metadata) > 0 {
		b, err := json.Marshal(state.Metadata)
		if err != nil {
			return fmt.Errorf("sqlite: marshal metadata: %w", err)
		}
		metaJSON = string(b)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id, created_at, updated_at, metadata_json)
		VALUES (?, ?, ?, NULLIF(?, ''))
		ON CONFLICT(id) DO UPDATE SET
		    updated_at    = excluded.updated_at,
		    metadata_json = NULLIF(excluded.metadata_json, '')
	`, id, state.CreatedAt.Unix(), state.UpdatedAt.Unix(), metaJSON); err != nil {
		return fmt.Errorf("sqlite: upsert session: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: clear messages: %w", err)
	}

	insertStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO messages (session_id, ord, role, content_text, content_json, ts)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("sqlite: prepare insert: %w", err)
	}
	defer insertStmt.Close()

	for i, m := range state.Messages {
		body, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("sqlite: marshal message %d: %w", i, err)
		}
		ts := m.CreatedAt
		if ts.IsZero() {
			ts = state.UpdatedAt
		}
		if _, err := insertStmt.ExecContext(ctx,
			id, i, string(m.Role), textForFTS(m.Content), string(body), ts.Unix()); err != nil {
			return fmt.Errorf("sqlite: insert message %d: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	committed = true
	return nil
}

// Delete implements glue.Store. Missing sessions are a no-op success.
// Foreign-key cascade removes the message rows; the FTS triggers
// remove their index rows.
func (s *Store) Delete(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return errors.New("sqlite: nil store")
	}
	if strings.TrimSpace(id) == "" {
		return errors.New("sqlite: session id is required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete session %q: %w", id, err)
	}
	return nil
}

// textForFTS concatenates the text content of a message's parts with
// "\n" separators. Tool calls, tool-call args, image content, and
// thinking are intentionally excluded from the FTS index — searches
// target what the user said and what the assistant said, not the
// shape of the call.
func textForFTS(parts []glue.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type != glue.ContentTypeText || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// DB returns the underlying *sql.DB for tests and future Searcher
// implementations that need direct query access. Public consumers
// should treat the returned handle as read-only outside of the
// Store's Save / Load / Delete API.
func (s *Store) DB() *sql.DB { return s.db }
