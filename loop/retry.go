package loop

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// RetryPolicy bounds the loop-level provider retries that absorb
// transient failures (429s, 5xx, dropped streams) before they surface
// as turn errors. The zero value enables retries with the defaults;
// set Disabled to restore fail-fast behavior.
type RetryPolicy struct {
	// Disabled turns loop-level retries off entirely.
	Disabled bool

	// MaxRetries is the number of retries after the first attempt.
	// Zero means DefaultMaxRetries.
	MaxRetries int

	// BaseDelay is the first backoff delay; each retry doubles it.
	// Zero means DefaultRetryBaseDelay. A server-provided retry hint
	// (Retry-After, Gemini RetryInfo.retryDelay) overrides the
	// computed delay when larger.
	BaseDelay time.Duration

	// MaxDelay caps the backoff. Zero means DefaultRetryMaxDelay.
	MaxDelay time.Duration
}

// Retry defaults.
const (
	DefaultMaxRetries     = 3
	DefaultRetryBaseDelay = 2 * time.Second
	DefaultRetryMaxDelay  = 30 * time.Second
)

// EventRetry is emitted before each retry sleep, with the triggering
// error in Event.Error and attempt/delay details in Event.Metadata
// ("attempt", "max_attempts", "delay").
const EventRetry EventType = "retry"

// OverflowError marks a provider failure caused by the request
// exceeding the model's context window. It is never retried at the
// loop level: retrying an oversized request can only fail again.
// Callers that own a compactor (glue.Session) catch it, compact once,
// and retry once.
type OverflowError struct{ Err error }

func (e *OverflowError) Error() string { return "loop: context window exceeded: " + e.Err.Error() }
func (e *OverflowError) Unwrap() error { return e.Err }

type errorClass int

const (
	classFatal errorClass = iota
	classTransient
	classOverflow
)

// Fatal patterns are checked first: errors that retrying cannot fix
// and that must not be mistaken for transient ones by the broader
// transient regex (e.g. "quota" alone would match "rate limit"-ish
// wording on some providers).
var fatalRe = regexp.MustCompile(`(?i)(api.?key|unauthoriz|unauthenticated|forbidden|permission.?denied|invalid.?(request|argument)|billing|payment|quota exceeded.*plan|model not found|404)`)

// Overflow patterns: the request is too large for the context window.
// Drawn from the error strings of Gemini, OpenAI-compatible hosts,
// Anthropic, and Groq (pi's bank, trimmed to providers glue targets).
var overflowRe = regexp.MustCompile(`(?i)(prompt is too long|context.?window|context.?length|maximum.?context|input token count.*exceed|exceeds.*token|too many tokens|reduce the length of the messages)`)

// Transient patterns: worth retrying with backoff.
var transientRe = regexp.MustCompile(`(?i)(\b429\b|\b50[0234]\b|too many requests|rate.?limit|resource.?exhausted|overloaded|service.?unavailable|internal (server )?error|bad gateway|gateway timeout|connection (reset|refused|closed)|broken pipe|socket hang up|unexpected EOF|stream (closed|ended|reset)|closed before done|premature|fetch failed|temporar|try again|retry|timed? ?out)`)

func classifyProviderError(err error) errorClass {
	if err == nil {
		return classFatal
	}
	msg := err.Error()
	if overflowRe.MatchString(msg) {
		return classOverflow
	}
	if fatalRe.MatchString(msg) {
		return classFatal
	}
	if transientRe.MatchString(msg) {
		return classTransient
	}
	return classFatal
}

// retryHintRe extracts a server-suggested delay from error text:
// Gemini embeds RetryInfo as `"retryDelay":"22s"`, HTTP-ish errors say
// `retry after 7s` or `Retry-After: 7`.
var retryHintRe = regexp.MustCompile(`(?i)(?:retry.?delay["']?\s*[:=]\s*["']?|retry.?after["':\s]+)(\d+(?:\.\d+)?)\s*(ms|s)?`)

func parseRetryHint(msg string) time.Duration {
	m := retryHintRe.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v <= 0 {
		return 0
	}
	unit := time.Second
	if m[2] == "ms" {
		unit = time.Millisecond
	}
	return time.Duration(v * float64(unit))
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxRetries <= 0 {
		p.MaxRetries = DefaultMaxRetries
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = DefaultRetryBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = DefaultRetryMaxDelay
	}
	return p
}

func (p RetryPolicy) delay(attempt int, err error) time.Duration {
	d := p.BaseDelay << attempt
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	if err != nil {
		if hint := parseRetryHint(err.Error()); hint > d {
			d = hint
			if d > p.MaxDelay {
				d = p.MaxDelay
			}
		}
	}
	return d
}

// runAssistantTurnWithRetry wraps runAssistantTurn with classified
// retries. Transient provider failures (429s, 5xx, dropped/never-
// finished streams) back off and retry; overflow surfaces as
// *OverflowError for the caller's compactor; everything else fails
// fast. Nothing is appended to the transcript until an attempt
// succeeds, so retries can never duplicate history.
func runAssistantTurnWithRetry(ctx context.Context, req RunRequest, messages []Message, emit func(Event)) (Message, error) {
	if req.Retry.Disabled {
		return runAssistantTurn(ctx, req, messages, emit)
	}
	policy := req.Retry.withDefaults()

	var lastErr error
	for attempt := 0; ; attempt++ {
		assistant, err := runAssistantTurn(ctx, req, messages, emit)
		if err == nil {
			return assistant, nil
		}
		if ctx.Err() != nil {
			return Message{}, err
		}
		switch classifyProviderError(err) {
		case classOverflow:
			return Message{}, &OverflowError{Err: err}
		case classFatal:
			return Message{}, err
		}
		lastErr = err

		if attempt >= policy.MaxRetries {
			return Message{}, fmt.Errorf("loop: provider failed after %d attempts: %w", attempt+1, lastErr)
		}
		d := policy.delay(attempt, lastErr)
		emit(Event{
			Type:  EventRetry,
			Error: lastErr.Error(),
			Metadata: map[string]any{
				"attempt":      attempt + 1,
				"max_attempts": policy.MaxRetries + 1,
				"delay":        d.String(),
			},
		})
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Message{}, ctx.Err()
		case <-timer.C:
		}
	}
}
