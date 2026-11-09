// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"sync"

	"github.com/erain/glue"
)

// lazyProvider defers credentials and construction until the first ordinary run.
type lazyProvider struct {
	mu       sync.Mutex
	name     string
	factory  providerFactory
	provider glue.Provider
}

func (p *lazyProvider) Stream(ctx context.Context, request glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	if p.provider == nil {
		provider, err := p.factory(p.name)
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if provider == nil {
			p.mu.Unlock()
			return nil, errors.New("provider factory returned nil")
		}
		p.provider = provider
	}
	provider := p.provider
	p.mu.Unlock()
	return provider.Stream(ctx, request)
}
