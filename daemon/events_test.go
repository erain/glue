package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/erain/glue"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func eventServer(t *testing.T, dir string) *Server {
	t.Helper()
	s, err := New(Options{Host: sessionOnlyHost{}, Token: "token", StateDir: dir, KeepaliveInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func finishedEvents(t *testing.T, s *Server) *run {
	t.Helper()
	r := s.newRun("session", "client", func() {})
	for i := 0; i < 5; i++ {
		r.emit("text_delta", map[string]int{"n": i})
	}
	r.emit("run_done", nil)
	r.finish()
	return r
}

func eventRequest(s *Server, id, query, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+id+"/events"+query, nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Last-Event-ID", header)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func checkSequences(t *testing.T, w *httptest.ResponseRecorder, first int64) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	scanner := bufio.NewScanner(bytes.NewReader(w.Body.Bytes()))
	next := first
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			var e EventEnvelope
			if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &e); err != nil {
				t.Fatal(err)
			}
			if e.Seq != next {
				t.Fatalf("seq %d want %d", e.Seq, next)
			}
			next++
		}
	}
	if next != 7 {
		t.Fatalf("ended at %d want 7", next)
	}
}

func TestEventsResumeAfterSeq(t *testing.T) {
	s := eventServer(t, t.TempDir())
	r := finishedEvents(t, s)
	checkSequences(t, eventRequest(s, r.id, "?after=3", ""), 4)
}
func TestEventsLastEventIDHeader(t *testing.T) {
	s := eventServer(t, t.TempDir())
	r := finishedEvents(t, s)
	checkSequences(t, eventRequest(s, r.id, "", "3"), 4)
	checkSequences(t, eventRequest(s, r.id, "?after=4", "3"), 5)
}
func TestEventsDurableAcrossServerInstances(t *testing.T) {
	dir := t.TempDir()
	s := eventServer(t, dir)
	r := finishedEvents(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	other := eventServer(t, dir)
	checkSequences(t, eventRequest(other, r.id, "?after=3", ""), 4)
}
func TestSSEIDIsSeq(t *testing.T) {
	s := eventServer(t, t.TempDir())
	r := finishedEvents(t, s)
	w := eventRequest(s, r.id, "", "")
	id := ""
	scanner := bufio.NewScanner(bytes.NewReader(w.Body.Bytes()))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			id = strings.TrimPrefix(line, "id: ")
		}
		if strings.HasPrefix(line, "data: ") {
			var e EventEnvelope
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
				t.Fatal(err)
			}
			if id != strconv.FormatInt(e.Seq, 10) {
				t.Fatalf("id %q seq %d", id, e.Seq)
			}
		}
	}
}
func TestKeepaliveComments(t *testing.T) {
	s := eventServer(t, t.TempDir())
	r := s.newRun("session", "client", func() {})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/runs/"+r.id+"/events", nil)
	req.Header.Set("Authorization", "Bearer token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if scanner.Text() == ": ping" {
			return
		}
	}
	t.Fatal("no keepalive comment")
}

type releasableProvider struct{ release, canceled chan struct{} }

func (p releasableProvider) Stream(ctx context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	events := make(chan glue.ProviderEvent, 3)
	events <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	go func() {
		defer close(events)
		select {
		case <-ctx.Done():
			close(p.canceled)
		case <-p.release:
			events <- glue.ProviderEvent{Type: glue.ProviderEventTextDelta, Delta: "done"}
			events <- glue.ProviderEvent{Type: glue.ProviderEventDone}
		}
	}()
	return events, nil
}
func TestDisconnectDoesNotCancelRun(t *testing.T) {
	p := releasableProvider{make(chan struct{}), make(chan struct{})}
	s, err := New(Options{Host: glue.NewAgent(glue.AgentOptions{Provider: p}), Token: "token", StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	resp := postJSON(t, ts.URL+"/v1/sessions/default/runs", "token", `{"text":"go"}`)
	var start startRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+start.EventsURL, nil)
	req.Header.Set("Authorization", "Bearer token")
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	stream.Body.Close()
	select {
	case <-p.canceled:
		t.Fatal("disconnect canceled provider")
	case <-time.After(30 * time.Millisecond):
	}
	close(p.release)
	events := getSSE(t, fmt.Sprintf("%s%s", ts.URL, start.EventsURL), "token")
	if events[len(events)-1].Type != "run_done" {
		t.Fatal(events)
	}
}

func TestEventsInvalidCursor(t *testing.T) {
	s := eventServer(t, t.TempDir())
	r := finishedEvents(t, s)
	for _, query := range []string{"?after=-1", "?after=bad"} {
		if w := eventRequest(s, r.id, query, ""); w.Code != 400 {
			t.Fatalf("%s: %d", query, w.Code)
		}
	}
}
func TestDurableWriteFailureStopsPublication(t *testing.T) {
	s := eventServer(t, t.TempDir())
	canceled := false
	r := s.newRun("session", "client", func() { canceled = true })
	if _, err := s.log.db.Exec("DROP TABLE events"); err != nil {
		t.Fatal(err)
	}
	r.emit("text_delta", nil)
	events, done, _ := r.eventsFrom(0)
	if !canceled || !done || len(events) != 0 || r.persistErr == nil {
		t.Fatalf("canceled %v done %v events %v err %v", canceled, done, events, r.persistErr)
	}
}
