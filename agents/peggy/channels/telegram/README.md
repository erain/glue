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

### Daemon-client mode

Standalone mode builds Peggy inside the `peggy-telegram` process. To
share one long-running Peggy process with terminal clients, start the
daemon first and run Telegram as a daemon client:

```sh
peggy serve --coding --workdir /path/to/repo
peggy-telegram --daemon
```

`--daemon` reads the same metadata file written by `peggy serve` and
`glue serve`. Override discovery with `--daemon-base-url`,
`--daemon-token`, or `--daemon-metadata`; the token also falls back to
`GLUE_DAEMON_TOKEN`. Telegram still enforces `allow_chats` before any
daemon request is sent.

Daemon-client mode also supports memory commands from allowlisted
chats without starting a model run, plus skill commands for reusable
workspace workflows:

```text
/skills
/skill triage issue=GLUE-123
/memories
/memories 20
/recall Australian Shepherd
/recall_memories preference
/forget_memory mem_123
```

You can also put daemon settings under `channels.telegram.daemon`:

```json
{
  "channels": {
    "telegram": {
      "allow_chats": [123456789],
      "daemon": {
        "enabled": true,
        "metadata": "~/.config/glue/daemon.json"
      }
    }
  }
}
```

### Coding setup

`peggy-telegram` uses the same `settings.json` as the CLI. To let
Peggy work in a trusted local checkout from Telegram, enable coding
tools and set the workspace root:

```json
{
  "coding": {
    "enabled": true,
    "work_dir": "/path/to/repo",
    "allowed_binaries": ["go", "git", "make"],
    "allow_overwrite": false
  },
  "channels": {
    "telegram": {
      "allow_chats": [123456789]
    }
  }
}
```

Then run the bot from the same host that has that checkout:

```sh
export PEGGY_TELEGRAM_TOKEN='123456:ABCDEF...'
peggy-telegram
```

When the model asks to run `write_file` or `shell_exec`, Telegram
shows inline buttons for `Deny`, `Allow once`, `Allow session`, and
`Allow target`. Read-only coding tools do not prompt.

## Settings reference

```json
{
  "channels": {
    "telegram": {
      "bot_token_env": "PEGGY_TELEGRAM_TOKEN",
      "allow_chats": [123456789],
      "long_poll_timeout_seconds": 30,
      "api_base_url": "",
      "daemon": {
        "enabled": false,
        "metadata": "~/.config/glue/daemon.json",
        "base_url": "",
        "token": ""
      }
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
| `daemon.enabled` | `false` | When true, Telegram connects to a running Peggy daemon instead of constructing Peggy in-process. |
| `daemon.metadata` | glue daemon metadata path | Connection metadata written by `peggy serve` / `glue serve`. |
| `daemon.base_url` | metadata | Explicit daemon base URL override. |
| `daemon.token` | metadata or `GLUE_DAEMON_TOKEN` | Explicit bearer token override. Prefer metadata/env over committing tokens. |

## Session-id namespacing

Each Telegram chat maps to a Peggy session id `telegram:<chat_id>` —
e.g. `telegram:123456789`. This namespacing keeps Telegram
conversations distinct from CLI sessions (`default`, `--session foo`)
and from Peggy's curated `__memories__` session inside a single
sqlite store. `Agent.SearchSessions` can scope to one channel by
filtering on the session id (FTS5 prefix search is a planned
follow-up; exact-match works today).

## Permissions

When Peggy coding tools are enabled in the shared `settings.json`,
side-effecting tool calls (`write_file`, `shell_exec`) are confirmed in
Telegram with inline keyboard buttons:

- `Deny`
- `Allow once`
- `Allow session`
- `Allow target`

Permission requests are sent only to the same allowlisted chat whose
message triggered the prompt. Decisions remembered for a session or
target live only in the running `peggy-telegram` process in standalone
mode, or in the running `peggy serve` process in daemon-client mode.
Daemon remembered decisions are scoped to the daemon client id, so a
Telegram allow does not silently authorize a terminal request. If the
bot is stopped, the request times out, or the callback comes from a
non-allowlisted chat, Peggy denies the side effect and surfaces that
denial to the model as a tool result.

Read-only tools such as `read_file`, `git_diff_branch`, and
`git_log_branch` do not prompt.

Peggy-level permission tiers can make Telegram stricter or looser than
other clients:

```json
{
  "permissions": {
    "default_tier": "prompt",
    "channels": {
      "cli": "trusted",
      "telegram": "read_only"
    }
  }
}
```

`prompt` keeps the inline keyboard flow, `read_only` denies
side-effecting tools before any Telegram prompt is sent, and `trusted`
allows side-effecting tools without prompting. `trusted` still keeps the
coding tool constraints from Peggy settings, including workspace root,
binary allowlist, overwrite policy, timeouts, and output limits.

## What Peggy on Telegram supports today

- Text-message-only inbound and outbound. Photos / voice / documents /
  stickers are dropped.
- Long-polling. Webhook-mode is a follow-up.
- One Telegram bot per process. It can run standalone or as a daemon
  client for one shared Peggy process.
- Replies are sent as one message per turn, after the model finishes.
  Edit-in-place streaming and "thinking…" placeholders are a
  follow-up (Telegram rate-limits message edits and the trade-offs
  warrant their own pass).
- Replies exceeding Telegram's 4096-character limit are truncated
  with a `… [truncated]` suffix; full responses are still in the
  session transcript / sqlite store.
- Inline-keyboard permission prompts for side-effecting coding tools
  in allowlisted chats.
- Daemon-client skill commands for listing and running reusable
  workspace skills from chat.
- Daemon-client memory commands for listing, recall search,
  memories-only recall, and curated-memory deletion.
- Optional Peggy permission tiers for prompt/read-only/trusted channel
  behavior.

## What's coming

- Edit-in-place streaming for replies (delta → edited message).
- Persistent per-user permission policy across daemon restarts.
- Webhook-mode (push-based) for low-latency hosted setups.
- `Agent.SearchSessions` channel-prefix filter so the user can scope
  recall to "things we talked about on Telegram."
