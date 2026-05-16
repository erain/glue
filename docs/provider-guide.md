# Provider Plugin Guide

Glue's provider abstraction is small on purpose: any backend that can
stream a sequence of "text part / tool call / done" events into a
language can satisfy it. This guide walks through what the [`Provider`]
interface promises, what events to emit and in what order, and how to
verify a new provider in tests.

A complete, runnable reference implementation lives at
[`../examples/echo-provider`](../examples/echo-provider). It compiles,
passes tests, and is the shortest path from "I have a backend" to "Glue
can drive it." Copy that package and replace the body of `stream()`
with your network code.

[`Provider`]: ../types.go

## The interface

```go
type Provider interface {
    Stream(ctx context.Context, req ProviderRequest) (<-chan ProviderEvent, error)
}
```

`Stream` returns immediately with a channel; the goroutine that fills the
channel does the work. The loop reads until the channel is closed.

`ProviderRequest` carries everything the loop knows about the next
assistant turn:

| Field          | Type                | Notes                                                         |
|----------------|---------------------|---------------------------------------------------------------|
| `Model`        | `string`            | Either the agent default or a per-call `WithModel` override.   |
| `SystemPrompt` | `string`            | Already includes any AGENTS.md, skill catalog, and role block. |
| `Messages`     | `[]Message`         | Full transcript + the new user message; do not mutate.         |
| `Tools`        | `[]ToolSpec`        | Specs only — no executor — for forwarding to the model.        |
| `Options`      | `map[string]any`    | Provider-specific settings (e.g. `temperature`).               |

## Event contract

A correct provider emits exactly the following sequence:

1. **Exactly one** `ProviderEventStart`. Optionally include a partially
   populated `Message` so the loop has model/role context. The loop
   handles both forms.
2. **Zero or more** of:
   - `ProviderEventTextDelta` with `Delta` set
   - `ProviderEventThinkingDelta` with `Delta` set
   - `ProviderEventToolCall` with `ToolCall` set
3. **Exactly one** terminal event:
   - `ProviderEventDone` with the final `Message` (the loop overwrites
     its accumulated message with this one — make sure your message is
     complete, including all content parts), **or**
   - `ProviderEventError` with `Error` populated.
4. Then **close the channel.**

`ProviderEventStart` and `ProviderEventDone` are required because the
loop's `runAssistantTurn` uses them to bracket text accumulation and to
decide whether to keep streaming. Closing the channel without a Done
event is treated as an error.

## Required and optional behaviors

**Required**

- `req.Messages` is owned by the caller; do not mutate it. Clone before
  rewriting.
- Honor `ctx` cancellation when sending into the events channel —
  otherwise a slow consumer plus a canceled context can deadlock. The
  echo example uses a `select { case <-ctx.Done(): case events <- e: }`
  helper.
- Provide a stable role on the final `Message` (typically
  `MessageRoleAssistant`) and a `CreatedAt` timestamp.
- Map your backend's stop reason to `loop.StopReason` (use
  `StopReasonStop`, `StopReasonLength`, `StopReasonToolUse`,
  `StopReasonError`, or `StopReasonCanceled`).

**Optional but recommended**

- Populate `Message.Usage` with token counts when the backend reports
  them.
- Stash backend-specific identifiers under `Message.Metadata` (e.g.
  `response_id`).
- Convert `req.Tools` into your backend's native tool/function-call
  format and emit `ProviderEventToolCall` for inbound calls.

**Out of scope for the provider**

- Tool execution. The loop runs tools; the provider only relays calls
  and ferries back the resulting tool messages on the next turn.
- Transcript accumulation across turns. The loop handles that.

## Tool-call shape

When the backend returns a tool/function call, emit:

```go
ProviderEvent{
    Type:     ProviderEventToolCall,
    ToolCall: &ToolCall{
        ID:        "<provider-supplied or synthesized>",
        Name:      "<matches a registered Tool's Name>",
        Arguments: json.RawMessage(`{...}`),
    },
}
```

`Arguments` must be a JSON object (or `{}` for empty). The loop
normalizes it before invoking the executor; non-object args become an
error tool result.

When you receive tool-result messages on the next turn, look for
messages with `Role == MessageRoleTool` and convert them into the
backend's tool-response shape. Glue groups consecutive tool-role
messages logically per turn, so a single backend tool-response payload
should be able to carry multiple `FunctionResponse`-shaped entries
where applicable.

## Testing a new provider

A provider can be unit-tested without any network access. The pattern is:

1. **Construct** the provider directly with whatever options it takes.
2. **Drive** it through the public `glue` API by passing it as
   `AgentOptions.Provider`. The loop's existing tests cover everything
   that doesn't depend on your backend semantics; your tests only need
   to cover the convert-in / convert-out logic.
3. **Assert** at the assistant message: text, tool calls, stop reason.

The echo example's tests demonstrate this:

```go
func TestEchoProviderRoundTripThroughAgent(t *testing.T) {
    agent := glue.NewAgent(glue.AgentOptions{Provider: New()})
    session, _ := agent.Session(context.Background(), "x")
    res, _ := session.Prompt(context.Background(), "hello world")
    if res.Text != "hello world" { ... }
}
```

For backends with real network calls, gate live tests on an environment
variable — see `providers/gemini/gemini_test.go`'s `TestLiveSmoke` for
the convention. CI never sets the variable, so live tests must skip
quietly there.

## Where to put the package

- **In a third-party module**: just import `glue` and put the package
  wherever makes sense. Glue's public API is stable for re-export of
  the message and event types.
- **In this repo**: under `providers/<name>/`, following the layout of
  `providers/gemini`. Update `docs/design.md`'s "Package Boundaries"
  section to list the new package and confirm the dependency direction
  (`providers/<name>` may import `glue/loop`, but the loop must not
  import the provider).

## Subscription-auth providers

Providers that authenticate via a user's existing
ChatGPT / SaaS-account subscription rather than a static API key follow
the same `Provider` contract as the rest, but have an extra fragility
budget — OAuth flows, token refresh, custom headers, vendor-specific
allowlists. Glue keeps that fragility *inside the provider package*:
the loop, the agent, and other providers never learn about the auth
shape.

The pattern is documented in
[`adr/0006-codex-provider.md`](adr/0006-codex-provider.md), which
designs `providers/codex` (ChatGPT-subscription auth, Responses
transport against `chatgpt.com`). New subscription-auth providers
should follow that same package layout (`providers/<name>/auth` for
token handling, `providers/<name>` for the `glue.Provider`
implementation), reference upstream open-source CLIs as the protocol
spec rather than copying code, and quarantine all vendor-specific
headers and base URLs in the package.

## Common mistakes

- **Aliasing the same `Message` across `Start` and `Done`.** The loop
  dereferences `event.Message` when it *reads* the event, not when you
  send it. If you send `&output` in the `Start` event and continue to
  mutate `output` (e.g., appending content as deltas arrive), the
  loop's Start handler may see a partially-populated message and then
  the subsequent `TextDelta` events will append to that content,
  effectively double-writing the text. The echo example sidesteps this
  by sending `Start` with no `Message` and constructing a fresh
  `Message` on `Done`. If you do want a Start `Message` for metadata,
  send a clone or a value-typed pointer to a separate struct, not an
  alias to the buffer you'll keep mutating.
- **Forgetting to close the channel.** The loop will hang if the
  channel never closes. Use `defer close(events)` in the goroutine.
- **Skipping the `Done` event.** Closing the channel without a Done is
  an error path, not a quiet stop.
- **Mutating `req.Messages`.** The loop and the session both keep
  references to the same slice elements. Clone if you need to rewrite.
- **Sending into the channel without watching ctx.** A canceled context
  with a still-pending send leaks the goroutine.
- **Returning errors from `Stream` for transient issues.** Prefer
  `ProviderEventError` once the goroutine has started so the loop can
  surface the error consistently with provider-internal failures.
