# ADR 0001: Make The Agent Loop Reusable And Provider-Agnostic

## Status

Accepted.

## Context

Glue is inspired by Flue and pi-mono. Flue shows a productive agent/session/skill
developer experience, while pi-mono shows a clean runtime loop: stream model
events, execute tool calls, append tool results, and continue until the model
stops.

The loop is the part most likely to be reused outside the initial Gemini and CLI
use cases. If it depends on Gemini, stores, CLI rendering, Markdown discovery, or
filesystem layout, future providers and applications will be harder to add.

## Decision

Glue will implement the agent loop as a standalone package.

The loop accepts normalized messages, tools, provider, model/options, and a
system prompt. It emits normalized events and returns newly produced transcript
messages. It does not import the public `glue` package, the Gemini provider
package, stores, CLI code, or Markdown context loaders.

P0 uses deterministic sequential tool execution. Parallel execution can be added
later as an option without changing the default transcript semantics.

## Consequences

- Providers must translate their native APIs into Glue's normalized event and
  message model.
- The public `glue` package can focus on ergonomics: agents, sessions, skills,
  roles, stores, and options.
- The CLI can stream loop/session events without owning runtime behavior.
- Tests can validate the loop with fake providers before Gemini integration.
- Some duplication may exist between low-level loop types and high-level
  user-facing aliases, but this is preferable to coupling the loop upward.
