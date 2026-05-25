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
	"unicode/utf8"

	"github.com/erain/glue/agents/peggy"
)

// ChannelName is the stable identifier for this channel.
const ChannelName = "telegram"

// Options configures a Channel. Most fields are optional with sensible
// defaults. Standalone mode requires Peggy; daemon-client mode requires
// Daemon.
type Options struct {
	// Peggy is the agent that handles inbound messages. Required.
	// Ignored when Daemon is non-nil.
	Peggy *peggy.Peggy

	// Daemon handles inbound messages by talking to a running Peggy
	// daemon. When set, Peggy may be nil.
	Daemon *DaemonClient

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

	// Permission, when non-nil, handles inline-keyboard responses for
	// Peggy side-effect permission requests. The same value must be
	// passed to peggy.New via peggy.Options.Permission.
	Permission *Permission
}

// Channel is the peggy.Channel implementation for Telegram.
type Channel struct {
	peggy   *peggy.Peggy
	daemon  *DaemonClient
	cfg     Config
	api     *API
	allowed map[int64]struct{}
	stderr  io.Writer
	perm    *Permission
}

// New constructs a Channel. Returns an error when required fields are
// missing — the bot token in particular surfaces early rather than
// failing silently inside the poll loop.
func New(opts Options) (*Channel, error) {
	if opts.Peggy == nil && opts.Daemon == nil {
		return nil, errors.New("telegram: Peggy or Daemon is required")
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

	ch := &Channel{
		peggy:   opts.Peggy,
		daemon:  opts.Daemon,
		cfg:     cfg,
		api:     NewAPI(cfg.APIBaseURL, token, opts.HTTPClient),
		allowed: allowed,
		stderr:  stderr,
		perm:    opts.Permission,
	}
	if ch.perm != nil {
		ch.perm.attach(ch.api, ch.allowed, stderr)
	}
	return ch, nil
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
		offset     int64
		backoff    = transientRetryInitial
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
			if u.CallbackQuery != nil {
				c.handleCallback(ctx, *u.CallbackQuery)
				continue
			}
			go c.handleUpdate(ctx, u)
		}
	}
}

func (c *Channel) handleCallback(ctx context.Context, cb CallbackQuery) {
	chatID, ok := callbackChatID(cb)
	if !ok {
		if c.perm != nil {
			_ = c.perm.handleCallback(ctx, cb)
		}
		return
	}
	if _, ok := c.allowed[chatID]; !ok {
		fmt.Fprintf(c.stderr, "telegram: dropping callback from non-allowlisted chat %d\n", chatID)
		if c.perm != nil {
			_ = c.perm.handleCallback(ctx, cb)
		}
		return
	}
	if c.perm != nil && c.perm.handleCallback(ctx, cb) {
		return
	}
	if c.daemon != nil && c.daemon.HandleCallback(ctx, cb, c.api) {
		return
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
	var text string
	var err error
	if c.daemon != nil {
		if reply, handled, commandErr := c.daemon.Command(ctx, sessionID, u.Message.Text, c.api, chatID); handled {
			if commandErr != nil {
				fmt.Fprintf(c.stderr, "telegram: daemon command failed for chat %d: %v\n", chatID, commandErr)
				reply = "Command error: " + commandErr.Error()
			}
			if strings.TrimSpace(reply) != "" {
				if err := sendTelegramText(ctx, c.api, chatID, strings.TrimSpace(reply)); err != nil {
					fmt.Fprintf(c.stderr, "telegram: send to chat %d: %v\n", chatID, err)
				}
			}
			return
		}
		text, err = c.daemon.Prompt(ctx, sessionID, u.Message.Text, c.api, chatID)
	} else {
		text, err = c.peggy.Prompt(ctx, sessionID, u.Message.Text, &buf)
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "telegram: prompt failed for chat %d: %v\n", chatID, err)
		// Reply with a short error rather than going silent — the user
		// sent something and deserves an acknowledgment.
		_ = sendTelegramText(ctx, c.api, chatID, "(I hit an error responding. Check the agent logs.)")
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
	if err := sendTelegramText(ctx, c.api, chatID, reply); err != nil {
		fmt.Fprintf(c.stderr, "telegram: send to chat %d: %v\n", chatID, err)
	}
}

func sendTelegramText(ctx context.Context, api *API, chatID int64, text string) error {
	for _, chunk := range telegramTextChunks(text) {
		if err := api.SendMessage(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func telegramTextChunks(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	chunks := []string{}
	for len(text) > telegramMessageLimit {
		cut := telegramTextChunkCut(text, telegramMessageLimit)
		if cut <= 0 || cut > len(text) {
			cut = telegramMessageLimit
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

func telegramTextChunkCut(text string, limit int) int {
	if len(text) <= limit {
		return len(text)
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	if cut == 0 {
		_, size := utf8.DecodeRuneInString(text)
		return size
	}
	window := text[:cut]
	floor := cut / 2
	if idx := strings.LastIndex(window[floor:], "\n"); idx >= 0 {
		return floor + idx + 1
	}
	if idx := strings.LastIndex(window[floor:], " "); idx >= 0 {
		return floor + idx + 1
	}
	return cut
}

// Compile-time assertion that *Channel satisfies peggy.Channel.
var _ peggy.Channel = (*Channel)(nil)
