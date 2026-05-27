// Package loop contains Glue's provider-agnostic agent loop.
//
// The loop streams assistant responses from a provider, executes requested
// tools, appends tool results, and repeats until the provider stops or the
// context is canceled. It must not depend on the public glue package,
// provider packages, stores, CLI code, or Markdown context discovery.
//
// The entry point is [Run], which executes a [RunRequest] until the
// provider stops or the context is canceled. Tools run sequentially in
// source order by default; set RunRequest.Parallel to dispatch a single
// assistant message's tool calls concurrently while preserving
// transcript order. RunRequest.MaxTurns bounds the turn count and
// surfaces budget exhaustion as StopReasonMaxTurns.
package loop
