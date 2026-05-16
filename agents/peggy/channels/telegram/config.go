package telegram

import (
	"encoding/json"
	"fmt"
)

// Config is the decoded form of settings.json's
// channels.telegram subtree.
type Config struct {
	// BotTokenEnv names the environment variable that holds the bot
	// token (issued by BotFather). The variable's value is loaded at
	// channel construction time. Default: PEGGY_TELEGRAM_TOKEN.
	BotTokenEnv string `json:"bot_token_env"`

	// AllowChats restricts inbound messages to the listed chat ids.
	// Empty = refuse-all (no inbound message is processed). Channels
	// MUST gate on this — see ADR-0008 §5.
	AllowChats []int64 `json:"allow_chats"`

	// LongPollTimeoutSeconds caps the timeout passed to getUpdates.
	// 30 is a reasonable default that balances latency vs. Telegram's
	// connection limits. Zero falls back to the default.
	LongPollTimeoutSeconds int `json:"long_poll_timeout_seconds"`

	// APIBaseURL overrides the Telegram API root. Empty falls back to
	// https://api.telegram.org. Used by tests to point at an
	// httptest.NewServer.
	APIBaseURL string `json:"api_base_url"`
}

const (
	defaultBotTokenEnv   = "PEGGY_TELEGRAM_TOKEN"
	defaultLongPollSecs  = 30
	defaultAPIBaseURL    = "https://api.telegram.org"
	maxLongPollSecs      = 60
	telegramMessageLimit = 4096
)

// DecodeConfig parses the raw channels.telegram JSON subtree and fills
// in defaults. A nil / empty raw payload yields a default Config.
func DecodeConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &c); err != nil {
			return Config{}, fmt.Errorf("telegram: decode config: %w", err)
		}
	}
	if c.BotTokenEnv == "" {
		c.BotTokenEnv = defaultBotTokenEnv
	}
	if c.LongPollTimeoutSeconds <= 0 {
		c.LongPollTimeoutSeconds = defaultLongPollSecs
	}
	if c.LongPollTimeoutSeconds > maxLongPollSecs {
		c.LongPollTimeoutSeconds = maxLongPollSecs
	}
	if c.APIBaseURL == "" {
		c.APIBaseURL = defaultAPIBaseURL
	}
	return c, nil
}
