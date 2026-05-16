package telegram

import (
	"encoding/json"
	"testing"
)

func TestDecodeConfig_Defaults(t *testing.T) {
	c, err := DecodeConfig(nil)
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	if c.BotTokenEnv != defaultBotTokenEnv {
		t.Errorf("BotTokenEnv = %q", c.BotTokenEnv)
	}
	if c.LongPollTimeoutSeconds != defaultLongPollSecs {
		t.Errorf("LongPollTimeoutSeconds = %d", c.LongPollTimeoutSeconds)
	}
	if c.APIBaseURL != defaultAPIBaseURL {
		t.Errorf("APIBaseURL = %q", c.APIBaseURL)
	}
}

func TestDecodeConfig_HappyPath(t *testing.T) {
	raw := json.RawMessage(`{
        "bot_token_env": "MY_TOKEN",
        "allow_chats": [1, 2, 3],
        "long_poll_timeout_seconds": 10,
        "api_base_url": "http://localhost:9999"
    }`)
	c, err := DecodeConfig(raw)
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	if c.BotTokenEnv != "MY_TOKEN" || len(c.AllowChats) != 3 || c.AllowChats[2] != 3 ||
		c.LongPollTimeoutSeconds != 10 || c.APIBaseURL != "http://localhost:9999" {
		t.Errorf("decoded wrong: %+v", c)
	}
}

func TestDecodeConfig_ClampsLongPoll(t *testing.T) {
	raw := json.RawMessage(`{"long_poll_timeout_seconds": 9999}`)
	c, _ := DecodeConfig(raw)
	if c.LongPollTimeoutSeconds != maxLongPollSecs {
		t.Errorf("clamp failed: %d", c.LongPollTimeoutSeconds)
	}
}

func TestDecodeConfig_InvalidJSONErrors(t *testing.T) {
	if _, err := DecodeConfig([]byte("{not json")); err == nil {
		t.Fatal("expected error")
	}
}

func TestDecodeConfig_NullPayload(t *testing.T) {
	c, err := DecodeConfig([]byte("null"))
	if err != nil {
		t.Fatalf("null should be tolerated: %v", err)
	}
	if c.BotTokenEnv != defaultBotTokenEnv {
		t.Errorf("defaults not applied on null: %+v", c)
	}
}
