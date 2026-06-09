// Package providers exposes a small driver-style registry of provider
// constructors. It lets callers ask for a provider by name without
// hand-coding a switch over every shipped provider package.
//
// Each provider sub-package registers itself in init() so importing
// `_ "github.com/erain/glue/providers/nvidia"` makes that provider
// resolvable via providers.New("nvidia"). The registry holds factory
// functions, not constructed providers, so registration is cheap and
// has no I/O or credential side effects.
package providers

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/erain/glue/loop"
)

// Capabilities records harness-relevant facts about a provider's
// models, declared at registration instead of scattered through
// if-provider-name switches. The zero value means "unknown / assume
// nothing": consumers must treat absent capabilities conservatively.
type Capabilities struct {
	// ContextWindow is the default model's context window in tokens.
	// Zero means unknown.
	ContextWindow int

	// ParallelTools reports whether tool calls from one assistant turn
	// are safe to execute concurrently against this provider's models.
	ParallelTools bool

	// PromptVariant selects the system-prompt flavor assembled for
	// this provider's models: "terse" for frontier models that need
	// minimal steering, "" for the default (more explicit) variant.
	PromptVariant string

	// AutoContinue reports that the provider's models are prone to the
	// narrate-then-stop stall and benefit from the loop's bounded
	// "Please continue." nudge.
	AutoContinue bool
}

// Factory describes one registered provider.
type Factory struct {
	// New returns a fresh provider configured with package defaults
	// (no explicit API key — falls back to the provider's env var).
	New func() loop.Provider

	// DefaultModel is the model id callers should use when they have
	// no opinion. The registry exposes it so a CLI agent can present
	// `--model` defaults that vary by provider.
	DefaultModel string

	// EnvKey is the environment variable the provider reads when
	// APIKey is empty. Used by KeyAvailable.
	EnvKey string

	// Capabilities declares harness-relevant facts about the
	// provider's models. Optional; the zero value means unknown.
	Capabilities Capabilities
}

// CapabilitiesFor returns the registered capabilities for name, or the
// zero value when the provider is unknown or declared none.
func CapabilitiesFor(name string) Capabilities {
	f, ok := Lookup(name)
	if !ok {
		return Capabilities{}
	}
	return f.Capabilities
}

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a provider to the registry. Re-registering an existing
// name overwrites — driver-style packages may register from init() and
// the last importer wins.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	factories[strings.ToLower(name)] = f
}

// Lookup returns the factory for name, or false if no provider is
// registered under that name.
func Lookup(name string) (Factory, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := factories[strings.ToLower(name)]
	return f, ok
}

// New constructs a fresh provider for the registered name. Returns the
// provider, its default model, the env var it consults, and an error
// if the name is unknown.
func New(name string) (loop.Provider, string, string, error) {
	f, ok := Lookup(name)
	if !ok {
		return nil, "", "", fmt.Errorf("unknown provider %q (known: %s)", name, strings.Join(Known(), ", "))
	}
	return f.New(), f.DefaultModel, f.EnvKey, nil
}

// Known returns the registered provider names, sorted for stable
// output in error messages and CLI help text.
func Known() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for name := range factories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// KeyAvailable reports whether the registered provider's env var is
// non-empty in the process environment. Returns false for unknown
// providers and for providers registered without an EnvKey.
func KeyAvailable(name string) bool {
	f, ok := Lookup(name)
	if !ok || f.EnvKey == "" {
		return false
	}
	return strings.TrimSpace(os.Getenv(f.EnvKey)) != ""
}
