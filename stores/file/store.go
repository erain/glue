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
	"strings"
	"time"

	"glue"
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
