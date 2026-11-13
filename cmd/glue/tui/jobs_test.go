// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeJobs struct {
	rows   []JobRow
	pulled []string
}

func (f *fakeJobs) List(context.Context) ([]JobRow, error) { return f.rows, nil }
func (f *fakeJobs) Transcript(_ context.Context, id string) (string, error) {
	return "[00:01] plan: 2 items\n[00:09] job achieved — " + id + " done\n", nil
}
func (f *fakeJobs) Pull(_ context.Context, id string) (string, error) {
	f.pulled = append(f.pulled, id)
	return "fetched glue/" + id + "; on branch glue/" + id + "\n", nil
}

// runCmd runs a command and feeds every resulting message back into the
// model, skipping ticks so tests do not wait.
func runCmd(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			runCmd(t, m, c)
		}
	case nil, jobsTickMsg, jobsCountTickMsg:
	default:
		_, next := m.Update(msg)
		runCmd(t, m, next)
	}
}

func jobsModel() (*Model, *fakeJobs) {
	f := &fakeJobs{rows: []JobRow{
		{ID: "job-00000000000a", Status: "running", Objective: "fix the flaky test", Updated: time.Now().Add(-time.Minute)},
		{ID: "job-00000000000b", Status: "waiting_for_input", Objective: "add Shout", Updated: time.Now()},
	}}
	m := newModel(context.Background(), Config{Jobs: f})
	m.width = 120
	m.tick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	return m, f
}

func keyOf(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func TestJobsPanelList(t *testing.T) {
	m, _ := jobsModel()
	cmd, _ := parseSlashCommand("/jobs")
	_, next := m.handleSlash(cmd)
	runCmd(t, m, next)
	if m.jobs == nil || len(m.jobs.rows) != 2 {
		t.Fatalf("panel = %+v", m.jobs)
	}
	view := renderJobsPanel(m.jobs, 120, time.Now())
	for _, want := range []string{"Remote jobs", "job-00000000000a", "running", "fix the flaky test", "job-00000000000b", "waiting_for_input"} {
		if !strings.Contains(view, want) {
			t.Fatalf("panel lacks %q:\n%s", want, view)
		}
	}
	if seg := m.jobsStatusSegment(); !strings.Contains(seg, "2 remote jobs · 1 waiting") {
		t.Fatalf("status segment = %q", seg)
	}
	m.handleJobsKey(keyOf(tea.KeyEsc))
	if m.jobs != nil {
		t.Fatal("Esc did not close the panel")
	}
	if seg := m.jobsStatusSegment(); seg == "" {
		t.Fatal("the status bar count went away with the panel")
	}
	none := newModel(context.Background(), Config{})
	if seg := none.jobsStatusSegment(); seg != "" {
		t.Fatalf("no jobs, segment %q", seg)
	}
}

func TestJobsPanelTranscript(t *testing.T) {
	m, _ := jobsModel()
	cmd, _ := parseSlashCommand("/jobs")
	_, next := m.handleSlash(cmd)
	runCmd(t, m, next)
	_, next = m.handleJobsKey(keyOf(tea.KeyDown))
	runCmd(t, m, next)
	_, next = m.handleJobsKey(keyOf(tea.KeyEnter))
	runCmd(t, m, next)
	if m.jobs.viewing != "job-00000000000b" || !strings.Contains(m.jobs.transcript, "job achieved — job-00000000000b done") {
		t.Fatalf("viewing %q transcript %q", m.jobs.viewing, m.jobs.transcript)
	}
	if view := renderJobsPanel(m.jobs, 120, time.Now()); !strings.Contains(view, "Job job-00000000000b") || !strings.Contains(view, "plan: 2 items") {
		t.Fatalf("transcript view:\n%s", view)
	}
	// A tick from the list view (older generation) is ignored.
	if c, _ := m.updateJobs(jobsTickMsg{gen: m.jobs.gen - 1}); c != nil {
		t.Fatal("stale tick scheduled work")
	}
	_, next = m.handleJobsKey(keyOf(tea.KeyEsc))
	runCmd(t, m, next)
	if m.jobs == nil || m.jobs.viewing != "" {
		t.Fatalf("Esc should return to the list: %+v", m.jobs)
	}
}

func TestJobsPanelPull(t *testing.T) {
	m, f := jobsModel()
	cmd, _ := parseSlashCommand("/jobs")
	_, next := m.handleSlash(cmd)
	runCmd(t, m, next)
	_, next = m.handleJobsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	runCmd(t, m, next)
	if len(f.pulled) != 1 || f.pulled[0] != "job-00000000000a" {
		t.Fatalf("pulled = %v", f.pulled)
	}
	last := m.transcript[len(m.transcript)-1]
	if !strings.Contains(last.Text, "pull job-00000000000a: fetched glue/job-00000000000a") {
		t.Fatalf("transcript ends with %+v", last)
	}
}
