// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

type notifySink struct {
	mu    sync.Mutex
	got   []Notification
	fails int
	ts    *httptest.Server
}

func newNotifySink(t *testing.T, fails int) *notifySink {
	s := &notifySink{fails: fails}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.fails > 0 {
			s.fails--
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		var n Notification
		_ = json.NewDecoder(r.Body).Decode(&n)
		s.got = append(s.got, n)
	}))
	t.Cleanup(s.ts.Close)
	return s
}

func (s *notifySink) wait(t *testing.T, n int) []Notification {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		s.mu.Lock()
		got := append([]Notification(nil), s.got...)
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
	}
	t.Fatalf("got %d notifications, want %d", len(s.got), n)
	return nil
}

func fastNotify(t *testing.T) {
	orig := notifyBackoff
	notifyBackoff = []time.Duration{0, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { notifyBackoff = orig })
}

func TestNotifyOnDoneAndWaiting(t *testing.T) {
	fastNotify(t)
	sink := newNotifySink(t, 1)
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) {
		return &scriptProvider{turns: [][]glue.ProviderEvent{
			turn(`{"items":[{"title":"write file"}]}`),
			{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w", Name: "write_file", Arguments: []byte(`{"path":"a.txt","content":"a\n"}`)}}, {Type: glue.ProviderEventDone}},
			turn("wrote it"),
			turn(`{"done":true,"items":[{"title":"write file","done":true,"evidence":"a.txt"}],"summary":"Add a.txt"}`),
		}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	m.SetCredentials(Credentials{NotifyURLs: []string{sink.ts.URL + "/hook"}})
	job, err := m.Create(context.Background(), Spec{Objective: "write a file", Repo: RepoSpec{Path: repoFixture(t)}, Policy: "ask-all"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	waiting := sink.wait(t, 1)[0]
	if waiting.Event != "waiting_for_input" || waiting.Status != WaitingForInput || !strings.Contains(waiting.Question, "write_file") || !strings.Contains(waiting.Text, "needs you") {
		t.Fatalf("waiting notification = %+v", waiting)
	}
	perms, err := m.Permissions(context.Background(), job.ID, "pending")
	if err != nil || len(perms) != 1 {
		t.Fatalf("pending = %v, %v", perms, err)
	}
	if err := m.Decide(context.Background(), job.ID, perms[0].ID, glue.PermissionDecision{Allow: true}); err != nil {
		t.Fatal(err)
	}
	waitJob(t, m, job.ID)
	got := sink.wait(t, 2)
	done := got[1]
	if done.Event != "done" || done.Status != Achieved || done.Summary != "Add a.txt" || !strings.Contains(done.Text, "achieved") {
		t.Fatalf("done notification = %+v", done)
	}
	if len(got) != 2 {
		t.Fatalf("notifications = %d, want exactly one per event", len(got))
	}
}

func TestNotifyURLRedacted(t *testing.T) {
	fastNotify(t)
	sink := newNotifySink(t, 0)
	secretURL := sink.ts.URL + "/hooks/T000/B000/xoxsecretpath"
	stateDir := t.TempDir()
	m, err := New(Options{StateDir: stateDir, ProviderFactory: func(string) (glue.Provider, error) {
		return &scriptProvider{turns: [][]glue.ProviderEvent{
			turn(`{"items":[{"title":"echo"}]}`),
			{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w", Name: "write_file", Arguments: []byte(`{"path":"url.txt","content":"` + secretURL + `\n"}`)}}, {Type: glue.ProviderEventDone}},
			turn("echoed " + secretURL),
			turn(`{"done":true,"items":[{"title":"echo","done":true,"evidence":"ok"}],"summary":"done"}`),
		}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	m.SetCredentials(Credentials{GitHubToken: "ghp_notifytesttoken1234567890", NotifyURLs: []string{secretURL}})
	m.SetCredentials(Credentials{GitHubToken: "ghp_notifytesttoken1234567890"})
	job, err := m.Create(context.Background(), Spec{Objective: "echo", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	waitJob(t, m, job.ID)
	sink.wait(t, 1)
	err = filepath.Walk(stateDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.Contains(p, string(filepath.Separator)+"worktrees"+string(filepath.Separator)) {
			return err
		}
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "xoxsecretpath") {
			t.Errorf("notify URL persisted in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNotifyFailureDoesNotFailJob(t *testing.T) {
	fastNotify(t)
	sink := newNotifySink(t, 100)
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) {
		return &scriptProvider{turns: [][]glue.ProviderEvent{
			turn(`{"items":[{"title":"look"}]}`),
			turn("looked"),
			turn(`{"done":true,"items":[{"title":"look","done":true,"evidence":"ok"}],"summary":"done"}`),
		}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	m.SetCredentials(Credentials{NotifyURLs: []string{sink.ts.URL}})
	job, _ := m.Create(context.Background(), Spec{Objective: "look", Repo: RepoSpec{Path: repoFixture(t)}})
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatalf("status = %s", got.Status)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		events, _ := m.Events(context.Background(), job.ID, 0)
		for _, ev := range events {
			if ev.Type == "notify_failed" {
				if raw, _ := json.Marshal(ev.Payload); strings.Contains(string(raw), sink.ts.URL) {
					t.Fatalf("notify_failed leaks the URL: %s", raw)
				}
				return
			}
		}
	}
	t.Fatal("no notify_failed event")
}

func TestValidNotifyURL(t *testing.T) {
	for _, ok := range []string{"https://ntfy.sh/glue-abc", "http://127.0.0.1:9/x"} {
		if err := ValidNotifyURL(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ntfy.sh/x", "ftp://x/y", "https://"} {
		if ValidNotifyURL(bad) == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
