package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/agents/peggy"
	filestore "github.com/erain/glue/stores/file"
)

// fakeProvider is a stripped fakeProvider for channel tests. The
// fakeProvider in agents/peggy is package-private, so we keep a local
// copy here.
type fakeProvider struct {
	text     string
	mu       sync.Mutex
	requests []glue.ProviderRequest
	calls    int
}

func (p *fakeProvider) Stream(_ context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.calls++
	text := p.text
	p.mu.Unlock()
	ch := make(chan glue.ProviderEvent, 4)
	ch <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	ch <- glue.ProviderEvent{Type: glue.ProviderEventTextDelta, Delta: text}
	ch <- glue.ProviderEvent{Type: glue.ProviderEventDone, Message: &glue.Message{
		Role:    glue.MessageRoleAssistant,
		Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: text}},
	}}
	close(ch)
	return ch, nil
}

type toolProvider struct {
	mu       sync.Mutex
	requests []glue.ProviderRequest
	calls    int
}

func (p *toolProvider) Stream(_ context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	call := p.calls
	p.calls++
	p.mu.Unlock()
	ch := make(chan glue.ProviderEvent, 4)
	ch <- glue.ProviderEvent{Type: glue.ProviderEventStart}
	if call == 0 {
		ch <- glue.ProviderEvent{Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{
			ID:        "c1",
			Name:      "write_file",
			Arguments: []byte(`{"path":"note.txt","content":"hello from telegram"}`),
		}}
		ch <- glue.ProviderEvent{Type: glue.ProviderEventDone}
	} else {
		ch <- glue.ProviderEvent{Type: glue.ProviderEventTextDelta, Delta: "done"}
		ch <- glue.ProviderEvent{Type: glue.ProviderEventDone, Message: &glue.Message{
			Role:    glue.MessageRoleAssistant,
			Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "done"}},
		}}
	}
	close(ch)
	return ch, nil
}

func newTestPeggy(t *testing.T, fp *fakeProvider) *peggy.Peggy {
	t.Helper()
	store := filestore.New(filepath.Join(t.TempDir(), "sessions"))
	p, err := peggy.New(peggy.Options{
		Settings:           peggy.Settings{},
		Provider:           fp,
		Store:              store,
		DisableMemoryTools: true, // avoid file-store search errors
	})
	if err != nil {
		t.Fatalf("peggy.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// telegramFixture is a fake Telegram Bot API server. It serves
// scripted responses to getUpdates and records sendMessage POSTs.
type telegramFixture struct {
	server     *httptest.Server
	mu         sync.Mutex
	updates    [][]Update // each call to getUpdates pops the next slice
	updatesCh  chan struct{}
	sendBodies [][]byte
}

func newTelegramFixture(t *testing.T, updates [][]Update) *telegramFixture {
	t.Helper()
	f := &telegramFixture{updates: updates, updatesCh: make(chan struct{}, 1)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			f.mu.Lock()
			var next []Update
			if len(f.updates) > 0 {
				next = f.updates[0]
				f.updates = f.updates[1:]
			}
			f.mu.Unlock()
			payload := struct {
				OK     bool     `json:"ok"`
				Result []Update `json:"result"`
			}{OK: true, Result: next}
			_ = json.NewEncoder(w).Encode(payload)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.sendBodies = append(f.sendBodies, body)
			f.mu.Unlock()
			_, _ = io.WriteString(w, `{"ok": true, "result": {"message_id": 1, "chat": {"id": 0, "type": "private"}, "date": 0, "text": ""}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *telegramFixture) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sendBodies)
}

func (f *telegramFixture) lastSendText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sendBodies) == 0 {
		return ""
	}
	var body map[string]any
	_ = json.Unmarshal(f.sendBodies[len(f.sendBodies)-1], &body)
	if s, ok := body["text"].(string); ok {
		return s
	}
	return ""
}

func (f *telegramFixture) sendBody(index int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index < 0 || index >= len(f.sendBodies) {
		return nil
	}
	return append([]byte(nil), f.sendBodies[index]...)
}

func (f *telegramFixture) appendUpdates(updates []Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, updates)
}

func messageUpdate(updateID int64, chatID int64, text string) Update {
	return Update{UpdateID: updateID, Message: &Message{
		MessageID: updateID, Chat: Chat{ID: chatID, Type: "private"}, Text: text, Date: time.Now().Unix(),
	}}
}

func callbackUpdate(updateID int64, chatID int64, data string) Update {
	return Update{UpdateID: updateID, CallbackQuery: &CallbackQuery{
		ID:      fmt.Sprintf("cb-%d", updateID),
		Message: &Message{MessageID: updateID, Chat: Chat{ID: chatID, Type: "private"}, Text: "permission", Date: time.Now().Unix()},
		Data:    data,
	}}
}

func TestChannel_AllowedChatPromptsAndReplies(t *testing.T) {
	fp := &fakeProvider{text: "Hello back."}
	p := newTestPeggy(t, fp)
	fix := newTelegramFixture(t, [][]Update{
		{messageUpdate(1, 555, "Hi Peggy")},
	})
	ch, err := New(Options{
		Peggy: p,
		Token: "secret",
		Config: Config{
			BotTokenEnv:            "PEGGY_TELEGRAM_TOKEN",
			AllowChats:             []int64{555},
			APIBaseURL:             fix.server.URL,
			LongPollTimeoutSeconds: 1,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ch.Run(ctx) }()

	// Wait until sendMessage has been hit, then cancel.
	deadline := time.After(2500 * time.Millisecond)
	for fix.sendCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for sendMessage")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fp.calls == 0 {
		t.Fatal("provider not invoked")
	}
	if got := fix.lastSendText(); !strings.Contains(got, "Hello back") {
		t.Errorf("send text = %q", got)
	}
	// Session id namespacing is exercised end-to-end here: opening
	// the namespaced session id on the same agent must return a
	// non-empty transcript (the message we just routed). The
	// namespacing helper itself is covered in agents/peggy/channel_test.go.
	if fp.requests[0].Messages[0].Content[0].Text != "Hi Peggy" {
		t.Errorf("provider got wrong text: %+v", fp.requests[0].Messages)
	}
	sess, err := p.Agent().Session(ctx, "telegram:555")
	if err != nil {
		t.Fatalf("namespaced session lookup: %v", err)
	}
	if msgs := sess.Messages(); len(msgs) == 0 {
		t.Errorf("namespaced session has no messages — session-id routing broken")
	}
}

func TestChannel_TelegramPermissionCallbackUnblocksPrompt(t *testing.T) {
	workDir := t.TempDir()
	provider := &toolProvider{}
	store := filestore.New(filepath.Join(t.TempDir(), "sessions"))
	perm := NewPermission(PermissionOptions{Timeout: 2 * time.Second})
	p, err := peggy.New(peggy.Options{
		Settings: peggy.Settings{Coding: peggy.CodingSettings{
			Enabled:         true,
			WorkDir:         workDir,
			AllowOverwrite:  true,
			AllowedBinaries: []string{"go"},
		}},
		Provider:           provider,
		Store:              store,
		DisableMemoryTools: true,
		Permission:         perm,
	})
	if err != nil {
		t.Fatalf("peggy.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	fix := newTelegramFixture(t, [][]Update{{messageUpdate(1, 555, "write the file")}})
	ch, err := New(Options{
		Peggy:      p,
		Token:      "secret",
		Permission: perm,
		Config: Config{
			AllowChats:             []int64{555},
			APIBaseURL:             fix.server.URL,
			LongPollTimeoutSeconds: 1,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ch.Run(ctx) }()

	waitFor(t, func() bool { return fix.sendCount() >= 1 })
	callbackData := callbackDataForButton(t, fix.sendBody(0), "Allow once")
	fix.appendUpdates([]Update{callbackUpdate(2, 555, callbackData)})
	waitFor(t, func() bool { return fix.sendCount() >= 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "note.txt"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(data) != "hello from telegram" {
		t.Fatalf("written content = %q", data)
	}
	if got := fix.lastSendText(); got != "done" {
		t.Fatalf("final send text = %q, want done", got)
	}
}

func TestChannel_NonAllowedChatDropped(t *testing.T) {
	fp := &fakeProvider{text: "nope"}
	p := newTestPeggy(t, fp)
	fix := newTelegramFixture(t, [][]Update{
		{messageUpdate(1, 999, "intruder")},
	})
	var stderr bytes.Buffer
	ch, err := New(Options{
		Peggy: p,
		Token: "secret",
		Config: Config{
			AllowChats:             []int64{555}, // 999 not listed
			APIBaseURL:             fix.server.URL,
			LongPollTimeoutSeconds: 1,
		},
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ch.Run(ctx) }()

	// Wait briefly so the goroutine has time to process the update.
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	if fp.calls != 0 {
		t.Errorf("non-allowlisted chat should not call provider; calls=%d", fp.calls)
	}
	if fix.sendCount() != 0 {
		t.Errorf("no reply should be sent; sends=%d", fix.sendCount())
	}
	if !strings.Contains(stderr.String(), "non-allowlisted") {
		t.Errorf("stderr missing drop diagnostic: %q", stderr.String())
	}
}

func TestChannel_EmptyAllowChatsRefusesAll(t *testing.T) {
	fp := &fakeProvider{text: "nope"}
	p := newTestPeggy(t, fp)
	fix := newTelegramFixture(t, [][]Update{
		{messageUpdate(1, 1, "hi")},
	})
	var stderr bytes.Buffer
	ch, err := New(Options{
		Peggy:  p,
		Token:  "secret",
		Config: Config{APIBaseURL: fix.server.URL, LongPollTimeoutSeconds: 1},
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go ch.Run(ctx)
	time.Sleep(400 * time.Millisecond)
	cancel()
	if fp.calls != 0 {
		t.Errorf("calls = %d, expected 0", fp.calls)
	}
	if !strings.Contains(stderr.String(), "refusing all inbound") {
		t.Errorf("refuse-all diagnostic missing: %q", stderr.String())
	}
}

func TestChannel_MultipleUpdatesPerPoll(t *testing.T) {
	fp := &fakeProvider{text: "ack"}
	p := newTestPeggy(t, fp)
	fix := newTelegramFixture(t, [][]Update{
		{
			messageUpdate(10, 555, "first"),
			messageUpdate(11, 555, "second"),
			messageUpdate(12, 555, "third"),
		},
	})
	ch, err := New(Options{
		Peggy: p,
		Token: "tk",
		Config: Config{
			AllowChats:             []int64{555},
			APIBaseURL:             fix.server.URL,
			LongPollTimeoutSeconds: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ch.Run(ctx) }()
	deadline := time.After(2500 * time.Millisecond)
	for fix.sendCount() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d sends recorded", fix.sendCount())
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	cancel()
	<-done
	if fp.calls != 3 {
		t.Errorf("provider calls = %d, want 3", fp.calls)
	}
	if fix.sendCount() != 3 {
		t.Errorf("sends = %d, want 3", fix.sendCount())
	}
}

func TestChannel_ContextCancelExitsRun(t *testing.T) {
	fp := &fakeProvider{text: "ack"}
	p := newTestPeggy(t, fp)
	fix := newTelegramFixture(t, nil) // no updates; loop just polls
	ch, _ := New(Options{
		Peggy: p, Token: "tk",
		Config: Config{
			AllowChats:             []int64{1},
			APIBaseURL:             fix.server.URL,
			LongPollTimeoutSeconds: 1,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ch.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}

func TestNew_MissingTokenErrors(t *testing.T) {
	fp := &fakeProvider{}
	p := newTestPeggy(t, fp)
	_, err := New(Options{Peggy: p, Config: Config{BotTokenEnv: "NEVER_SET_TOKEN_VAR"}})
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	if !strings.Contains(err.Error(), "bot token") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNew_NilPeggyErrors(t *testing.T) {
	_, err := New(Options{Token: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestChannel_PromptErrorRepliesWithFallback(t *testing.T) {
	fp := &errProvider{}
	p := newTestPeggy2(t, fp)
	fix := newTelegramFixture(t, [][]Update{
		{messageUpdate(1, 555, "boom")},
	})
	var stderr bytes.Buffer
	ch, _ := New(Options{
		Peggy: p, Token: "tk", Stderr: &stderr,
		Config: Config{
			AllowChats:             []int64{555},
			APIBaseURL:             fix.server.URL,
			LongPollTimeoutSeconds: 1,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ch.Run(ctx) }()
	deadline := time.After(1500 * time.Millisecond)
	for fix.sendCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("no fallback reply sent")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	cancel()
	<-done
	if !strings.Contains(fix.lastSendText(), "error") {
		t.Errorf("fallback reply text = %q", fix.lastSendText())
	}
	if !strings.Contains(stderr.String(), "prompt failed") {
		t.Errorf("missing diagnostic: %q", stderr.String())
	}
}

// errProvider always fails Stream so we can exercise the channel's
// fallback-reply path.
type errProvider struct{ count atomic.Int64 }

func (p *errProvider) Stream(_ context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.count.Add(1)
	return nil, fmt.Errorf("simulated upstream failure")
}

func newTestPeggy2(t *testing.T, fp glue.Provider) *peggy.Peggy {
	t.Helper()
	store := filestore.New(filepath.Join(t.TempDir(), "sessions"))
	p, err := peggy.New(peggy.Options{
		Settings:           peggy.Settings{},
		Provider:           fp,
		Store:              store,
		DisableMemoryTools: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}
