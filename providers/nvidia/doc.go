// Package nvidia implements a Glue provider for the NVIDIA build inference
// API (https://build.nvidia.com), which exposes an OpenAI-compatible
// chat-completions endpoint at https://integrate.api.nvidia.com/v1.
//
// It supports text streaming, OpenAI-shape tool calling, and reasoning
// content deltas. Models are addressed by their build.nvidia.com path,
// for example "moonshotai/kimi-k2.6" or "meta/llama-3.3-70b-instruct".
package nvidia
