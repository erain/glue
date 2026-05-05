// Package openrouter implements a Glue provider for OpenRouter
// (https://openrouter.ai), an OpenAI-compatible aggregator that routes
// requests across many underlying model providers.
//
// It supports text streaming, OpenAI-shape tool calling, and reasoning
// deltas (mapped from OpenRouter's "reasoning" delta field). Models are
// addressed by their OpenRouter id, for example "openrouter/free" (a
// meta-route that picks a free underlying model) or
// "anthropic/claude-3.5-sonnet".
//
// OpenRouter sends SSE comment lines ("data: " is omitted; the line
// begins with ":") as keep-alives during routing/cold-start; the
// stream parser silently drops them.
package openrouter
