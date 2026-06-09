package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// flakyProvider fails its first failures Stream calls with err, then
// delegates to a scripted success turn.
type flakyProvider struct {
	mu       sync.Mutex
	failures int
	err      error
	calls    int
}

func (p *flakyProvider) Stream(_ context.Context, _ ProviderRequest) (<-chan ProviderEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls <= p.failures {
		return nil, p.err
	}
	ch := make(chan ProviderEvent, 2)
	ch <- ProviderEvent{Type: ProviderEventTextDelta, Delta: "recovered"}
	ch <- ProviderEvent{Type: ProviderEventDone}
	close(ch)
	return ch, nil
}

func fastRetry() RetryPolicy {
	return RetryPolicy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func TestRetryTransientThenSuccess(t *testing.T) {
	t.Parallel()
	provider := &flakyProvider{failures: 2, err: errors.New("429 too many requests")}
	var retries []Event
	res, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "hi"}}}},
		Retry:    fastRetry(),
		Emit: func(e Event) {
			if e.Type == EventRetry {
				retries = append(retries, e)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.calls != 3 {
		t.Fatalf("provider calls = %d, want 3", provider.calls)
	}
	if len(retries) != 2 {
		t.Fatalf("retry events = %d, want 2", len(retries))
	}
	if retries[0].Metadata["attempt"] != 1 || retries[0].Metadata["max_attempts"] != 4 {
		t.Fatalf("retry metadata = %#v", retries[0].Metadata)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Content[0].Text != "recovered" {
		t.Fatalf("final text = %q", last.Content[0].Text)
	}
}

func TestRetryFatalFailsFast(t *testing.T) {
	t.Parallel()
	provider := &flakyProvider{failures: 99, err: errors.New("401 invalid api key")}
	_, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "hi"}}}},
		Retry:    fastRetry(),
	})
	if err == nil {
		t.Fatal("want error")
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (no retries on fatal)", provider.calls)
	}
}

func TestRetryOverflowSurfacesTyped(t *testing.T) {
	t.Parallel()
	provider := &flakyProvider{failures: 99, err: errors.New("input token count 250000 exceeds the maximum")}
	_, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "hi"}}}},
		Retry:    fastRetry(),
	})
	var overflow *OverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("err = %v, want *OverflowError", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (overflow is not retried)", provider.calls)
	}
}

func TestRetryExhausted(t *testing.T) {
	t.Parallel()
	provider := &flakyProvider{failures: 99, err: errors.New("503 service unavailable")}
	_, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "hi"}}}},
		Retry:    RetryPolicy{MaxRetries: 2, BaseDelay: time.Millisecond},
	})
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("err = %v, want exhaustion after 3 attempts", err)
	}
	if provider.calls != 3 {
		t.Fatalf("provider calls = %d, want 3", provider.calls)
	}
}

func TestRetryDisabledFailsFast(t *testing.T) {
	t.Parallel()
	provider := &flakyProvider{failures: 99, err: errors.New("429 too many requests")}
	_, err := Run(context.Background(), RunRequest{
		Provider: provider,
		Messages: []Message{{Role: MessageRoleUser, Content: []ContentPart{{Type: ContentTypeText, Text: "hi"}}}},
		Retry:    RetryPolicy{Disabled: true},
	})
	if err == nil {
		t.Fatal("want error")
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
}

func TestClassifyProviderError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want errorClass
	}{
		{"429 too many requests", classTransient},
		{"rate limit exceeded, retryDelay: 22s", classTransient},
		{"503 Service Unavailable", classTransient},
		{"connection reset by peer", classTransient},
		{"loop: provider stream closed before done event", classTransient},
		{"prompt is too long: 250000 tokens", classOverflow},
		{"input token count exceeds the maximum allowed", classOverflow},
		{"the request exceeds the context window of this model", classOverflow},
		{"401 unauthorized: invalid api key", classFatal},
		{"model not found", classFatal},
		{"some inexplicable failure", classFatal},
	}
	for _, c := range cases {
		if got := classifyProviderError(errors.New(c.msg)); got != c.want {
			t.Errorf("classify(%q) = %d, want %d", c.msg, got, c.want)
		}
	}
}

func TestParseRetryHint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want time.Duration
	}{
		{`status 429 ... "retryDelay":"22s" ...`, 22 * time.Second},
		{"Retry-After: 7", 7 * time.Second},
		{"please retry after 500 ms", 500 * time.Millisecond},
		{"no hint here", 0},
	}
	for _, c := range cases {
		if got := parseRetryHint(c.msg); got != c.want {
			t.Errorf("parseRetryHint(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestRetryHonorsServerHint(t *testing.T) {
	t.Parallel()
	p := RetryPolicy{BaseDelay: time.Millisecond, MaxDelay: time.Hour}.withDefaults()
	d := p.delay(0, fmt.Errorf(`429 "retryDelay":"3s"`))
	if d != 3*time.Second {
		t.Fatalf("delay = %v, want 3s (server hint)", d)
	}
}
