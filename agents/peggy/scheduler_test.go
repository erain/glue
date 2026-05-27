package peggy

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustSchedule(t *testing.T, params NewScheduleParams, now time.Time) Schedule {
	t.Helper()
	s, err := NewSchedule(params, now)
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}
	return s
}

func TestNewScheduleValidation(t *testing.T) {
	now := time.Date(2026, 5, 27, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		params  NewScheduleParams
		wantErr string
	}{
		{"prompt requires text", NewScheduleParams{Kind: ScheduleKindPrompt, Every: time.Hour}, "needs a prompt"},
		{"skill requires name", NewScheduleParams{Kind: ScheduleKindSkill, Every: time.Hour}, "needs a skill"},
		{"needs a cadence", NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "x"}, "exactly one of every or at"},
		{"both cadences rejected", NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "x", Every: time.Hour, At: &now}, "exactly one of every or at"},
		{"interval floor", NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "x", Every: time.Second}, "below the"},
		{"prompt tier rejected", NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "x", Every: time.Hour, Tier: PermissionTierPrompt}, "no client to prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSchedule(tc.params, now)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewScheduleDefaultsTierAndSession(t *testing.T) {
	now := time.Date(2026, 5, 27, 9, 0, 0, 0, time.UTC)
	s := mustSchedule(t, NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "hi", Every: time.Hour}, now)
	if s.Tier != PermissionTierTrusted {
		t.Fatalf("tier = %q, want trusted (default)", s.Tier)
	}
	if s.SessionID != "schedule:"+s.ID {
		t.Fatalf("session = %q, want schedule:%s", s.SessionID, s.ID)
	}
	if !s.NextRun.Equal(now.Add(time.Hour)) {
		t.Fatalf("next run = %v, want now+1h", s.NextRun)
	}
}

func TestScheduleAdvanceRecurringFireForward(t *testing.T) {
	now := time.Date(2026, 5, 27, 9, 0, 0, 0, time.UTC)
	s := mustSchedule(t, NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "hi", Every: time.Hour}, now)
	// First run is due at now+1h; simulate the scheduler waking up 3h
	// late. The next run should jump forward past the missed ticks.
	late := now.Add(3*time.Hour + 30*time.Minute)
	advanced := s.advance(late, "")
	if !advanced.NextRun.After(late) {
		t.Fatalf("next run %v not after %v", advanced.NextRun, late)
	}
	if advanced.NextRun.Sub(now)%time.Hour != 0 {
		t.Fatalf("next run not aligned to interval: %v", advanced.NextRun)
	}
	if advanced.LastRun == nil || !advanced.LastRun.Equal(late) {
		t.Fatalf("last run = %v", advanced.LastRun)
	}
}

func TestScheduleOneShotBecomesDone(t *testing.T) {
	now := time.Date(2026, 5, 27, 9, 0, 0, 0, time.UTC)
	at := now.Add(time.Minute)
	s := mustSchedule(t, NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "hi", At: &at}, now)
	if !s.Due(at) {
		t.Fatal("one-shot not due at its time")
	}
	done := s.advance(at, "")
	if !done.Done() {
		t.Fatal("one-shot not Done after run")
	}
	if done.Due(at.Add(time.Hour)) {
		t.Fatal("done one-shot still due")
	}
}

func TestScheduleStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	store, err := OpenScheduleStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now()
	s := mustSchedule(t, NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "hi", Every: time.Hour}, now)
	if err := store.Add(s); err != nil {
		t.Fatalf("add: %v", err)
	}

	reopened, err := OpenScheduleStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.List()
	if len(got) != 1 || got[0].ID != s.ID {
		t.Fatalf("reloaded = %#v", got)
	}

	// Update on a missing id is a no-op (does not resurrect).
	if err := reopened.Update(Schedule{ID: "ghost", EverySeconds: 60}); err != nil {
		t.Fatalf("update ghost: %v", err)
	}
	if len(reopened.List()) != 1 {
		t.Fatal("ghost schedule resurrected")
	}

	removed, err := reopened.Remove(s.ID)
	if err != nil || !removed {
		t.Fatalf("remove: removed=%v err=%v", removed, err)
	}
	final, _ := OpenScheduleStore(path)
	if len(final.List()) != 0 {
		t.Fatalf("after remove = %#v", final.List())
	}
}

func TestSchedulerTickRunsDueAndAdvances(t *testing.T) {
	store, _ := OpenScheduleStore("") // in-memory
	base := time.Date(2026, 5, 27, 9, 0, 0, 0, time.UTC)
	recurring := mustSchedule(t, NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "tick", Every: time.Hour}, base)
	if err := store.Add(recurring); err != nil {
		t.Fatal(err)
	}

	var ran int32
	var gotTier PermissionTier
	var mu sync.Mutex
	run := func(_ context.Context, s Schedule) (string, error) {
		atomic.AddInt32(&ran, 1)
		mu.Lock()
		gotTier = s.Tier
		mu.Unlock()
		return "done", nil
	}

	clock := base.Add(2 * time.Hour) // schedule due (next was base+1h)
	sched := NewScheduler(store, run, WithSchedulerClock(func() time.Time { return clock }))
	sched.Tick(context.Background())
	sched.wg.Wait()

	if atomic.LoadInt32(&ran) != 1 {
		t.Fatalf("ran = %d, want 1", ran)
	}
	if gotTier != PermissionTierTrusted {
		t.Fatalf("run tier = %q, want trusted", gotTier)
	}
	after := store.List()[0]
	if !after.NextRun.After(clock) {
		t.Fatalf("next run %v not advanced past %v", after.NextRun, clock)
	}
	if after.LastRun == nil {
		t.Fatal("last run not recorded")
	}

	// Not due again at the same clock: a second tick must not re-run.
	sched.Tick(context.Background())
	sched.wg.Wait()
	if atomic.LoadInt32(&ran) != 1 {
		t.Fatalf("ran = %d after second tick, want 1", ran)
	}
}

func TestSchedulerRecordsRunError(t *testing.T) {
	store, _ := OpenScheduleStore("")
	base := time.Date(2026, 5, 27, 9, 0, 0, 0, time.UTC)
	s := mustSchedule(t, NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "boom", Every: time.Hour}, base)
	store.Add(s)
	run := func(_ context.Context, _ Schedule) (string, error) {
		return "", errors.New("kaboom")
	}
	sched := NewScheduler(store, run, WithSchedulerClock(func() time.Time { return base.Add(2 * time.Hour) }))
	sched.Tick(context.Background())
	sched.wg.Wait()
	got := store.List()[0]
	if got.LastError != "kaboom" {
		t.Fatalf("last error = %q, want kaboom", got.LastError)
	}
	if got.LastRun == nil {
		t.Fatal("errored run still advances cadence and records LastRun")
	}
}

func TestSchedulerOverlapGuard(t *testing.T) {
	store, _ := OpenScheduleStore("")
	base := time.Date(2026, 5, 27, 9, 0, 0, 0, time.UTC)
	s := mustSchedule(t, NewScheduleParams{Kind: ScheduleKindPrompt, Prompt: "slow", Every: time.Hour}, base)
	store.Add(s)

	release := make(chan struct{})
	var started int32
	run := func(_ context.Context, _ Schedule) (string, error) {
		atomic.AddInt32(&started, 1)
		<-release
		return "ok", nil
	}
	sched := NewScheduler(store, run, WithSchedulerClock(func() time.Time { return base.Add(2 * time.Hour) }))
	sched.Tick(context.Background()) // starts run, blocks on release
	sched.Tick(context.Background()) // same schedule still running → skip
	close(release)
	sched.wg.Wait()
	if got := atomic.LoadInt32(&started); got != 1 {
		t.Fatalf("started = %d, want 1 (overlap not guarded)", got)
	}
}
