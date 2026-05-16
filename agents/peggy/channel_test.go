package peggy

import (
	"encoding/json"
	"testing"
)

func TestChannelSessionID(t *testing.T) {
	cases := []struct {
		channel string
		id      string
		want    string
	}{
		{"telegram", "12345", "telegram:12345"},
		{"TELEGRAM", "abc", "telegram:abc"},      // lower-cased
		{"  telegram  ", "9", "telegram:9"},     // trimmed
		{"", "7", "7"},                           // no-op for empty channel
		{"slack", "U123", "slack:U123"},
	}
	for _, tc := range cases {
		if got := ChannelSessionID(tc.channel, tc.id); got != tc.want {
			t.Errorf("ChannelSessionID(%q, %q) = %q, want %q", tc.channel, tc.id, got, tc.want)
		}
	}
}

func TestSettings_ChannelsRoundTrip(t *testing.T) {
	src := map[string]any{
		"provider": "openrouter",
		"channels": map[string]any{
			"telegram": map[string]any{
				"bot_token_env": "PEGGY_TELEGRAM_TOKEN",
				"allow_chats":   []int{12345, 67890},
			},
		},
	}
	raw, _ := json.Marshal(src)
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := s.Channels["telegram"]; !ok {
		t.Fatalf("Channels[telegram] missing: %+v", s.Channels)
	}
	// The raw subtree should preserve the JSON shape verbatim so each
	// channel package can decode it.
	var sub struct {
		BotTokenEnv string  `json:"bot_token_env"`
		AllowChats  []int64 `json:"allow_chats"`
	}
	if err := json.Unmarshal(s.Channels["telegram"], &sub); err != nil {
		t.Fatalf("decode subtree: %v", err)
	}
	if sub.BotTokenEnv != "PEGGY_TELEGRAM_TOKEN" {
		t.Errorf("bot_token_env = %q", sub.BotTokenEnv)
	}
	if len(sub.AllowChats) != 2 || sub.AllowChats[0] != 12345 {
		t.Errorf("allow_chats = %v", sub.AllowChats)
	}
}
