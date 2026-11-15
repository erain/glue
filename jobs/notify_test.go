// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

func TestNotifyNtfyPlainText(t *testing.T) {
	var gotBody, gotTitle, gotType string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotTitle, gotType = string(b), r.Header.Get("Title"), r.Header.Get("Content-Type")
	}))
	t.Cleanup(ts.Close)
	if isNtfy(ts.URL) || !isNtfy("https://ntfy.sh/topic") || !isNtfy("https://ntfy.example.org/t") || isNtfy("https://hooks.slack.com/x") {
		t.Fatal("isNtfy misclassifies URLs")
	}
	n := Notification{JobID: "job-1", Status: Achieved, Text: "Glue job job-1 achieved: done"}
	if err := postNotification(context.Background(), ts.URL, n); err != nil || !strings.HasPrefix(gotType, "application/json") || !strings.Contains(gotBody, `"text":"Glue job job-1 achieved: done"`) || gotTitle != "" {
		t.Fatalf("json post: %v type=%q body=%q title=%q", err, gotType, gotBody, gotTitle)
	}
}

func TestNotifyDeliveredBeforeClose(t *testing.T) {
	orig := notifyBackoff
	notifyBackoff = []time.Duration{0, 300 * time.Millisecond, 300 * time.Millisecond}
	t.Cleanup(func() { notifyBackoff = orig })
	sink := newNotifySink(t, 1) // the first attempt fails, so delivery is still retrying at Close
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
	m.SetCredentials(Credentials{NotifyURLs: []string{sink.ts.URL}})
	job, _ := m.Create(context.Background(), Spec{Objective: "look", Repo: RepoSpec{Path: repoFixture(t)}})
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	waitJob(t, m, job.ID)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	got := len(sink.got)
	sink.mu.Unlock()
	if got != 1 {
		t.Fatalf("notifications delivered by the time Close returned = %d, want 1", got)
	}
}

type ntfyPost struct{ title, click, actions, body string }
type ntfyHub struct {
	mu         sync.Mutex
	posts      []ntfyPost
	msgs       map[string][]string
	open, drop int
	*httptest.Server
}

func checkNtfy(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

func newNtfyHub(t *testing.T, drop int) *ntfyHub {
	fastNotify(t)
	orig := ntfyReplyBackoff
	ntfyReplyBackoff = 5 * time.Millisecond
	t.Cleanup(func() { ntfyReplyBackoff = orig })
	h := &ntfyHub{msgs: map[string][]string{}, drop: drop}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topic, rest, _ := strings.Cut(strings.Trim(r.URL.Path, "/"), "/")
		if r.Method != http.MethodGet || rest != "json" {
			b, _ := io.ReadAll(r.Body)
			h.mu.Lock()
			h.msgs[topic] = append(h.msgs[topic], string(b))
			if topic == "t" {
				h.posts = append(h.posts, ntfyPost{r.Header.Get("Title"), r.Header.Get("Click"), r.Header.Get("Actions"), string(b)})
			}
			h.mu.Unlock()
			return
		}
		h.mu.Lock()
		if h.drop > 0 {
			h.drop--
			h.mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		h.open++
		h.mu.Unlock()
		defer func() { h.mu.Lock(); h.open--; h.mu.Unlock() }()
		for sent := 0; r.Context().Err() == nil; time.Sleep(2 * time.Millisecond) {
			h.mu.Lock()
			for ; sent < len(h.msgs[topic]); sent++ {
				_ = json.NewEncoder(w).Encode(map[string]any{"event": "message", "time": 2, "message": h.msgs[topic][sent]})
			}
			h.mu.Unlock()
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *ntfyHub) wait(t *testing.T, n int) (got []ntfyPost) {
	t.Helper()
	for i := 0; i < 1000 && len(got) < n; i++ {
		time.Sleep(5 * time.Millisecond)
		h.mu.Lock()
		got = append([]ntfyPost(nil), h.posts...)
		h.mu.Unlock()
	}
	checkNtfy(t, len(got) >= n, "got %d ntfy posts, want %d", len(got), n)
	return got
}

func ntfyButtons(hdr string) (out [][3]string) {
	for head, after, ok := strings.Cut(hdr, ", method=POST, body='"); ok; head, after, ok = strings.Cut(hdr, ", method=POST, body='") {
		body, tail, _ := strings.Cut(after, "', clear=true")
		hdr = strings.TrimLeft(tail, "; ")
		i := strings.LastIndex(head, ", http")
		lbl := strings.TrimSpace(strings.TrimPrefix(head[:i], "http,"))
		if u, err := strconv.Unquote(lbl); err == nil && strings.HasPrefix(lbl, `"`) {
			lbl = u
		}
		out = append(out, [3]string{lbl, strings.TrimSpace(head[i+2:]), body})
	}
	return out
}

func tapNtfyButton(t *testing.T, hdr, want string) {
	t.Helper()
	for _, b := range ntfyButtons(hdr) {
		if b[0] == want {
			resp, err := http.Post(b[1], "application/json", strings.NewReader(b[2]))
			checkNtfy(t, err == nil && resp.Body.Close() == nil && resp.StatusCode < 300, "tap %q: %v %v", want, err, resp)
			return
		}
	}
	t.Fatalf("button %q not found in %q", want, hdr)
}

func newNtfyJob(t *testing.T, hub *ntfyHub, pub, policy string, turns ...[]glue.ProviderEvent) (*Manager, *recordingScript, Job) {
	t.Helper()
	p := &recordingScript{last: make(chan string, 64), scriptProvider: scriptProvider{turns: turns}}
	m := setupManager(t, p)
	m.SetCredentials(Credentials{PublicURL: pub, NotifyURLs: []string{hub.URL + "/t?ntfy=1"}})
	job, err := m.Create(context.Background(), Spec{Objective: "ntfy test", Repo: RepoSpec{Path: repoFixture(t)}, Policy: policy})
	checkNtfy(t, err == nil && m.Start(context.Background(), job.ID) == nil, "create/start: %v", err)
	return m, p, job
}

func drainLast(p *recordingScript) (s string) {
	for len(p.last) > 0 {
		s += <-p.last + "\n"
	}
	return s
}

func hasSourceNtfy(m *Manager, id, typ string) bool {
	evs, _ := m.Events(context.Background(), id, 0)
	raw, _ := json.Marshal(evs)
	return strings.Contains(string(raw), `"type":"`+typ+`"`) && strings.Contains(string(raw), `"source":"ntfy"`)
}

func TestNtfyQuestionActionButtonsUnblockJob(t *testing.T) {
	hub, tricky := newNtfyHub(t, 1), `Beta, "2"; don't`
	qArgs, _ := json.Marshal(map[string]any{"question": "Pick option?", "options": []string{"Alpha", tricky}})
	m, p, job := newNtfyJob(t, hub, "https://glue.example.com/", "", turn(`{"items":[{"title":"ask"}]}`), toolCallEvents("q1", "ask_user", string(qArgs)), turn("answered"), turn(`{"done":true,"items":[{"title":"ask","done":true,"evidence":"ok"}],"summary":"done"}`))
	wantClick, p0 := "https://glue.example.com/#/jobs/"+job.ID, hub.wait(t, 1)[0]
	checkNtfy(t, p0.click == wantClick && p0.title == "Glue job waiting_for_input", "waiting post = %+v", p0)
	tapNtfyButton(t, p0.actions, tricky)
	checkNtfy(t, waitJob(t, m, job.ID).Status == Achieved && hasSourceNtfy(m, job.ID, "answer"), "expected achieved with ntfy answer")
	posts := hub.wait(t, 3)
	_ = postNotification(context.Background(), hub.URL+"/t?ntfy=1", Notification{JobID: job.ID, Status: Achieved, PRURL: "https://github.com/o/r/pull/9", clickURL: wantClick, Text: "pr"})
	checkNtfy(t, strings.Contains(drainLast(p), tricky) && strings.Contains(posts[1].body, "✓ Glue job "+job.ID+": answered") && posts[1].click == wantClick && posts[2].click == wantClick && hub.wait(t, 4)[3].click == "https://github.com/o/r/pull/9", "posts=%+v", posts)
}

func TestNtfyFreeTextReplyAnswersQuestion(t *testing.T) {
	hub := newNtfyHub(t, 0)
	m, p, job := newNtfyJob(t, hub, "", "", turn(`{"items":[{"title":"ask"}]}`), toolCallEvents("q1", "ask_user", `{"question":"What port?"}`), toolCallEvents("q2", "ask_user", `{"question":"Env?","options":["dev","stage","prod","dr"]}`), turn("done"), turn(`{"done":true,"items":[{"title":"ask","done":true,"evidence":"ok"}],"summary":"done"}`))
	p0 := hub.wait(t, 1)[0]
	_, r1, ok1 := strings.Cut(p0.body, "\nReply: ")
	checkNtfy(t, ok1 && len(ntfyButtons(p0.actions)) == 0 && strings.HasPrefix(p0.actions, "view, Reply in ntfy, "), "0-option post = %+v", p0)
	_, _ = http.Post(strings.TrimSpace(r1), "text/plain", strings.NewReader("8080"))
	p2 := hub.wait(t, 3)[2]
	_, r2, ok2 := strings.Cut(p2.body, "\nReply: ")
	checkNtfy(t, ok2 && len(ntfyButtons(p2.actions)) == 2 && strings.Contains(p2.actions, "; view, Reply in ntfy, "), ">3-option post = %+v", p2)
	_, _ = http.Post(strings.TrimSpace(r2), "text/plain", strings.NewReader("dr"))
	got, all := waitJob(t, m, job.ID), drainLast(p)
	checkNtfy(t, got.Status == Achieved && strings.Contains(all, "8080") && strings.Contains(all, "dr"), "status=%s all=%q", got.Status, all)
}

func TestNtfyPermissionActionButtonsApproveAndDeny(t *testing.T) {
	hub := newNtfyHub(t, 0)
	m, p, job := newNtfyJob(t, hub, "", "ask-all", turn(`{"items":[{"title":"perms"}]}`), toolCallEvents("w1", "write_file", `{"path":"a.txt","content":"1\n"}`), toolCallEvents("w2", "write_file", `{"path":"a.txt","content":"2\n"}`), toolCallEvents("w3", "write_file", `{"path":"b.txt","content":"3\n"}`), turn("handled"), turn(`{"done":true,"items":[{"title":"perms","done":true,"evidence":"ok"}],"summary":"done"}`))
	tapNtfyButton(t, hub.wait(t, 1)[0].actions, "Allow session")
	pDeny := hub.wait(t, 3)[2]
	checkNtfy(t, strings.Contains(pDeny.body, "b.txt"), "expected w3 prompted, got %+v", pDeny)
	tapNtfyButton(t, pDeny.actions, "Deny")
	got, all := waitJob(t, m, job.ID), drainLast(p)
	checkNtfy(t, got.Status == Achieved && hasSourceNtfy(m, job.ID, "permission_decision") && strings.Contains(strings.ToLower(all), "denied"), "status=%s all=%q", got.Status, all)
}

func TestNtfyReplyRedactsSecretsAndClosesOnCancel(t *testing.T) {
	hub := newNtfyHub(t, 0)
	m, _, job := newNtfyJob(t, hub, "", "", turn(`{"items":[{"title":"wait"}]}`), toolCallEvents("q1", "ask_user", `{"question":"Check?","options":["Yes","No"]}`))
	b0 := ntfyButtons(hub.wait(t, 1)[0].actions)[0]
	var parsed map[string]string
	_ = json.Unmarshal([]byte(b0[2]), &parsed)
	_, _ = http.Post(b0[1], "application/json", strings.NewReader(`{"token":"bad","question_id":"`+parsed["question_id"]+`","text":"Yes"}`))
	_, _ = http.Post(b0[1], "text/plain", strings.NewReader(strings.Repeat("x", maxNtfyReplyBytes+10)))
	time.Sleep(30 * time.Millisecond)
	waitStatus(t, m, job.ID, WaitingForInput)
	_, _ = m.Cancel(context.Background(), job.ID), waitJob(t, m, job.ID)
	open := 1
	for i := 0; i < 400 && open > 0; i++ {
		time.Sleep(5 * time.Millisecond)
		hub.mu.Lock()
		open = hub.open
		hub.mu.Unlock()
	}
	checkNtfy(t, len(parsed["token"]) == 32 && open == 0 && m.Close() == nil, "tok=%q open=%d", parsed["token"], open)
	assertNotPersisted(t, m.options.StateDir, "glue-reply-", parsed["token"])
}
