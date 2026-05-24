package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/agents/peggy"
)

type permissionAPIFixture struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies map[string][][]byte
}

func newPermissionAPIFixture(t *testing.T) *permissionAPIFixture {
	t.Helper()
	f := &permissionAPIFixture{bodies: map[string][][]byte{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies[method] = append(f.bodies[method], body)
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"ok": true, "result": {}}`)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *permissionAPIFixture) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies[method])
}

func (f *permissionAPIFixture) lastBody(method string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	bodies := f.bodies[method]
	if len(bodies) == 0 {
		return nil
	}
	return append([]byte(nil), bodies[len(bodies)-1]...)
}

func TestTelegramPermissionAllowOnceFromCallback(t *testing.T) {
	fix := newPermissionAPIFixture(t)
	api := NewAPI(fix.server.URL, "tk", fix.server.Client())
	perm := NewPermission(PermissionOptions{Timeout: time.Second})
	perm.attach(api, map[int64]struct{}{555: {}}, nil)
	req := glue.PermissionRequest{
		Tool:      "shell_exec",
		Action:    "exec",
		Target:    "go test ./...",
		Args:      []byte(`{"argv":["go","test","./..."]}`),
		SessionID: peggy.ChannelSessionID(ChannelName, "555"),
	}

	decisionCh := make(chan glue.PermissionDecision, 1)
	errCh := make(chan error, 1)
	go func() {
		decision, err := perm.Decide(context.Background(), req)
		decisionCh <- decision
		errCh <- err
	}()

	waitFor(t, func() bool { return fix.count("sendMessage") == 1 })
	callbackData := callbackDataForButton(t, fix.lastBody("sendMessage"), "Allow once")
	if !perm.handleCallback(context.Background(), CallbackQuery{
		ID:      "cb1",
		Message: &Message{Chat: Chat{ID: 555}},
		Data:    callbackData,
	}) {
		t.Fatal("callback was not handled")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Decide error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Decide error")
	}
	select {
	case decision := <-decisionCh:
		if !decision.Allow || decision.RememberFor != glue.RememberNever {
			t.Fatalf("decision = %+v, want allow once", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for decision")
	}
	if fix.count("answerCallbackQuery") != 1 {
		t.Fatalf("answerCallbackQuery count = %d, want 1", fix.count("answerCallbackQuery"))
	}
}

func TestTelegramPermissionRememberTarget(t *testing.T) {
	fix := newPermissionAPIFixture(t)
	api := NewAPI(fix.server.URL, "tk", fix.server.Client())
	perm := NewPermission(PermissionOptions{Timeout: time.Second})
	perm.attach(api, map[int64]struct{}{555: {}}, nil)
	req := glue.PermissionRequest{
		Tool:      "write_file",
		Action:    "write_file",
		Target:    "main.go",
		SessionID: peggy.ChannelSessionID(ChannelName, "555"),
	}

	decisionCh := make(chan glue.PermissionDecision, 1)
	go func() {
		decision, _ := perm.Decide(context.Background(), req)
		decisionCh <- decision
	}()
	waitFor(t, func() bool { return fix.count("sendMessage") == 1 })
	callbackData := callbackDataForButton(t, fix.lastBody("sendMessage"), "Allow target")
	perm.handleCallback(context.Background(), CallbackQuery{
		ID:      "cb1",
		Message: &Message{Chat: Chat{ID: 555}},
		Data:    callbackData,
	})
	if decision := <-decisionCh; !decision.Allow || decision.RememberFor != glue.RememberSessionTarget {
		t.Fatalf("decision = %+v, want target allow", decision)
	}

	second, err := perm.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}
	if !second.Allow || second.RememberFor != glue.RememberSessionTarget {
		t.Fatalf("second = %+v, want cached target allow", second)
	}
	if fix.count("sendMessage") != 1 {
		t.Fatalf("sendMessage count = %d, want cached second decision", fix.count("sendMessage"))
	}
}

func TestTelegramPermissionRejectsCallbackFromDifferentChat(t *testing.T) {
	fix := newPermissionAPIFixture(t)
	api := NewAPI(fix.server.URL, "tk", fix.server.Client())
	perm := NewPermission(PermissionOptions{Timeout: time.Second})
	perm.attach(api, map[int64]struct{}{555: {}}, nil)
	req := glue.PermissionRequest{
		Tool:      "write_file",
		Action:    "write_file",
		Target:    "main.go",
		SessionID: peggy.ChannelSessionID(ChannelName, "555"),
	}

	decisionCh := make(chan glue.PermissionDecision, 1)
	go func() {
		decision, _ := perm.Decide(context.Background(), req)
		decisionCh <- decision
	}()
	waitFor(t, func() bool { return fix.count("sendMessage") == 1 })
	callbackData := callbackDataForButton(t, fix.lastBody("sendMessage"), "Allow once")
	perm.handleCallback(context.Background(), CallbackQuery{
		ID:      "cb-wrong",
		Message: &Message{Chat: Chat{ID: 999}},
		Data:    callbackData,
	})
	select {
	case decision := <-decisionCh:
		t.Fatalf("wrong chat resolved decision unexpectedly: %+v", decision)
	case <-time.After(50 * time.Millisecond):
	}

	perm.handleCallback(context.Background(), CallbackQuery{
		ID:      "cb-right",
		Message: &Message{Chat: Chat{ID: 555}},
		Data:    callbackData,
	})
	select {
	case decision := <-decisionCh:
		if !decision.Allow {
			t.Fatalf("decision = %+v, want allow after right chat callback", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for right-chat decision")
	}
}

func TestTelegramPermissionTimeoutAndAllowlistDeny(t *testing.T) {
	fix := newPermissionAPIFixture(t)
	api := NewAPI(fix.server.URL, "tk", fix.server.Client())
	perm := NewPermission(PermissionOptions{Timeout: time.Millisecond})
	perm.attach(api, map[int64]struct{}{555: {}}, nil)

	timeoutDecision, err := perm.Decide(context.Background(), glue.PermissionRequest{
		Tool:      "shell_exec",
		Action:    "exec",
		Target:    "go test ./...",
		SessionID: peggy.ChannelSessionID(ChannelName, "555"),
	})
	if err != nil {
		t.Fatalf("timeout Decide: %v", err)
	}
	if timeoutDecision.Allow || !strings.Contains(timeoutDecision.Reason, "timed out") {
		t.Fatalf("timeout decision = %+v, want timeout deny", timeoutDecision)
	}

	denied, err := perm.Decide(context.Background(), glue.PermissionRequest{
		Tool:      "shell_exec",
		Action:    "exec",
		Target:    "go test ./...",
		SessionID: peggy.ChannelSessionID(ChannelName, "999"),
	})
	if err != nil {
		t.Fatalf("allowlist Decide: %v", err)
	}
	if denied.Allow || !strings.Contains(denied.Reason, "allowlisted") {
		t.Fatalf("allowlist decision = %+v, want allowlist deny", denied)
	}
}

func callbackDataForButton(t *testing.T, body []byte, text string) string {
	t.Helper()
	var payload struct {
		ReplyMarkup InlineKeyboardMarkup `json:"reply_markup"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("sendMessage body: %v", err)
	}
	for _, row := range payload.ReplyMarkup.InlineKeyboard {
		for _, button := range row {
			if button.Text == text {
				return button.CallbackData
			}
		}
	}
	t.Fatalf("button %q not found in %#v", text, payload.ReplyMarkup)
	return ""
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if ok() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for condition")
		case <-tick.C:
		}
	}
}
