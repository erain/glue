package daemon

import (
	"database/sql"
	"errors"
	"github.com/erain/glue"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetentionSweepsOnlyOldTerminalRuns(t *testing.T) {
	s := eventServer(t, t.TempDir())
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now.Add(-721 * time.Hour) }
	old := s.newRun("session", "client", func() {})
	old.emit("permission_request", permissionRequestPayload{PermissionID: "p", ExpiresAt: now.Add(-720 * time.Hour)})
	old.emit("run_done", nil)
	old.finish()
	var terminalIDs []string
	for _, typ := range []string{"run_error", "run_interrupted"} {
		r := s.newRun("session", "client", func() {})
		r.emit(typ, nil)
		r.finish()
		terminalIDs = append(terminalIDs, r.id)
	}
	running := s.newRun("session", "client", func() {})
	running.emit("run_start", nil)
	s.now = func() time.Time { return now.Add(-720 * time.Hour) }
	boundary := finishedEvents(t, s)
	s.now = func() time.Time { return now }
	recent := finishedEvents(t, s)
	if err := s.sweepRuns(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.log.record(old.id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old row: %v", err)
	}
	for _, id := range terminalIDs {
		if _, err := s.log.record(id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("terminal %s remains: %v", id, err)
		}
	}
	if s.getRun(old.id) != nil {
		t.Fatal("old live index entry remains")
	}
	events, err := s.log.read(old.id, 0)
	if err != nil || len(events) != 0 {
		t.Fatalf("events %v err %v", events, err)
	}
	permissions, err := s.log.permissions(old.id, "")
	if err != nil || len(permissions) != 0 {
		t.Fatalf("permissions %v err %v", permissions, err)
	}
	for _, id := range []string{running.id, boundary.id, recent.id} {
		if _, err := s.log.record(id); err != nil {
			t.Fatalf("retained %s: %v", id, err)
		}
	}
}

func TestRetentionBackgroundSweepAndClose(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s, err := New(Options{Host: sessionOnlyHost{}, Token: "token", StateDir: t.TempDir(), RetainRuns: time.Hour, SweepInterval: 5 * time.Millisecond, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := finishedEvents(t, s)
	clock.Add(int64(2 * time.Hour))
	deadline := time.Now().Add(time.Second)
	for {
		_, err := s.log.record(r.id)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("sweep did not delete: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.log.db.Ping(); err == nil {
		t.Fatal("database remains open")
	}
}
func TestRetentionCloseInterruptsActiveRun(t *testing.T) {
	dir := t.TempDir()
	p := &blockingProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	s, err := New(Options{Host: glue.NewAgent(glue.AgentOptions{Provider: p}), Token: "token", StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	start := startRun(t, ts.URL, "default")
	<-p.started
	ts.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	other := eventServer(t, dir)
	record, err := other.log.record(start.RunID)
	if err != nil || record.Status != "interrupted" {
		t.Fatalf("record %v err %v", record, err)
	}
	events, err := other.log.read(start.RunID, 0)
	if err != nil || events[len(events)-1].Type != "run_interrupted" {
		t.Fatalf("events %v err %v", events, err)
	}
}

func TestRetentionDeletionRollsBackOnFailure(t *testing.T) {
	s := eventServer(t, t.TempDir())
	s.now = func() time.Time { return time.Now().Add(-800 * time.Hour) }
	r := finishedEvents(t, s)
	s.now = time.Now
	if _, err := s.log.db.Exec("DROP TABLE permissions"); err != nil {
		t.Fatal(err)
	}
	if err := s.sweepRuns(); err == nil {
		t.Fatal("expected deletion error")
	}
	events, err := s.log.read(r.id, 0)
	if err != nil || len(events) == 0 || s.getRun(r.id) == nil {
		t.Fatalf("events %v err %v", events, err)
	}
}
