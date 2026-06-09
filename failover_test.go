package glue

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubProvider struct {
	streamErr error
	events    []ProviderEvent
}

func (s stubProvider) Stream(_ context.Context, _ ProviderRequest) (<-chan ProviderEvent, error) {
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	ch := make(chan ProviderEvent, len(s.events))
	for _, e := range s.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func okStream(text string) []ProviderEvent {
	return []ProviderEvent{
		{Type: ProviderEventStart},
		{Type: ProviderEventTextDelta, Delta: text},
		{Type: ProviderEventDone},
	}
}

func collect(t *testing.T, ch <-chan ProviderEvent) []ProviderEvent {
	t.Helper()
	var out []ProviderEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestFailover_FirstSucceeds(t *testing.T) {
	p := WithFailover(
		stubProvider{events: okStream("first")},
		stubProvider{events: okStream("second")},
	)
	ch, err := p.Stream(context.Background(), ProviderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	if len(got) != 3 || got[1].Delta != "first" {
		t.Fatalf("expected first provider's stream, got %+v", got)
	}
}

func TestFailover_StreamErrorFallsThrough(t *testing.T) {
	p := WithFailover(
		stubProvider{streamErr: errors.New("boom")},
		stubProvider{events: okStream("fallback")},
	)
	ch, err := p.Stream(context.Background(), ProviderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	if len(got) != 3 || got[1].Delta != "fallback" {
		t.Fatalf("expected fallback stream, got %+v", got)
	}
}

func TestFailover_FirstEventErrorFallsThrough(t *testing.T) {
	p := WithFailover(
		stubProvider{events: []ProviderEvent{{Type: ProviderEventError, Error: "model errored"}}},
		stubProvider{events: okStream("fallback")},
	)
	ch, err := p.Stream(context.Background(), ProviderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	if len(got) != 3 || got[1].Delta != "fallback" {
		t.Fatalf("expected fallback stream, got %+v", got)
	}
}

func TestFailover_EmptyStreamFallsThrough(t *testing.T) {
	p := WithFailover(
		stubProvider{events: nil},
		stubProvider{events: okStream("fallback")},
	)
	ch, err := p.Stream(context.Background(), ProviderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	if len(got) != 3 || got[1].Delta != "fallback" {
		t.Fatalf("expected fallback stream, got %+v", got)
	}
}

func TestFailover_AllFailReturnsAggregateError(t *testing.T) {
	p := WithFailover(
		stubProvider{streamErr: errors.New("boom1")},
		stubProvider{events: []ProviderEvent{{Type: ProviderEventError, Error: "boom2"}}},
	)
	_, err := p.Stream(context.Background(), ProviderRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	var fe *FailoverError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FailoverError, got %T: %v", err, err)
	}
	if len(fe.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(fe.Attempts))
	}
	if !strings.Contains(err.Error(), "boom1") || !strings.Contains(err.Error(), "boom2") {
		t.Fatalf("aggregate should include each failure: %v", err)
	}
}

func TestFailover_NoProviders(t *testing.T) {
	_, err := WithFailover().Stream(context.Background(), ProviderRequest{})
	if err == nil {
		t.Fatal("expected error for empty failover")
	}
}

func TestFailover_CommitsAfterFirstSuccessfulEvent(t *testing.T) {
	// Once the first non-error event is observed, the wrapper must NOT
	// fall through even if a later event is an error.
	p := WithFailover(
		stubProvider{events: []ProviderEvent{
			{Type: ProviderEventStart},
			{Type: ProviderEventError, Error: "mid-stream error"},
		}},
		stubProvider{events: okStream("fallback")},
	)
	ch, err := p.Stream(context.Background(), ProviderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	// Should be the first provider's events (commit), not the fallback.
	if len(got) != 2 || got[1].Type != ProviderEventError {
		t.Fatalf("expected committed-but-errored first provider, got %+v", got)
	}
}
