package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetUpdates_ParsesCannedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
            "ok": true,
            "result": [
                {"update_id": 1, "message": {"message_id": 100, "chat": {"id": 555, "type": "private"}, "date": 1700000000, "text": "hello"}},
                {"update_id": 2, "message": {"message_id": 101, "chat": {"id": 555, "type": "private"}, "date": 1700000001, "text": "hi again"}}
            ]
        }`)
	}))
	defer srv.Close()

	api := NewAPI(srv.URL, "secret-token", srv.Client())
	updates, err := api.GetUpdates(context.Background(), 0, 30)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("got %d updates: %+v", len(updates), updates)
	}
	if updates[0].Message.Text != "hello" || updates[1].Message.Text != "hi again" {
		t.Errorf("text mismatch: %+v", updates)
	}
	if updates[0].Message.Chat.ID != 555 {
		t.Errorf("chat id = %d", updates[0].Message.Chat.ID)
	}
}

func TestGetUpdates_ParsesCallbackQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
            "ok": true,
            "result": [
                {"update_id": 3, "callback_query": {"id": "cb1", "from": {"id": 42}, "message": {"message_id": 200, "chat": {"id": 555, "type": "private"}, "date": 1700000000, "text": "permission"}, "data": "perm:abc:once"}}
            ]
        }`)
	}))
	defer srv.Close()

	api := NewAPI(srv.URL, "secret-token", srv.Client())
	updates, err := api.GetUpdates(context.Background(), 0, 30)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates) != 1 || updates[0].CallbackQuery == nil {
		t.Fatalf("updates = %+v, want callback_query", updates)
	}
	cb := updates[0].CallbackQuery
	if cb.ID != "cb1" || cb.Data != "perm:abc:once" || cb.Message.Chat.ID != 555 {
		t.Fatalf("callback = %+v, want parsed id/data/chat", cb)
	}
}

func TestSendMessage_PostsCorrectShape(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"ok": true, "result": {}}`)
	}))
	defer srv.Close()

	api := NewAPI(srv.URL, "tk", srv.Client())
	if err := api.SendMessage(context.Background(), 555, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("body json: %v", err)
	}
	if body["chat_id"].(float64) != 555 {
		t.Errorf("chat_id = %v", body["chat_id"])
	}
	if body["text"] != "hello" {
		t.Errorf("text = %v", body["text"])
	}
}

func TestSendMessageWithReplyMarkup_PostsInlineKeyboard(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"ok": true, "result": {}}`)
	}))
	defer srv.Close()

	api := NewAPI(srv.URL, "tk", srv.Client())
	keyboard := InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
		{Text: "Allow once", CallbackData: "perm:1:once"},
	}}}
	if err := api.SendMessageWithReplyMarkup(context.Background(), 555, "allow?", keyboard); err != nil {
		t.Fatalf("SendMessageWithReplyMarkup: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("body json: %v", err)
	}
	if body["reply_markup"] == nil {
		t.Fatalf("body = %+v, missing reply_markup", body)
	}
}

func TestAnswerCallbackQuery_PostsCorrectShape(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"ok": true, "result": {}}`)
	}))
	defer srv.Close()

	api := NewAPI(srv.URL, "tk", srv.Client())
	if err := api.AnswerCallbackQuery(context.Background(), "cb1", "Allowed."); err != nil {
		t.Fatalf("AnswerCallbackQuery: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("body json: %v", err)
	}
	if body["callback_query_id"] != "cb1" || body["text"] != "Allowed." {
		t.Fatalf("body = %+v, want callback id/text", body)
	}
}

func TestSendMessage_Truncates(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"ok": true, "result": {}}`)
	}))
	defer srv.Close()
	api := NewAPI(srv.URL, "tk", srv.Client())
	long := strings.Repeat("x", telegramMessageLimit+1000)
	if err := api.SendMessage(context.Background(), 1, long); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(capturedBody, &body)
	got := body["text"].(string)
	if len(got) > telegramMessageLimit {
		t.Errorf("truncation failed: %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("missing truncation marker: %q", got[len(got)-30:])
	}
}

func TestAPI_NotOKResponseSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok": false, "error_code": 401, "description": "Unauthorized"}`)
	}))
	defer srv.Close()
	api := NewAPI(srv.URL, "tk", srv.Client())
	_, err := api.GetUpdates(context.Background(), 0, 30)
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPI_EmptyTokenErrors(t *testing.T) {
	api := NewAPI("http://example", "", nil)
	if _, err := api.GetUpdates(context.Background(), 0, 30); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestRedactErr_StripsToken(t *testing.T) {
	err := errors.New("Get https://api.telegram.org/botSECRET/getUpdates: dial tcp: no route to host")
	red := redactErr(err, "SECRET")
	if strings.Contains(red.Error(), "SECRET") {
		t.Fatalf("token leaked: %v", red)
	}
	if !strings.Contains(red.Error(), "<redacted>") {
		t.Fatalf("no redaction marker: %v", red)
	}
}
