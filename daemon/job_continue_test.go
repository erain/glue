// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
)

func TestJobContinueHTTP(t *testing.T) {
	p := &turnProvider{turns: [][]glue.ProviderEvent{}}
	p.turns = append(p.turns, textTurn(`{"items":[{"title":"A"}]}`))
	for i := 0; i < 3; i++ {
		p.turns = append(p.turns, textTurn("work"), textTurn(`{"items":[{"title":"A"}]}`))
	}
	p.turns = append(p.turns, textTurn("finish"), textTurn(`{"done":true,"items":[{"title":"A","done":true}]}`))
	s, _, repo := jobHTTPFixture(t, p)
	id := createHTTPJob(t, s, repo, false)
	path := "/v1/jobs/" + id + "/continue"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	jobHTTP(t, s, http.MethodPost, "/v1/jobs/missing/continue", `{}`, 404)
	jobHTTP(t, s, http.MethodGet, path, "", 405)
	for _, body := range []string{`{"max_iterations":-1}`, `{"max_iterations":"bad"}`, `{"extra":true}`, `{} {}`} {
		jobHTTP(t, s, http.MethodPost, path, body, 400)
	}
	jobHTTP(t, s, http.MethodPost, path, `{"text":"again","max_iterations":1}`, 409)
	jobHTTP(t, s, http.MethodPost, "/v1/jobs/"+id+"/start", "", 200)
	wait := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			job, err := s.jobs.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if string(job.Status) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("status %s want %s", job.Status, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait("blocked")
	result := jobHTTP(t, s, http.MethodPost, path, `{"text":"finish A","max_iterations":1}`, 202)
	if result["accepted"] != true || result["job_id"] != id {
		t.Fatal(result)
	}
	wait("achieved")
}
