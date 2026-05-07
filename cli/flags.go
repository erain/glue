// Package cli provides small helpers downstream agents share with the
// canonical cmd/glue runner. Today the only helper is StandardFlags, a
// flag.FlagSet wiring for the six options every multi-provider Glue
// agent accepts: --provider, --model, --id, --store, --work,
// --max-turns.
//
// The package is intentionally thin: a full CLI runner lives at
// cmd/glue. cli is for downstream agents that want to share the flag
// shape without re-registering each variable themselves.
package cli

import (
	"flag"
)

// StandardConfig holds the parsed values of the six common flags. Read
// it via the closure returned from RegisterStandardFlags after calling
// fs.Parse.
type StandardConfig struct {
	// Provider is the provider list. A bare name selects one provider;
	// a comma-separated list (e.g. "nvidia,openrouter,gemini") asks
	// the agent to fail over to the first one whose API key is set.
	// Pair with glue.WithFailover and the providers registry to act
	// on the list.
	Provider string

	// Model is the model id. Empty means "use the active provider's
	// DefaultModel from the registry."
	Model string

	// ID is the session id used for file-backed transcripts.
	ID string

	// Store is the directory passed to stores/file.New.
	Store string

	// Work is the working directory used for AGENTS.md / skills /
	// roles discovery, and as the cwd for filesystem and git tools.
	Work string

	// MaxTurns is the loop budget cap for one prompt; zero or negative
	// is forwarded as-is so glue.AgentOptions / RunRequest can fall
	// back to their own default.
	MaxTurns int
}

// StandardFlagDefaults are the values RegisterStandardFlags wires by
// default. Exported so agents that want to override one or two without
// re-registering everything can read the canonical default.
var StandardFlagDefaults = StandardConfig{
	Provider: "nvidia",
	Model:    "",
	ID:       "default",
	Store:    ".glue/sessions",
	Work:     ".",
	MaxTurns: 32,
}

// RegisterStandardFlags wires the six common flags onto fs and returns
// a getter that reads them after fs.Parse(). Defaults match
// StandardFlagDefaults. Help text mentions failover semantics so agents
// don't need to redocument them.
//
// Callers may override individual defaults by passing a non-nil
// *StandardConfig — only fields you set are applied; zero values fall
// back to StandardFlagDefaults. Pass nil to use defaults verbatim.
func RegisterStandardFlags(fs *flag.FlagSet, defaults *StandardConfig) func() StandardConfig {
	d := StandardFlagDefaults
	if defaults != nil {
		if defaults.Provider != "" {
			d.Provider = defaults.Provider
		}
		if defaults.Model != "" {
			d.Model = defaults.Model
		}
		if defaults.ID != "" {
			d.ID = defaults.ID
		}
		if defaults.Store != "" {
			d.Store = defaults.Store
		}
		if defaults.Work != "" {
			d.Work = defaults.Work
		}
		if defaults.MaxTurns != 0 {
			d.MaxTurns = defaults.MaxTurns
		}
	}

	provider := fs.String("provider", d.Provider, "provider name; comma-separated list (e.g. 'nvidia,openrouter,gemini') asks the agent to fail over to the first whose API key is set")
	model := fs.String("model", d.Model, "model id (defaults to the active provider's DefaultModel)")
	id := fs.String("id", d.ID, "session id (file-backed sessions key off this)")
	store := fs.String("store", d.Store, "session store directory")
	work := fs.String("work", d.Work, "working directory (used for AGENTS.md / skills / roles discovery and as the cwd for filesystem tools)")
	maxTurns := fs.Int("max-turns", d.MaxTurns, "loop budget — caps total assistant turns")

	return func() StandardConfig {
		return StandardConfig{
			Provider: *provider,
			Model:    *model,
			ID:       *id,
			Store:    *store,
			Work:     *work,
			MaxTurns: *maxTurns,
		}
	}
}
