package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/erain/glue/loop"
	"github.com/erain/glue/providers"
)

func TestStreamAndToolReplay(t *testing.T) {
	type captured struct {
		body       map[string]any
		auth, path string
	}
	requests := make(chan captured, 2)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		requests <- captured{body, r.Header.Get("Authorization"), r.URL.Path}
		w.Header().Set("Content-Type", "text/event-stream")
		calls++
		if calls == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Inspect files\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"function\":{\"name\":\"inspect\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"total_tokens\":10}}\n\ndata: [DONE]\n\n")
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv(EnvKey, "fixture-key")
	p := New(Options{BaseURL: server.URL, HTTPClient: server.Client()})
	req := loop.ProviderRequest{Tools: []loop.ToolSpec{{Name: "inspect", Parameters: json.RawMessage(`{"type":"object"}`)}}, Messages: []loop.Message{{Role: loop.MessageRoleUser, Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "inspect"}}}}}
	var assistant *loop.Message
	events, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	thought := ""
	for e := range events {
		if e.Type == loop.ProviderEventError {
			t.Fatal(e.Error)
		}
		if e.Type == loop.ProviderEventThinkingDelta {
			thought += e.Delta
		}
		if e.Type == loop.ProviderEventDone {
			assistant = e.Message
		}
	}
	if thought != "Inspect files" || assistant == nil || assistant.Provider != "deepseek" || assistant.Model != DefaultModel {
		t.Fatalf("thought=%q assistant=%#v", thought, assistant)
	}
	first := <-requests
	if first.auth != "Bearer fixture-key" || first.path != "/chat/completions" || first.body["model"] != DefaultModel {
		t.Fatal(first)
	}
	req.Messages = append(req.Messages, *assistant, loop.Message{Role: loop.MessageRoleTool, ToolCallID: "call1", ToolName: "inspect", Content: []loop.ContentPart{{Type: loop.ContentTypeText, Text: "files"}}})
	events, err = p.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for e := range events {
		if e.Type == loop.ProviderEventError {
			t.Fatal(e.Error)
		}
		if e.Type == loop.ProviderEventTextDelta {
			text += e.Delta
		}
		if e.Type == loop.ProviderEventDone && (e.Message == nil || e.Message.Usage == nil || e.Message.Usage.TotalTokens != 10) {
			t.Fatalf("usage=%#v", e.Message)
		}
	}
	if text != "Done" {
		t.Fatal(text)
	}
	second := <-requests
	messages := second.body["messages"].([]any)
	replay := messages[1].(map[string]any)
	if replay["reasoning_content"] != "Inspect files" || len(replay["tool_calls"].([]any)) != 1 || messages[2].(map[string]any)["tool_call_id"] != "call1" {
		t.Fatal(messages)
	}
}

func TestThinkingOptions(t *testing.T) {
	for _, level := range []string{"", "default", "off", "low", "high", "max"} {
		t.Run(level, func(t *testing.T) {
			var captured map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&captured)
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			t.Cleanup(server.Close)
			p := New(Options{APIKey: "key", BaseURL: server.URL})
			opts := map[string]any{"thinking_level": level}
			before := map[string]any{"thinking_level": level}
			events, err := p.Stream(context.Background(), loop.ProviderRequest{Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			for range events {
			}
			if !reflect.DeepEqual(opts, before) {
				t.Fatal("mutated options")
			}
			if _, ok := captured["thinking_level"]; ok {
				t.Fatal("normalized option leaked to API")
			}
			if level == "" || level == "default" {
				if captured["thinking"] != nil || captured["reasoning_effort"] != nil {
					t.Fatal(captured)
				}
				return
			}
			want := "enabled"
			if level == "off" {
				want = "disabled"
			}
			if captured["thinking"].(map[string]any)["type"] != want {
				t.Fatal(captured)
			}
			if level != "off" && captured["reasoning_effort"] != level {
				t.Fatal(captured)
			}
		})
	}
	for _, opts := range []map[string]any{
		{"thinking_level": "medium"}, {"thinking_level": 42}, {"thinking_level": "low", "thinking": map[string]any{"type": "disabled"}}, {"thinking_level": "low", "reasoning_effort": "max"},
	} {
		_, err := New(Options{}).Stream(context.Background(), loop.ProviderRequest{Options: opts})
		if err == nil {
			t.Fatalf("expected error for %v", opts)
		}
	}
}

func TestRegistrationAndErrors(t *testing.T) {
	f, ok := providers.Lookup("deepseek")
	if !ok || f.DefaultModel != DefaultModel || f.EnvKey != EnvKey || len(providers.Models("deepseek")) != 1 {
		t.Fatalf("factory=%#v", f)
	}
	t.Setenv(EnvKey, "")
	_, err := New(Options{}).Stream(context.Background(), loop.ProviderRequest{})
	if err == nil || !strings.Contains(err.Error(), EnvKey) {
		t.Fatalf("err=%v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fixture unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	_, err = New(Options{APIKey: "key", BaseURL: server.URL}).Stream(context.Background(), loop.ProviderRequest{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err=%v", err)
	}
}
