package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSharedStreamResumeAndTerminal(t *testing.T) {
	s := eventServer(t, t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/events?after=1", nil)
	req.Header.Set("Last-Event-ID", "0")
	w := httptest.NewRecorder()
	s.streamEvents(w, req, func(after int64) (eventBatch, error) {
		if after != 1 {
			t.Fatalf("after %d", after)
		}
		return eventBatch{events: []EventEnvelope{{Seq: 2, Type: "job_done"}}, done: true}, nil
	})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "id: 2\n") || !strings.Contains(w.Body.String(), "event: job_done") {
		t.Fatalf("response %d %s", w.Code, w.Body.String())
	}
}
func TestSharedStreamReadFailure(t *testing.T) {
	s := eventServer(t, t.TempDir())
	w := httptest.NewRecorder()
	s.streamEvents(w, httptest.NewRequest(http.MethodGet, "/events", nil), func(int64) (eventBatch, error) { return eventBatch{}, errors.New("read failed") })
	if w.Code != 500 || strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("response %d %s", w.Code, w.Body.String())
	}
}
func TestSharedStreamIdleAndLaterFailure(t *testing.T) {
	s := eventServer(t, t.TempDir())
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.streamEvents(w, r, func(int64) (eventBatch, error) {
			calls++
			if calls > 1 {
				return eventBatch{}, errors.New("read failed")
			}
			return eventBatch{events: []EventEnvelope{{Seq: 1, Type: "text_delta"}}}, nil
		})
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(body), ": ping") || strings.Contains(string(body), "job_done") || strings.Count(string(body), "id: 1\n") != 1 {
		t.Fatalf("body %s err %v", body, err)
	}
}
