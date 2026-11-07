package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erain/glue"
)

func TestRunThinkingSelection(t *testing.T) {
	for _, tc := range []struct {
		provider, model, level string
		valid                  bool
	}{
		{"gemini", "gemini-4-argon", "high", true}, {"gemini", "gemini/gemini-4-argon", "medium", true},
		{"gemini", "gemini-3.1-pro-preview", "medium", true}, {"gemini", "gemini-3-pro-preview", "medium", false},
		{"deepseek", "", "off", true}, {"deepseek", "", "max", true}, {"deepseek", "", "medium", false},
		{"gemini", "custom", "", true}, {"gemini", "custom", "default", true}, {"gemini", "custom", "high", false},
		{"codex", "", "high", false},
	} {
		t.Run(tc.provider+"/"+tc.model+"/"+tc.level, func(t *testing.T) {
			p := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("ok")}}
			constructed := false
			factory := func(string) (glue.Provider, error) { constructed = true; return p, nil }
			var out, errs bytes.Buffer
			code := runCLI(context.Background(), []string{"run", "--provider", tc.provider, "--model", tc.model, "--thinking", tc.level, "--prompt", "go", "--store", t.TempDir()}, &out, &errs, factory)
			if !tc.valid {
				if code == 0 || constructed || !strings.Contains(errs.String(), "unsupported") {
					t.Fatalf("code=%d constructed=%v err=%s", code, constructed, errs.String())
				}
				return
			}
			if code != 0 || len(p.requests) != 1 {
				t.Fatalf("code=%d err=%s", code, errs.String())
			}
			want := tc.level
			if want == "default" {
				want = ""
			}
			got, _ := p.requests[0].Options["thinking_level"].(string)
			if got != want {
				t.Fatalf("level=%q want=%q", got, want)
			}
		})
	}
}

func TestGoalThinkingAppliesToEveryPhase(t *testing.T) {
	p := &scriptedProvider{turns: [][]glue.ProviderEvent{
		goalCLITurn(`{"items":[{"title":"A"}]}`), goalCLITurn("did A"), goalCLITurn(`{"done":true,"items":[{"title":"A","done":true}],"summary":"done"}`),
	}}
	var errs bytes.Buffer
	code := runCLI(context.Background(), []string{"goal", "--provider", "deepseek", "--thinking", "low", "--store", t.TempDir(), "ship A"}, io.Discard, &errs, fakeFactory(p))
	if code != 0 || len(p.requests) != 3 {
		t.Fatalf("code=%d requests=%d err=%s", code, len(p.requests), errs.String())
	}
	for _, req := range p.requests {
		if req.Options["thinking_level"] != "low" || req.Model != "deepseek-flash" {
			t.Fatalf("request=%#v", req)
		}
	}
}

func TestServeThinkingReachesDaemonRun(t *testing.T) {
	p := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("served")}}
	serve := func(ctx context.Context, _ serveConfig, handler http.Handler, _ io.Writer) error {
		start := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions/default/runs", strings.NewReader(`{"text":"go"}`))
		request.Header.Set("Authorization", "Bearer fixture-token")
		handler.ServeHTTP(start, request)
		if start.Code != http.StatusCreated {
			t.Fatalf("start=%d %s", start.Code, start.Body.String())
		}
		var run struct {
			EventsURL string `json:"events_url"`
		}
		if err := json.Unmarshal(start.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		events := httptest.NewRequest(http.MethodGet, run.EventsURL, nil).WithContext(ctx)
		events.Header.Set("Authorization", "Bearer fixture-token")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, events)
		if result.Code != http.StatusOK {
			t.Fatal(result.Code)
		}
		return nil
	}
	var errs bytes.Buffer
	code := runCLIWithServe(context.Background(), []string{"serve", "--provider", "deepseek", "--thinking", "max", "--token", "fixture-token", "--metadata", "", "--store", t.TempDir()}, io.Discard, &errs, fakeFactory(p), serve)
	if code != 0 || len(p.requests) != 1 {
		t.Fatalf("code=%d requests=%d err=%s", code, len(p.requests), errs.String())
	}
	if p.requests[0].Options["thinking_level"] != "max" {
		t.Fatal(p.requests[0].Options)
	}
}

func TestInvalidThinkingFailsBeforeProviderInServeAndGoal(t *testing.T) {
	for _, cmd := range []string{"serve", "goal"} {
		t.Run(cmd, func(t *testing.T) {
			var errs bytes.Buffer
			code := runCLI(context.Background(), []string{cmd, "--provider", "deepseek", "--thinking", "medium"}, io.Discard, &errs, func(string) (glue.Provider, error) { t.Fatal("provider constructed"); return nil, nil })
			if code == 0 || !strings.Contains(errs.String(), "unsupported") {
				t.Fatalf("code=%d err=%s", code, errs.String())
			}
		})
	}
}
