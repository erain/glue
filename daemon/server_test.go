package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

type scriptedProvider struct {
	events []glue.ProviderEvent
}

func (p scriptedProvider) Stream(context.Context, glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	ch := make(chan glue.ProviderEvent, len(p.events))
	for _, event := range p.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

type blockingProvider struct {
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func (p *blockingProvider) Stream(ctx context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	ch := make(chan glue.ProviderEvent, 1)
	ch <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	p.once.Do(func() { close(p.started) })
	go func() {
		<-ctx.Done()
		close(p.canceled)
		close(ch)
	}()
	return ch, nil
}

func TestServerAuthAndHealth(t *testing.T) {
	srv := newTestServer(t, glue.NewAgent(glue.AgentOptions{Provider: scriptedProvider{}}))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	resp = postJSON(t, ts.URL+"/v1/sessions/default/runs", "", `{"text":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	resp = postJSON(t, ts.URL+"/v1/sessions/default/runs", "wrong", `{"text":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestServerStartsRunAndStreamsEvents(t *testing.T) {
	agent := glue.NewAgent(glue.AgentOptions{Provider: scriptedProvider{events: []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: "hello"},
		{Type: glue.ProviderEventDone},
	}}})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/sessions/default/runs", "token", `{"text":"say hi","client_id":"cli:test"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	var start startRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	if start.RunID == "" || start.SessionID != "default" || !strings.HasSuffix(start.EventsURL, "/events") {
		t.Fatalf("start response = %+v", start)
	}

	events := getSSE(t, ts.URL+start.EventsURL, "token")
	types := eventTypes(events)
	for _, want := range []string{"run_start", "loop_start", "turn_start", "message_start", "text_delta", "message_end", "turn_end", "loop_end", "run_done"} {
		if !contains(types, want) {
			t.Fatalf("events = %v, missing %s", types, want)
		}
	}
	if types[0] != "run_start" || types[len(types)-1] != "run_done" {
		t.Fatalf("events = %v, want run_start ... run_done", types)
	}
	for i, event := range events {
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, event.Seq, i+1)
		}
		if event.RunID != start.RunID || event.SessionID != "default" {
			t.Fatalf("event = %+v, want run/session ids", event)
		}
	}
}

func TestServerCancelRun(t *testing.T) {
	provider := &blockingProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	agent := glue.NewAgent(glue.AgentOptions{Provider: provider})
	srv := newTestServer(t, agent)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/sessions/default/runs", "token", `{"text":"wait"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start status = %d", resp.StatusCode)
	}
	var start startRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/v1/runs/"+start.RunID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	cancelResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want %d", cancelResp.StatusCode, http.StatusAccepted)
	}
	select {
	case <-provider.canceled:
	case <-time.After(time.Second):
		t.Fatal("provider was not canceled")
	}

	events := getSSE(t, ts.URL+start.EventsURL, "token")
	types := eventTypes(events)
	if types[len(types)-1] != "run_error" {
		t.Fatalf("events = %v, want terminal run_error", types)
	}
}

func newTestServer(t *testing.T, host Host) *Server {
	t.Helper()
	now := time.Date(2026, 5, 23, 20, 46, 0, 0, time.UTC)
	srv, err := New(Options{
		Host:  host,
		Token: "token",
		Now:   func() time.Time { return now },
		NewID: sequenceIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func sequenceIDs() func(prefix string) string {
	var mu sync.Mutex
	var n int
	return func(prefix string) string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("%s_%02d", prefix, n)
	}
}

func postJSON(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getSSE(t *testing.T, url, token string) []EventEnvelope {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var events []EventEnvelope
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event EventEnvelope
		dec := json.NewDecoder(bytes.NewBufferString(strings.TrimPrefix(line, "data: ")))
		dec.UseNumber()
		if err := dec.Decode(&event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no SSE events")
	}
	return events
}

func eventTypes(events []EventEnvelope) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Type)
	}
	return out
}

func contains(in []string, want string) bool {
	for _, got := range in {
		if got == want {
			return true
		}
	}
	return false
}
