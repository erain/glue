package peggy

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/erain/glue"
)

// DefaultSchedulerInterval is how often the scheduler checks for due
// schedules when no interval is configured.
const DefaultSchedulerInterval = 30 * time.Second

// ScheduleRunner executes one due schedule and returns the assistant
// response text. The scheduler records any error on the schedule but does
// not stop the loop.
type ScheduleRunner func(ctx context.Context, s Schedule) (string, error)

// Scheduler fires due schedules from a store on an interval. It is the
// proactive engine behind `peggy serve`: it lets Peggy initiate runs
// (reminders, recurring tasks) instead of only responding.
type Scheduler struct {
	store    *ScheduleStore
	run      ScheduleRunner
	now      func() time.Time
	interval time.Duration
	logf     func(format string, args ...any)

	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

// SchedulerOption configures a Scheduler.
type SchedulerOption func(*Scheduler)

// WithSchedulerClock overrides the time source (for tests).
func WithSchedulerClock(now func() time.Time) SchedulerOption {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// WithSchedulerInterval overrides the tick interval.
func WithSchedulerInterval(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		if d > 0 {
			s.interval = d
		}
	}
}

// WithSchedulerLogger sets a log sink for fired-run summaries and errors.
func WithSchedulerLogger(logf func(format string, args ...any)) SchedulerOption {
	return func(s *Scheduler) {
		if logf != nil {
			s.logf = logf
		}
	}
}

// NewScheduler builds a scheduler over a store and run function.
func NewScheduler(store *ScheduleStore, run ScheduleRunner, options ...SchedulerOption) *Scheduler {
	s := &Scheduler{
		store:    store,
		run:      run,
		now:      time.Now,
		interval: DefaultSchedulerInterval,
		logf:     func(string, ...any) {},
		running:  map[string]bool{},
	}
	for _, opt := range options {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// Run ticks until ctx is cancelled, then waits for in-flight runs to
// finish. It returns ctx.Err().
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return ctx.Err()
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick dispatches every due schedule that is not already running. Each
// fires in its own goroutine so a slow run does not block the loop; an
// overlap guard skips a schedule whose previous run is still in flight.
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.now()
	for _, sched := range s.store.List() {
		if !sched.Due(now) {
			continue
		}
		if !s.tryStart(sched.ID) {
			continue
		}
		sched := sched
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.finish(sched.ID)
			s.execute(ctx, sched)
		}()
	}
}

func (s *Scheduler) tryStart(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[id] {
		return false
	}
	s.running[id] = true
	return true
}

func (s *Scheduler) finish(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
}

func (s *Scheduler) execute(ctx context.Context, sched Schedule) {
	text, err := s.run(ctx, sched)
	runErr := ""
	if err != nil {
		runErr = err.Error()
		s.logf("peggy schedule %s failed: %v", sched.ID, err)
	} else {
		s.logf("peggy schedule %s ran (%d chars)", sched.ID, len(text))
	}
	// Advance using the schedule's recorded fire time, not a fresh clock,
	// so recurring cadence stays stable across slow runs.
	if updateErr := s.store.Update(sched.advance(s.now(), runErr)); updateErr != nil {
		s.logf("peggy schedule %s persist failed: %v", sched.ID, updateErr)
	}
}

// ScheduleRunner returns a ScheduleRunner bound to this Peggy. Each run
// applies the schedule's own permission tier via a per-prompt permission
// override and writes into the schedule's session id; output is persisted
// in the session (visible via `peggy sessions` and recall).
func (p *Peggy) ScheduleRunner() ScheduleRunner {
	return func(ctx context.Context, s Schedule) (string, error) {
		perm := NewTieredPermission(nil, s.Tier, "schedule")
		opts := []glue.PromptOption{glue.WithPermission(perm)}
		if s.Kind == ScheduleKindSkill {
			args := map[string]string(nil)
			if len(s.Args) > 0 {
				args = s.Args
			}
			return p.SkillWithOptions(ctx, s.SessionID, s.Skill, args, io.Discard, opts...)
		}
		return p.PromptWithOptions(ctx, s.SessionID, s.Prompt, io.Discard, opts...)
	}
}
