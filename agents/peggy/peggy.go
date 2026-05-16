package peggy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/erain/glue"
	"github.com/erain/glue/providers"
	"github.com/erain/glue/providers/codex"
	"github.com/erain/glue/providers/gemini"
	"github.com/erain/glue/providers/nvidia"
	"github.com/erain/glue/providers/openrouter"
	filestore "github.com/erain/glue/stores/file"
	sqlitestore "github.com/erain/glue/stores/sqlite"
)

// Options configures New. Most fields are optional.
//
// Provider and Store override anything Settings would build. Tests
// use these to inject a fake provider and an in-memory store without
// touching the filesystem.
type Options struct {
	// Settings is the parsed config. When zero, defaults apply.
	Settings Settings

	// Soul is the SOUL.md content. May be empty.
	Soul string

	// Provider, when non-nil, replaces the provider built from
	// Settings.Provider. Useful for tests and out-of-tree
	// integrations.
	Provider glue.Provider

	// Store, when non-nil, replaces the store built from
	// Settings.Store.
	Store glue.Store

	// Stderr collects diagnostic warnings (missing SOUL.md etc).
	// Defaults to os.Stderr.
	Stderr io.Writer
}

// Peggy is a constructed personal-assistant agent. Hold one per
// process; Sessions are derived as needed via Prompt.
type Peggy struct {
	agent    *glue.Agent
	store    glue.Store
	settings Settings
	soul     string
	stderr   io.Writer
}

// New builds a Peggy from the supplied Options. Settings defaults
// have already been applied by [LoadSettings]; callers that build
// Options programmatically should pass a value processed through
// [fillDefaults] (the simplest way: Settings.WithDefaults()).
func New(opts Options) (*Peggy, error) {
	settings := fillDefaults(opts.Settings)

	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	provider := opts.Provider
	if provider == nil {
		built, err := buildProvider(settings)
		if err != nil {
			return nil, err
		}
		provider = built
	}

	store := opts.Store
	if store == nil {
		built, err := buildStore(settings)
		if err != nil {
			return nil, err
		}
		store = built
	}

	systemPrompt := strings.TrimSpace(opts.Soul)

	compactor := &glue.SummarizingCompactor{
		Provider:     provider,
		Model:        settings.Model,
		TargetTokens: settings.Compaction.TargetTokens,
		KeepRecent:   settings.Compaction.KeepRecent,
	}

	agent := glue.NewAgent(glue.AgentOptions{
		Provider:            provider,
		Model:               settings.Model,
		SystemPrompt:        systemPrompt,
		Store:               store,
		Compactor:           compactor,
		CompactionThreshold: settings.Compaction.Threshold,
	})

	return &Peggy{
		agent:    agent,
		store:    store,
		settings: settings,
		soul:     systemPrompt,
		stderr:   stderr,
	}, nil
}

// Settings returns the effective settings the Peggy was constructed
// with (after defaults).
func (p *Peggy) Settings() Settings { return p.settings }

// Agent returns the underlying glue agent. Exposed for cross-session
// queries (Agent.SearchSessions) and for advanced integrations.
func (p *Peggy) Agent() *glue.Agent { return p.agent }

// Close releases resources held by the Peggy. Safe to call multiple
// times; safe on a nil receiver. When the store implements io.Closer
// (e.g. stores/sqlite) it is closed.
func (p *Peggy) Close() error {
	if p == nil {
		return nil
	}
	if closer, ok := p.store.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// Prompt runs one turn against the given session id and streams the
// assistant text to stdout. Returns the final concatenated text and
// any error.
//
// An empty sessionID resolves to "default".
func (p *Peggy) Prompt(ctx context.Context, sessionID, text string, stdout io.Writer) (string, error) {
	if p == nil || p.agent == nil {
		return "", errors.New("peggy: not initialised")
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("peggy: empty prompt")
	}
	session, err := p.agent.Session(ctx, sessionID)
	if err != nil {
		return "", err
	}
	opts := []glue.PromptOption{}
	if stdout != nil {
		opts = append(opts, glue.WithStreamWriter(stdout))
	}
	res, err := session.Prompt(ctx, text, opts...)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// buildProvider constructs the configured model backend. The codex
// provider doesn't expose an env-key probe via providers.KeyAvailable
// (subscription auth lives in auth.json), so we always use the
// explicit codex.New path; the other providers are constructed via
// the driver registry so any future provider lands "for free."
func buildProvider(s Settings) (glue.Provider, error) {
	name := strings.TrimSpace(s.Provider)
	if name == "" {
		name = DefaultProvider
	}
	switch strings.ToLower(name) {
	case "codex":
		return codex.New(codex.Options{Model: s.Model}), nil
	case "gemini":
		return gemini.New(gemini.Options{}), nil
	case "openrouter":
		return openrouter.New(openrouter.Options{}), nil
	case "nvidia":
		return nvidia.New(nvidia.Options{}), nil
	}
	// Last-ditch: try the driver registry. New providers land here
	// without code changes to peggy.
	prov, _, _, err := providers.New(name)
	if err != nil {
		return nil, fmt.Errorf("peggy: provider %q: %w", name, err)
	}
	return prov, nil
}

func buildStore(s Settings) (glue.Store, error) {
	switch strings.ToLower(s.Store.Type) {
	case "", "sqlite":
		if err := os.MkdirAll(filepath.Dir(s.Store.Path), 0o700); err != nil {
			return nil, fmt.Errorf("peggy: prepare sqlite path: %w", err)
		}
		return sqlitestore.Open(sqlitestore.Options{Path: s.Store.Path})
	case "file":
		return filestore.New(s.Store.Path), nil
	}
	return nil, fmt.Errorf("peggy: unknown store type %q", s.Store.Type)
}
