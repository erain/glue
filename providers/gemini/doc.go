// Package gemini adapts Google's Gemini API to Glue's provider interface
// using the google.golang.org/genai SDK.
//
// It streams text and thinking deltas, converts Glue tools to Gemini
// function declarations, and maps function calls and results across the
// normalized loop types. The model comes from AgentOptions.Model, a
// per-call WithModel, or the provider default. The API key is taken from
// Options.APIKey or the GEMINI_API_KEY environment variable.
package gemini
