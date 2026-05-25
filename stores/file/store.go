// Package file provides a local JSON-backed session store for Glue.
//
// Each session is written to <dir>/<url-escaped-id>.json. Saves are atomic:
// the JSON payload is first written to a sibling temp file in the same
// directory and then [os.Rename]'d into place, so concurrent readers either
// see the previous file or the new file but never a partial write.
package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/erain/glue"
)

// Store persists Glue sessions as local JSON files.
type Store struct {
	dir string
}

// New creates a file-backed store rooted at dir. The directory is created on
// first save; calling New does not touch the filesystem.
func New(dir string) *Store {
	return &Store{dir: dir}
}

// Load reads a session state. Missing sessions return found=false with no
// error.
func (s *Store) Load(_ context.Context, id string) (glue.SessionState, bool, error) {
	path, err := s.path(id)
	if err != nil {
		return glue.SessionState{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return glue.SessionState{}, false, nil
	}
	if err != nil {
		return glue.SessionState{}, false, err
	}

	var state glue.SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return glue.SessionState{}, false, fmt.Errorf("file store: load %q: %w", id, err)
	}
	if state.ID == "" {
		state.ID = id
	}
	if state.Version == 0 {
		state.Version = glue.SessionStateVersion
	}
	return state, true, nil
}

// Save writes a session state atomically.
func (s *Store) Save(_ context.Context, id string, state glue.SessionState) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
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

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Delete removes a session state. Missing sessions are a no-op success.
func (s *Store) Delete(_ context.Context, id string) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ListSessions implements glue.SessionLister by scanning JSON session files.
func (s *Store) ListSessions(ctx context.Context, opts glue.ListSessionsOptions) ([]glue.SessionSummary, error) {
	if s == nil {
		return nil, errors.New("file store: nil store")
	}
	if strings.TrimSpace(s.dir) == "" {
		return nil, errors.New("file store: directory is required")
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []glue.SessionSummary{}, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSpace(opts.Prefix)
	summaries := make([]glue.SessionSummary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id, err := url.PathUnescape(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, fmt.Errorf("file store: decode session file %s: %w", entry.Name(), err)
		}
		if prefix != "" && !strings.HasPrefix(id, prefix) {
			continue
		}
		state, found, err := s.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		summaries = append(summaries, sessionSummaryFromState(state))
	}
	sort.Slice(summaries, func(i, j int) bool {
		if !summaries[i].UpdatedAt.Equal(summaries[j].UpdatedAt) {
			return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
		}
		return summaries[i].ID < summaries[j].ID
	})
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= len(summaries) {
		return []glue.SessionSummary{}, nil
	}
	summaries = summaries[offset:]
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if len(summaries) > limit {
		summaries = summaries[:limit]
	}
	return summaries, nil
}

func sessionSummaryFromState(state glue.SessionState) glue.SessionSummary {
	summary := glue.SessionSummary{
		ID:        state.ID,
		CreatedAt: state.CreatedAt,
		UpdatedAt: state.UpdatedAt,
		Messages:  len(state.Messages),
	}
	for _, message := range state.Messages {
		switch message.Role {
		case glue.MessageRoleUser:
			summary.UserMessages++
		case glue.MessageRoleAssistant:
			summary.AssistantMessages++
		}
	}
	return summary
}

// Path returns the JSON file path for a session id. It is exposed for tests
// and debugging; production code should not rely on it.
func (s *Store) Path(id string) (string, error) {
	return s.path(id)
}

func (s *Store) path(id string) (string, error) {
	if s == nil {
		return "", errors.New("file store: nil store")
	}
	if strings.TrimSpace(s.dir) == "" {
		return "", errors.New("file store: directory is required")
	}
	if strings.TrimSpace(id) == "" {
		return "", errors.New("file store: session id is required")
	}
	return filepath.Join(s.dir, url.PathEscape(id)+".json"), nil
}

var _ glue.SessionLister = (*Store)(nil)
