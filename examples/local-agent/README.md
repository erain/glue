# Local Agent Example

This example is a small Gemini-backed CLI agent built directly on the Glue
library. It registers a `local_time` tool, streams the assistant text to
stdout, and persists sessions through `stores/file`.

The implementation lives in [`main.go`](main.go) and is intentionally
short: it shows the full shape of a Glue application — provider, store,
tool registration, session management, and event-stream output — in under
120 lines.

## Run

Set a Gemini API key, then run:

```sh
export GEMINI_API_KEY=...
go run ./examples/local-agent \
  --prompt "Use local_time for America/Toronto and summarize it." \
  --id demo
```

Flags:

- `--prompt` (required) — prompt text
- `--id` — session id (default `example`)
- `--model` — Gemini model id (default `gemini-2.5-flash`)
- `--store` — session store directory (default `.glue/example-sessions`)

The session is persisted to disk, so re-running with the same `--id`
continues the conversation.

## Test

```sh
go test ./examples/local-agent
```

Three offline tests cover the `local_time` tool's happy path, missing
timezone arg, and invalid JSON arg. The live test
(`TestLiveLocalAgent`) is gated behind `GEMINI_API_KEY` and skipped in CI.

## Compare to `cmd/glue`

`cmd/glue/run` is the generic local CLI; this example is a self-contained
program showing how to build a tool-calling agent in your own `main.go`.
The two share `glue.NewAgent` + `gemini.New` + `stores/file` + the same
event-streaming pattern.
