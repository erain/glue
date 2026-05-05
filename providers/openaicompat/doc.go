// Package openaicompat implements the shared streaming, tool-call, and
// convert logic for OpenAI-style chat-completions endpoints used by the
// concrete providers under providers/. Vendor packages such as
// providers/nvidia and providers/openrouter wrap [New] with their own
// defaults (base URL, API key env name, attribution headers, etc.) so
// downstream callers see a typed per-vendor constructor while sharing
// one provider implementation.
//
// The streaming reader silently drops SSE comment lines (lines whose
// first byte is ':') so backends that emit keep-alive comments during
// cold routing — OpenRouter is one — do not interfere with parsing.
//
// Reasoning content arrives under either delta.reasoning (OpenRouter)
// or delta.reasoning_content (NVIDIA). Both are accepted and emitted as
// loop.ProviderEventThinkingDelta.
package openaicompat
