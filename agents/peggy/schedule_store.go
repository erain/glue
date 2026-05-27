package peggy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// scheduleFileVersion is the on-disk envelope version.
const scheduleFileVersion = 1

type scheduleFile struct {
	Version   int        `json:"version"`
	Schedules []Schedule `json:"schedules"`
}

// ScheduleStore persists schedules to a JSON file with atomic writes. It
// is safe for concurrent use. A zero path disables persistence (in-memory
// only), which keeps tests and disabled-scheduling callers simple.
type ScheduleStore struct {
	path string

	mu        sync.Mutex
	schedules map[string]Schedule
}

// OpenScheduleStore loads schedules from path, creating an empty store if
// the file is absent. An empty path yields an in-memory store.
func OpenScheduleStore(path string) (*ScheduleStore, error) {
	s := &ScheduleStore{path: path, schedules: map[string]Schedule{}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("peggy: read schedules: %w", err)
	}
	var file scheduleFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("peggy: parse schedules %s: %w", path, err)
	}
	for _, sched := range file.Schedules {
		if sched.ID == "" {
			continue
		}
		s.schedules[sched.ID] = sched
	}
	return s, nil
}

// List returns all schedules sorted by next run time, then id.
func (s *ScheduleStore) List() []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *ScheduleStore) listLocked() []Schedule {
	out := make([]Schedule, 0, len(s.schedules))
	for _, sched := range s.schedules {
		out = append(out, sched)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NextRun.Equal(out[j].NextRun) {
			return out[i].ID < out[j].ID
		}
		return out[i].NextRun.Before(out[j].NextRun)
	})
	return out
}

// Add stores a new schedule and persists.
func (s *ScheduleStore) Add(sched Schedule) error {
	if sched.ID == "" {
		return errors.New("peggy: schedule id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedules[sched.ID] = sched
	return s.saveLocked()
}

// Remove deletes a schedule by id and persists. Returns false if absent.
func (s *ScheduleStore) Remove(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.schedules[id]; !ok {
		return false, nil
	}
	delete(s.schedules, id)
	return true, s.saveLocked()
}

// Update replaces an existing schedule (e.g. after a run) and persists.
// It is a no-op if the id no longer exists, so a run completing after the
// schedule was removed does not resurrect it.
func (s *ScheduleStore) Update(sched Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.schedules[sched.ID]; !ok {
		return nil
	}
	s.schedules[sched.ID] = sched
	return s.saveLocked()
}

func (s *ScheduleStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	file := scheduleFile{Version: scheduleFileVersion, Schedules: s.listLocked()}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("peggy: encode schedules: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("peggy: schedules dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".schedules-*")
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
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
