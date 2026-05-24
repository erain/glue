package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Update is one entry from getUpdates. Only the fields glue needs are
// modeled; everything else is ignored.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// Message is the text message variant we care about. Photo, voice,
// document, etc. are deferred follow-ups.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
}

// User is the sender of a message.
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
}

// Chat is the conversation a message belongs to.
type Chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
}

// CallbackQuery is the inline-keyboard callback variant used by
// permission prompts.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from,omitempty"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// InlineKeyboardMarkup is Telegram's inline keyboard payload shape.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// InlineKeyboardButton is one inline keyboard button.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// API is the minimal Telegram Bot API client used by the channel.
// All methods accept ctx; cancellation propagates to the underlying
// HTTP request.
type API struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// NewAPI constructs an API with sensible defaults.
func NewAPI(baseURL, token string, httpClient *http.Client) *API {
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 0, // long-poll needs no client-level timeout
		}
	}
	return &API{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		HTTPClient: httpClient,
	}
}

// GetUpdates calls the Bot API's getUpdates method (long-poll). offset
// is the smallest update_id we still want to receive; the server
// returns updates with id ≥ offset. timeout is the long-poll cap in
// seconds.
func (a *API) GetUpdates(ctx context.Context, offset int64, timeoutSeconds int) ([]Update, error) {
	body, _ := json.Marshal(map[string]any{
		"offset":  offset,
		"timeout": timeoutSeconds,
	})
	return a.do(ctx, "getUpdates", body, parseUpdates)
}

// SendMessage sends a text message to chatID. text is truncated to
// telegramMessageLimit bytes (Telegram's hard cap).
func (a *API) SendMessage(ctx context.Context, chatID int64, text string) error {
	return a.SendMessageWithReplyMarkup(ctx, chatID, text, nil)
}

// SendMessageWithReplyMarkup sends a text message with optional Telegram
// reply_markup such as [InlineKeyboardMarkup].
func (a *API) SendMessageWithReplyMarkup(ctx context.Context, chatID int64, text string, replyMarkup any) error {
	if len(text) > telegramMessageLimit {
		text = text[:telegramMessageLimit-len(truncationSuffix)] + truncationSuffix
	}
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	body, _ := json.Marshal(payload)
	_, err := a.do(ctx, "sendMessage", body, func(b []byte) ([]Update, error) {
		// sendMessage's result is the sent Message; we don't need it.
		return nil, nil
	})
	return err
}

// AnswerCallbackQuery acknowledges an inline-keyboard callback. text may be
// empty.
func (a *API) AnswerCallbackQuery(ctx context.Context, callbackQueryID, text string) error {
	body, _ := json.Marshal(map[string]any{
		"callback_query_id": callbackQueryID,
		"text":              text,
	})
	_, err := a.do(ctx, "answerCallbackQuery", body, func(b []byte) ([]Update, error) {
		return nil, nil
	})
	return err
}

const truncationSuffix = "\n… [truncated]"

// botAPIResponse is the envelope every Bot API method returns.
type botAPIResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

func (a *API) do(ctx context.Context, method string, body []byte, parse func([]byte) ([]Update, error)) ([]Update, error) {
	if a.Token == "" {
		return nil, errors.New("telegram: API.Token is required")
	}
	url := a.BaseURL + "/bot" + a.Token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("telegram: build %s request: %w", method, redactErr(err, a.Token))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: %s transport: %w", method, redactErr(err, a.Token))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("telegram: %s read body: %w", method, err)
	}
	var env botAPIResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("telegram: %s decode envelope: %w", method, err)
	}
	if !env.OK {
		return nil, fmt.Errorf("telegram: %s failed: %s (code=%d)", method, env.Description, env.ErrorCode)
	}
	return parse(env.Result)
}

func parseUpdates(b []byte) ([]Update, error) {
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	var updates []Update
	if err := json.Unmarshal(b, &updates); err != nil {
		return nil, fmt.Errorf("telegram: decode updates: %w", err)
	}
	return updates, nil
}

// redactErr scrubs the bot token from an error's text. URL errors
// often embed the token as a path segment; we don't want it in logs.
func redactErr(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, token) {
		msg = strings.ReplaceAll(msg, token, "<redacted>")
		return errors.New(msg)
	}
	return err
}

// Status codes used by the long-poll loop to decide retry behavior.
const (
	transientRetryInitial = time.Second
	transientRetryMax     = 30 * time.Second
)
