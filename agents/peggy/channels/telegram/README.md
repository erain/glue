# Peggy on Telegram

The first concrete `peggy.Channel`. Text-message-only, long-polling,
gated by a chat-id allowlist. Built on the pattern designed in
[`docs/adr/0008-channel-adapter.md`](../../../../docs/adr/0008-channel-adapter.md).

## One-time setup

1. **Create a bot.** Open a chat with
   [`@BotFather`](https://t.me/BotFather) on Telegram, run `/newbot`,
   give it a name and a username. Save the token it gives you.

2. **Pick a token environment variable.** The default is
   `PEGGY_TELEGRAM_TOKEN`; override with
   `channels.telegram.bot_token_env` in your `settings.json`. Export
   the variable in your shell or in whatever process supervisor you
   use:

   ```sh
   export PEGGY_TELEGRAM_TOKEN='123456:ABCDEF...'
   ```

3. **Find your chat id.** Send any message to your bot, then run:

   ```sh
   curl -s "https://api.telegram.org/bot$PEGGY_TELEGRAM_TOKEN/getUpdates" | jq '.result[0].message.chat.id'
   ```

4. **Configure the allowlist.** Edit `~/.config/peggy/settings.json`:

   ```json
   {
     "channels": {
       "telegram": {
         "allow_chats": [123456789]
       }
     }
   }
   ```

   **An empty `allow_chats` means refuse-all.** That's the safe
   default — running the bot before you've added your chat id won't
   leak responses to strangers, but it also won't reply to you.

5. **Run.**

   ```sh
   peggy-telegram
   ```

   SIGINT / SIGTERM stops cleanly.

## Settings reference

```json
{
  "channels": {
    "telegram": {
      "bot_token_env": "PEGGY_TELEGRAM_TOKEN",
      "allow_chats": [123456789],
      "long_poll_timeout_seconds": 30,
      "api_base_url": ""
    }
  }
}
```

| Field | Default | Notes |
|---|---|---|
| `bot_token_env` | `PEGGY_TELEGRAM_TOKEN` | Env var holding the BotFather token. |
| `allow_chats` | `[]` (refuse-all) | Telegram chat ids permitted to reach the agent. |
| `long_poll_timeout_seconds` | `30` | Seconds the server waits before returning an empty update set. Clamped to 60. |
| `api_base_url` | `https://api.telegram.org` | Override for tests / private mirrors. |

## Session-id namespacing

Each Telegram chat maps to a Peggy session id `telegram:<chat_id>` —
e.g. `telegram:123456789`. This namespacing keeps Telegram
conversations distinct from CLI sessions (`default`, `--session foo`)
and from Peggy's curated `__memories__` session inside a single
sqlite store. `Agent.SearchSessions` can scope to one channel by
filtering on the session id (FTS5 prefix search is a planned
follow-up; exact-match works today).

## What v0.1 supports

- Text-message-only inbound and outbound. Photos / voice / documents /
  stickers are dropped.
- Long-polling. Webhook-mode is a follow-up.
- One Telegram bot per process. Multi-bot in one process is an M3
  concern.
- Replies are sent as one message per turn, after the model finishes.
  Edit-in-place streaming and "thinking…" placeholders are a
  follow-up (Telegram rate-limits message edits and the trade-offs
  warrant their own pass).
- Replies exceeding Telegram's 4096-character limit are truncated
  with a `… [truncated]` suffix; full responses are still in the
  session transcript / sqlite store.

## What's coming

- Edit-in-place streaming for replies (delta → edited message).
- Per-user permission policy (gated by the M2 `Permission` interface).
- Webhook-mode (push-based) for low-latency hosted setups.
- `Agent.SearchSessions` channel-prefix filter so the user can scope
  recall to "things we talked about on Telegram."
