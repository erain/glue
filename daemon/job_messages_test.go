// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJobMessagesHTTP(t *testing.T) {
	s, _, repo := jobHTTPFixture(t, nil)
	id := createHTTPJob(t, s, repo, false)
	path := "/v1/jobs/" + id + "/messages"
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"text":"guidance"}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	jobHTTP(t, s, http.MethodPost, "/v1/jobs/missing/messages", `{"text":"guidance"}`, 404)
	jobHTTP(t, s, http.MethodGet, path, "", 405)
	for _, body := range []string{`{}`, `{"text":" "}`, `{"text":5}`, `{"text":"guidance","extra":true}`, `{"text":"guidance"} {}`, `null`} {
		jobHTTP(t, s, http.MethodPost, path, body, 400)
	}
	result := jobHTTP(t, s, http.MethodPost, path, `{"text":"guidance"}`, 202)
	if result["accepted"] != true || result["job_id"] != id {
		t.Fatal(result)
	}
	events, err := s.jobs.Events(context.Background(), id, 0)
	if err != nil || len(events) != 2 || events[1].Type != "operator_message" {
		t.Fatalf("events %v err %v", events, err)
	}
	jobHTTP(t, s, http.MethodDelete, "/v1/jobs/"+id, "", 202)
	jobHTTP(t, s, http.MethodPost, path, `{"text":"too late"}`, 409)
}
