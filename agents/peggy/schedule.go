package peggy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// MinScheduleInterval is the smallest recurring interval a schedule may
// use. It guards against runaway proactive runs.
const MinScheduleInterval = time.Minute

// ScheduleKind selects what a schedule runs.
type ScheduleKind string

const (
	ScheduleKindPrompt ScheduleKind = "prompt"
	ScheduleKindSkill  ScheduleKind = "skill"
)

// Schedule is one persisted proactive task. Exactly one of EverySeconds
// (recurring) or At (one-shot) is set. Tier is the permission posture the
// unattended run uses: "trusted" (default) or "read_only"; "prompt" is
// rejected because a scheduled run has no client to answer.
type Schedule struct {
	ID           string            `json:"id"`
	Kind         ScheduleKind      `json:"kind"`
	Prompt       string            `json:"prompt,omitempty"`
	Skill        string            `json:"skill,omitempty"`
	Args         map[string]string `json:"args,omitempty"`
	Tier         PermissionTier    `json:"tier"`
	SessionID    string            `json:"session_id"`
	EverySeconds int64             `json:"every_seconds,omitempty"`
	At           *time.Time        `json:"at,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	NextRun      time.Time         `json:"next_run"`
	LastRun      *time.Time        `json:"last_run,omitempty"`
	LastError    string            `json:"last_error,omitempty"`
}

// Recurring reports whether the schedule repeats on an interval.
func (s Schedule) Recurring() bool { return s.EverySeconds > 0 }

// interval returns the recurring period.
func (s Schedule) interval() time.Duration {
	return time.Duration(s.EverySeconds) * time.Second
}

// Done reports whether a one-shot schedule has already fired.
func (s Schedule) Done() bool {
	return !s.Recurring() && s.LastRun != nil
}

// Due reports whether the schedule should run at now.
func (s Schedule) Due(now time.Time) bool {
	if s.Done() {
		return false
	}
	return !s.NextRun.IsZero() && !s.NextRun.After(now)
}

// advance returns a copy updated after a run at now, recording the error
// (empty on success) and computing the next fire time. Recurring
// schedules fire-forward past any missed ticks; one-shot schedules
// become Done.
func (s Schedule) advance(now time.Time, runErr string) Schedule {
	out := s
	t := now
	out.LastRun = &t
	out.LastError = runErr
	if !s.Recurring() {
		return out
	}
	next := s.NextRun.Add(s.interval())
	for !next.After(now) {
		next = next.Add(s.interval())
	}
	out.NextRun = next
	return out
}

// describe returns a one-line human summary for CLI output.
func (s Schedule) describe() string {
	var when string
	if s.Recurring() {
		when = "every " + s.interval().String()
	} else if s.At != nil {
		when = "at " + s.At.Format(time.RFC3339)
	}
	what := s.Prompt
	if s.Kind == ScheduleKindSkill {
		what = "skill:" + s.Skill
	}
	what = strings.TrimSpace(what)
	if len(what) > 60 {
		what = what[:57] + "..."
	}
	return fmt.Sprintf("%s  [%s, %s]  %s", s.ID, when, s.Tier, what)
}

// NewScheduleParams is the input for building a validated Schedule.
type NewScheduleParams struct {
	Kind      ScheduleKind
	Prompt    string
	Skill     string
	Args      map[string]string
	Tier      PermissionTier
	SessionID string
	Every     time.Duration
	At        *time.Time
}

// NewSchedule validates params and returns a ready-to-store Schedule with
// a generated id and computed first NextRun. now is the creation time.
func NewSchedule(params NewScheduleParams, now time.Time) (Schedule, error) {
	tier := PermissionTier(normalizePermissionTier(string(params.Tier)))
	if tier == "" {
		tier = PermissionTierTrusted
	}
	switch tier {
	case PermissionTierTrusted, PermissionTierReadOnly:
		// ok
	case PermissionTierPrompt:
		return Schedule{}, fmt.Errorf("peggy: schedule tier %q is invalid: an unattended run has no client to prompt", tier)
	default:
		return Schedule{}, fmt.Errorf("peggy: invalid schedule tier %q", tier)
	}

	recurring := params.Every > 0
	oneShot := params.At != nil
	if recurring == oneShot {
		return Schedule{}, fmt.Errorf("peggy: schedule needs exactly one of every or at")
	}
	if recurring && params.Every < MinScheduleInterval {
		return Schedule{}, fmt.Errorf("peggy: schedule interval %s is below the %s minimum", params.Every, MinScheduleInterval)
	}

	s := Schedule{
		Kind:      params.Kind,
		Tier:      tier,
		Args:      params.Args,
		SessionID: strings.TrimSpace(params.SessionID),
		CreatedAt: now,
	}
	switch params.Kind {
	case ScheduleKindPrompt:
		s.Prompt = strings.TrimSpace(params.Prompt)
		if s.Prompt == "" {
			return Schedule{}, fmt.Errorf("peggy: prompt schedule needs a prompt")
		}
	case ScheduleKindSkill:
		s.Skill = strings.TrimSpace(params.Skill)
		if s.Skill == "" {
			return Schedule{}, fmt.Errorf("peggy: skill schedule needs a skill name")
		}
	default:
		return Schedule{}, fmt.Errorf("peggy: invalid schedule kind %q", params.Kind)
	}

	id, err := newScheduleID()
	if err != nil {
		return Schedule{}, err
	}
	s.ID = id
	if s.SessionID == "" {
		s.SessionID = "schedule:" + id
	}

	if recurring {
		s.EverySeconds = int64(params.Every / time.Second)
		s.NextRun = now.Add(params.Every)
	} else {
		at := params.At.UTC()
		s.At = &at
		s.NextRun = at
	}
	return s, nil
}

func newScheduleID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("peggy: schedule id: %w", err)
	}
	return "sch_" + hex.EncodeToString(b[:]), nil
}
