package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/erain/glue/agents/peggy"
)

// ChannelName is the stable identifier for this channel.
const ChannelName = "telegram"

// Options configures a Channel. Most fields are optional with sensible
// defaults; the only required pieces are Peggy and a populated Config.
type Options struct {
	// Peggy is the agent that handles inbound messages. Required.
	Peggy *peggy.Peggy

	// Config is the decoded settings.channels.telegram block.
	Config Config

	// Token is the bot token. If empty, the channel reads
	// os.Getenv(Config.BotTokenEnv) at construction time.
	Token string

	// HTTPClient is forwarded to the API. nil → default.
	HTTPClient *http.Client

	// Stderr collects diagnostics (rejected messages, transient
	// errors, etc.). Defaults to os.Stderr. The channel never logs
	// the bot token here.
	Stderr io.Writer
}

// Channel is the peggy.Channel implementation for Telegram.
type Channel struct {
	peggy   *peggy.Peggy
	cfg     Config
	api     *API
	allowed map[int64]struct{}
	stderr  io.Writer
}

// New constructs a Channel. Returns an error when required fields are
// missing — the bot token in particular surfaces early rather than
// failing silently inside the poll loop.
func New(opts Options) (*Channel, error) {
	if opts.Peggy == nil {
		return nil, errors.New("telegram: Peggy is required")
	}
	cfg, err := DecodeConfig(nil) // re-apply defaults defensively
	if err != nil {
		return nil, err
	}
	// Merge user-supplied config over defaults.
	if opts.Config.BotTokenEnv != "" {
		cfg.BotTokenEnv = opts.Config.BotTokenEnv
	}
	if opts.Config.LongPollTimeoutSeconds > 0 {
		cfg.LongPollTimeoutSeconds = opts.Config.LongPollTimeoutSeconds
	}
	if cfg.LongPollTimeoutSeconds > maxLongPollSecs {
		cfg.LongPollTimeoutSeconds = maxLongPollSecs
	}
	if opts.Config.APIBaseURL != "" {
		cfg.APIBaseURL = opts.Config.APIBaseURL
	}
	cfg.AllowChats = append([]int64(nil), opts.Config.AllowChats...)

	token := opts.Token
	if token == "" {
		token = strings.TrimSpace(os.Getenv(cfg.BotTokenEnv))
	}
	if token == "" {
		return nil, fmt.Errorf("telegram: bot token is required (set $%s or Options.Token)", cfg.BotTokenEnv)
	}

	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	allowed := make(map[int64]struct{}, len(cfg.AllowChats))
	for _, id := range cfg.AllowChats {
		allowed[id] = struct{}{}
	}

	return &Channel{
		peggy:   opts.Peggy,
		cfg:     cfg,
		api:     NewAPI(cfg.APIBaseURL, token, opts.HTTPClient),
		allowed: allowed,
		stderr:  stderr,
	}, nil
}

// Name implements peggy.Channel.
func (c *Channel) Name() string { return ChannelName }

// Run drives the long-poll loop. Returns nil when ctx is cancelled.
// Run must be called exactly once per Channel value.
func (c *Channel) Run(ctx context.Context) error {
	if c == nil || c.api == nil {
		return errors.New("telegram: channel not initialised")
	}
	if len(c.allowed) == 0 {
		fmt.Fprintln(c.stderr, "telegram: allow_chats is empty — refusing all inbound messages (set allow_chats in settings.json to enable)")
	}
	var (
		offset    int64
		backoff   = transientRetryInitial
		maxBackoff = transientRetryMax
	)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		updates, err := c.api.GetUpdates(ctx, offset, c.cfg.LongPollTimeoutSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintf(c.stderr, "telegram: getUpdates: %v (backing off %s)\n", err, backoff)
			if !c.sleep(ctx, backoff) {
				return nil
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = transientRetryInitial
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			c.handleUpdate(ctx, u)
		}
	}
}

func (c *Channel) sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// handleUpdate processes one update. It gates on the allowlist,
// constructs the session id, drives the prompt, and replies with the
// model's text.
func (c *Channel) handleUpdate(ctx context.Context, u Update) {
	if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
		return
	}
	chatID := u.Message.Chat.ID
	if _, ok := c.allowed[chatID]; !ok {
		fmt.Fprintf(c.stderr, "telegram: dropping message from non-allowlisted chat %d\n", chatID)
		return
	}

	sessionID := peggy.ChannelSessionID(ChannelName, strconv.FormatInt(chatID, 10))
	var buf bytes.Buffer
	text, err := c.peggy.Prompt(ctx, sessionID, u.Message.Text, &buf)
	if err != nil {
		fmt.Fprintf(c.stderr, "telegram: prompt failed for chat %d: %v\n", chatID, err)
		// Reply with a short error rather than going silent — the user
		// sent something and deserves an acknowledgment.
		_ = c.api.SendMessage(ctx, chatID, "(I hit an error responding. Check the agent logs.)")
		return
	}
	reply := strings.TrimSpace(text)
	if reply == "" {
		reply = strings.TrimSpace(buf.String())
	}
	if reply == "" {
		fmt.Fprintf(c.stderr, "telegram: prompt for chat %d produced no text\n", chatID)
		return
	}
	if err := c.api.SendMessage(ctx, chatID, reply); err != nil {
		fmt.Fprintf(c.stderr, "telegram: send to chat %d: %v\n", chatID, err)
	}
}

// Compile-time assertion that *Channel satisfies peggy.Channel.
var _ peggy.Channel = (*Channel)(nil)
