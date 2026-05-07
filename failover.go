package glue

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/erain/glue/loop"
)

// WithFailover returns a Provider that tries each underlying provider in
// order until one accepts a Stream. Failure modes that trigger fallover:
//
//   - the underlying Stream call returns a non-nil error before any
//     event is emitted;
//   - the underlying stream's first event is a [ProviderEventError];
//   - the underlying stream closes immediately with no events.
//
// Once any non-error event is observed the wrapper commits to that
// provider for the rest of the turn — it does not recover mid-stream.
// This preserves the loop's "no half-streamed transcripts on retry"
// invariant.
//
// Callers that want env-key probing (skip provider X when its API key
// is unset) should pre-filter via [providers.KeyAvailable] from
// github.com/erain/glue/providers and pass only the candidates whose
// keys are present.
//
// Returns a typed error implementing [FailoverError] when all providers
// fail; use errors.As to inspect per-provider attempts.
func WithFailover(provs ...Provider) Provider {
	return failoverProvider{provs: provs}
}

type failoverProvider struct {
	provs []Provider
}

// FailoverError aggregates per-provider failures from a [WithFailover]
// stream attempt. It implements error and exposes Attempts so callers
// can inspect each provider's outcome.
type FailoverError struct {
	Attempts []FailoverAttempt
}

// FailoverAttempt is one provider's result inside a [FailoverError].
type FailoverAttempt struct {
	Index int
	Err   error
}

func (e *FailoverError) Error() string {
	parts := make([]string, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		parts = append(parts, fmt.Sprintf("provider[%d]: %v", a.Index, a.Err))
	}
	return "all providers failed: " + strings.Join(parts, "; ")
}

func (f failoverProvider) Stream(ctx context.Context, req loop.ProviderRequest) (<-chan loop.ProviderEvent, error) {
	if len(f.provs) == 0 {
		return nil, errors.New("WithFailover: no providers")
	}

	attempts := make([]FailoverAttempt, 0, len(f.provs))
	for i, p := range f.provs {
		ch, err := p.Stream(ctx, req)
		if err != nil {
			attempts = append(attempts, FailoverAttempt{Index: i, Err: err})
			continue
		}

		first, ok := <-ch
		if !ok {
			attempts = append(attempts, FailoverAttempt{Index: i, Err: errors.New("empty stream")})
			continue
		}
		if first.Type == loop.ProviderEventError {
			// drain the rest so the underlying provider can clean up
			for range ch {
			}
			attempts = append(attempts, FailoverAttempt{Index: i, Err: errors.New(first.Error)})
			continue
		}

		// commit: re-emit the first event then forward the rest.
		out := make(chan loop.ProviderEvent, 16)
		go func() {
			defer close(out)
			out <- first
			for ev := range ch {
				out <- ev
			}
		}()
		return out, nil
	}

	return nil, &FailoverError{Attempts: attempts}
}
