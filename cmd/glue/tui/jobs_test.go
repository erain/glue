// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package tui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/erain/glue"
)

type fakeJobs struct {
	rows                  []JobRow
	pulled, calls         []string
	details               map[string]JobDetail
	logs, diffs           map[string]string
	pr                    string
	detailCalls, logCalls int
	teleport              func(context.Context, string) (TeleportResult, error)
}

func (f *fakeJobs) List(context.Context) ([]JobRow, error) { return f.rows, nil }

func (f *fakeJobs) rec(s string, a ...any) error {
	f.calls = append(f.calls, fmt.Sprintf(s, a...))
	return nil
}
func (f *fakeJobs) Diff(_ context.Context, id string) (string, error) { return f.diffs[id], nil }
func (f *fakeJobs) Cancel(_ context.Context, id string) error         { return f.rec("cancel:%s", id) }
func (f *fakeJobs) OpenPR(_ context.Context, s string) (string, error) {
	return f.pr, f.rec("pr:%s", s)
}
func (f *fakeJobs) Answer(_ context.Context, a, b, c string) error { return f.rec("%s:%s:%s", a, b, c) }
func (f *fakeJobs) Say(_ context.Context, a, b string, c bool) error {
	return f.rec("%s:%t:%s", a, c, b)
}
func (f *fakeJobs) DecidePermission(_ context.Context, id, p string, ok bool, r string) error {
	return f.rec("perm:%s:%s:%t:%s", id, p, ok, r)
}
func (f *fakeJobs) Detail(_ context.Context, id string) (JobDetail, error) {
	f.detailCalls++
	return f.details[id], nil
}
func (f *fakeJobs) Transcript(_ context.Context, id string) (string, error) {
	f.logCalls++
	return cmp.Or(f.logs[id], "[00:01] plan: 2 items\n[00:09] job achieved — "+id+" done\n"), nil
}
func (f *fakeJobs) Pull(_ context.Context, id string, apply bool) (string, error) {
	f.calls, f.pulled = append(f.calls, fmt.Sprintf("pull:%s:%t", id, apply)), append(f.pulled, id)
	return pick(apply, "applied patch from "+id+"\n", "fetched glue/"+id+"; on branch glue/"+id+"\n"), nil
}
func (f *fakeJobs) Teleport(ctx context.Context, id string) (TeleportResult, error) {
	if _ = f.rec("teleport:%s", id); f.teleport != nil {
		return f.teleport(ctx, id)
	}
	return TeleportResult{SessionID: "teleport-" + strings.TrimPrefix(id, "job-")}, nil
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
	}, details: map[string]JobDetail{}, logs: map[string]string{}, diffs: map[string]string{}, pr: "https://github.com/erain/glue/pull/99"}
	m := newModel(context.Background(), Config{Jobs: f})
	m.width = 120
	m.tick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	return m, f
}

func keyOf(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func sendJobs(t *testing.T, m *Model, keys ...string) {
	t.Helper()
	sp := map[string]tea.KeyType{"Enter": tea.KeyEnter, "Esc": tea.KeyEsc, "Down": tea.KeyDown, "PgUp": tea.KeyPgUp, "BS": tea.KeyBackspace, " ": tea.KeySpace}
	for _, k := range keys {
		var next tea.Cmd
		if k == "/jobs" {
			c, _ := parseSlashCommand(k)
			_, next = m.handleSlash(c)
		} else if k == ":tick" {
			next, _ = m.updateJobs(jobsTickMsg{gen: m.jobs.gen})
		} else if k == ":count" {
			next, _ = m.updateJobs(jobsCountTickMsg{})
		} else {
			typ, ok := sp[k]
			_, next = m.handleJobsKey(pick(ok, keyOf(typ), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}))
		}
		runCmd(t, m, next)
	}
}

func stepJobsView(t *testing.T, m *Model, keys []string, forbid string, wants ...string) {
	t.Helper()
	sendJobs(t, m, keys...)
	v := renderJobsPanel(m.jobs, 120, time.Now())
	if (forbid != "" && strings.Contains(v, forbid)) || slices.ContainsFunc(wants, func(w string) bool { return !strings.Contains(v, w) }) {
		t.Fatalf("view (wants=%v forbid=%q):\n%s", wants, forbid, v)
	}
}

func assertCalls(t *testing.T, f *fakeJobs, want ...string) {
	t.Helper()
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
}

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

func TestJobsPanelScrollAndDiffToggle(t *testing.T) {
	m, f := jobsModel()
	f.rows = append(f.rows, JobRow{ID: "job-00000000000c", Status: "suspended", Objective: "idle"})
	for i := range 30 {
		f.logs["job-00000000000a"] += fmt.Sprintf("line-%02d\n", i+1)
	}
	f.diffs["job-00000000000a"] = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-oldLine\n+newLine\n"
	sendJobs(t, m, "/jobs", ":tick", ":count")
	if f.detailCalls != 0 || f.logCalls != 0 {
		t.Fatalf("background ticks woke tasks: detail=%d logs=%d", f.detailCalls, f.logCalls)
	}
	stepJobsView(t, m, []string{"Enter"}, "line-01", "line-30")
	f.logs["job-00000000000a"] += "line-31\n"
	stepJobsView(t, m, []string{"k", "PgUp", "g", ":tick"}, "line-31", "line-01")
	f.logs["job-00000000000a"] += "line-32\n"
	stepJobsView(t, m, []string{"G", ":tick"}, "", "line-32")
	stepJobsView(t, m, []string{"d"}, "", "[diff]", "+newLine", "-oldLine")
	stepJobsView(t, m, []string{"d"}, "[diff]", "line-32")
}

func TestJobsPanelAnswerQuestionOptionsAndText(t *testing.T) {
	m, f := jobsModel()
	f.details["job-00000000000b"] = JobDetail{ID: "job-00000000000b", Status: "waiting_for_input", Question: &JobQuestion{ID: "q-1", Question: "Which store?", Options: []string{"SQLite", "Postgres"}}}
	stepJobsView(t, m, []string{"/jobs", "Down", "Enter"}, "", "Which store?", "[1] SQLite  [2] Postgres", "1-2/a answer")
	sendJobs(t, m, "2", "a", "customX", "BS", " ", "db", "Enter")
	assertCalls(t, f, "job-00000000000b:q-1:Postgres", "job-00000000000b:q-1:custom db")
}

func TestJobsPanelPermissionDecision(t *testing.T) {
	m, f := jobsModel()
	f.details["job-00000000000b"] = JobDetail{ID: "job-00000000000b", Status: "waiting_for_input", Permissions: []JobPermission{{ID: "perm-1", Tool: "shell", Action: "exec", Target: "make test"}}}
	stepJobsView(t, m, []string{"/jobs", "Down", "Enter"}, "s say", "y allow · s/S session · n deny")
	sendJobs(t, m, "y", "s", "S", "n")
	assertCalls(t, f, "perm:job-00000000000b:perm-1:true:", "perm:job-00000000000b:perm-1:true:session", "perm:job-00000000000b:perm-1:true:session", "perm:job-00000000000b:perm-1:false:")
}

func TestJobsPanelSayAndInterrupt(t *testing.T) {
	m, f := jobsModel()
	sendJobs(t, m, "/jobs", "Enter", "s", "check", " ", "edge", "Enter", "i", "stop now", "Enter")
	assertCalls(t, f, "job-00000000000a:false:check edge", "job-00000000000a:true:stop now")
}

func TestJobsPanelOpenPRAndCancelConfirm(t *testing.T) {
	m, f := jobsModel()
	f.rows = append(f.rows, JobRow{ID: "job-00000000000c", Status: "achieved", Objective: "ship feature"})
	f.details["job-00000000000c"] = JobDetail{ID: "job-00000000000c", Status: "achieved", Summary: "Shipped feature"}
	stepJobsView(t, m, []string{"/jobs", "Enter", "c"}, "", "Cancel job-00000000000a? (y/N)")
	stepJobsView(t, m, []string{"n", "c", "y", "Esc", "Down", "Down", "Enter", "o"}, "", "draft PR: https://github.com/erain/glue/pull/99")
	assertCalls(t, f, "cancel:job-00000000000a", "pr:job-00000000000c")
}

func TestJobsPanelApplyVsBranchPull(t *testing.T) {
	m, f := jobsModel()
	sendJobs(t, m, "/jobs", "p", "A")
	assertCalls(t, f, "pull:job-00000000000a:false", "pull:job-00000000000a:true")
}

func TestTUISlashTeleportAndJobsKeyTSwitchesSession(t *testing.T) {
	st := newMemStore()
	_, f := jobsModel()
	f.teleport = func(ctx context.Context, id string) (TeleportResult, error) {
		sid := "teleport-" + strings.TrimPrefix(id, "job-")
		_ = st.Save(ctx, sid, glue.SessionState{Version: glue.SessionStateVersion, ID: sid, Messages: []glue.Message{textMsg(glue.MessageRoleUser, "Handoff "+id), textMsg(glue.MessageRoleAssistant, "Remote job handoff "+id+": [x] verify suite green")}})
		return TeleportResult{SessionID: sid, Notice: "switched to glue/" + id}, nil
	}
	m := newModel(context.Background(), Config{Agent: glue.NewAgent(glue.AgentOptions{Provider: fakeProvider{}, Store: st}), Store: st, SessionID: "tui:init", Jobs: f, PublicURL: "https://glue.example.com/", SubmitRemote: func(context.Context, RemoteRequest, func(string)) (string, error) { return "job-00000000000a", nil }})
	m.tick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	slash := func(line string) string {
		c, _ := parseSlashCommand(line)
		_, next := m.handleSlash(c)
		runCmd(t, m, next)
		return stripANSI(m.View())
	}
	if v := slash("/remote ship feature"); !strings.Contains(v, "https://glue.example.com/#/jobs/job-00000000000a") {
		t.Fatalf("/remote missing browser permalink:\n%s", v)
	}
	if v := slash("/teleport job-00000000000a"); m.cfg.SessionID != "teleport-00000000000a" || !strings.Contains(v, "Remote job handoff job-00000000000a") {
		t.Fatalf("after /teleport session=%q view:\n%s", m.cfg.SessionID, v)
	}
	stepJobsView(t, m, []string{"/jobs", "Down"}, "", "t teleport")
	if sendJobs(t, m, "t"); m.jobs != nil || m.cfg.SessionID != "teleport-00000000000b" || !strings.Contains(stripANSI(m.View()), "Remote job handoff job-00000000000b") {
		t.Fatalf("after t in /jobs panel=%+v session=%q", m.jobs, m.cfg.SessionID)
	}
}
