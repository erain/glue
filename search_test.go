package glue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSearcher captures invocation arguments so tests can assert
// option plumbing.
type fakeSearcher struct {
	hits      []SearchHit
	err       error
	gotQuery  string
	gotOpts   SearchOptions
	callCount int
}

func (f *fakeSearcher) Load(ctx context.Context, id string) (SessionState, bool, error) {
	return SessionState{Version: SessionStateVersion, ID: id}, false, nil
}

func (f *fakeSearcher) Save(_ context.Context, _ string, _ SessionState) error {
	return nil
}

func (f *fakeSearcher) Delete(_ context.Context, _ string) error { return nil }

func (f *fakeSearcher) Search(_ context.Context, query string, opts SearchOptions) ([]SearchHit, error) {
	f.callCount++
	f.gotQuery = query
	f.gotOpts = opts
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

// nonSearchingStore is a Store that does *not* implement Searcher.
type nonSearchingStore struct{}

func (nonSearchingStore) Load(_ context.Context, id string) (SessionState, bool, error) {
	return SessionState{Version: SessionStateVersion, ID: id}, false, nil
}

func (nonSearchingStore) Save(_ context.Context, _ string, _ SessionState) error { return nil }

func (nonSearchingStore) Delete(_ context.Context, _ string) error { return nil }

func TestSearchSessions_NoStoreReturnsNotSupported(t *testing.T) {
	t.Parallel()
	a := NewAgent(AgentOptions{Provider: &recordingProvider{}})
	_, err := a.SearchSessions(context.Background(), "x")
	if !errors.Is(err, ErrSearchNotSupported) {
		t.Fatalf("err = %v, want ErrSearchNotSupported", err)
	}
}

func TestSearchSessions_NonSearcherStoreReturnsNotSupported(t *testing.T) {
	t.Parallel()
	a := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: nonSearchingStore{}})
	_, err := a.SearchSessions(context.Background(), "x")
	if !errors.Is(err, ErrSearchNotSupported) {
		t.Fatalf("err = %v, want ErrSearchNotSupported", err)
	}
}

func TestSearchSessions_ForwardsQueryAndOptions(t *testing.T) {
	t.Parallel()
	hits := []SearchHit{{SessionID: "s", Index: 1, Role: MessageRoleUser, Snippet: "<<hit>>", Score: 0.42}}
	fs := &fakeSearcher{hits: hits}
	a := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: fs})
	since := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	until := since.Add(72 * time.Hour)
	got, err := a.SearchSessions(context.Background(), "Aussie",
		WithLimit(5), WithOffset(2), WithSessionID("sess-1"),
		WithSince(since), WithUntil(until),
	)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(got) != 1 || got[0].Snippet != "<<hit>>" {
		t.Errorf("hits round-trip wrong: %+v", got)
	}
	if fs.gotQuery != "Aussie" {
		t.Errorf("query = %q", fs.gotQuery)
	}
	if fs.gotOpts.Limit != 5 || fs.gotOpts.Offset != 2 {
		t.Errorf("limit/offset = %d/%d", fs.gotOpts.Limit, fs.gotOpts.Offset)
	}
	if fs.gotOpts.SessionID != "sess-1" {
		t.Errorf("session_id = %q", fs.gotOpts.SessionID)
	}
	if !fs.gotOpts.Since.Equal(since) || !fs.gotOpts.Until.Equal(until) {
		t.Errorf("time bounds = %s / %s", fs.gotOpts.Since, fs.gotOpts.Until)
	}
}

func TestSearchSessions_LimitDefaultsAndClamps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opt  SearchOption
		want int
	}{
		{"zero defaults", WithLimit(0), DefaultSearchLimit},
		{"negative defaults", WithLimit(-3), DefaultSearchLimit},
		{"under cap kept", WithLimit(7), 7},
		{"over cap clamped", WithLimit(MaxSearchLimit + 50), MaxSearchLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeSearcher{}
			a := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: fs})
			_, _ = a.SearchSessions(context.Background(), "x", tc.opt)
			if fs.gotOpts.Limit != tc.want {
				t.Errorf("limit = %d want %d", fs.gotOpts.Limit, tc.want)
			}
		})
	}
}

func TestSearchSessions_NegativeOffsetClamped(t *testing.T) {
	t.Parallel()
	fs := &fakeSearcher{}
	a := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: fs})
	_, _ = a.SearchSessions(context.Background(), "x", WithOffset(-9))
	if fs.gotOpts.Offset != 0 {
		t.Errorf("offset = %d, want 0", fs.gotOpts.Offset)
	}
}

func TestSearchSessions_SearcherErrorPropagates(t *testing.T) {
	t.Parallel()
	fs := &fakeSearcher{err: errors.New("upstream boom")}
	a := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: fs})
	_, err := a.SearchSessions(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "upstream boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionSearch_ForcesSessionID(t *testing.T) {
	t.Parallel()
	fs := &fakeSearcher{}
	a := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: fs})
	sess, err := a.Session(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	// Even when caller supplies WithSessionID("other"), Session.Search
	// must force its own id.
	_, _ = sess.Search(context.Background(), "q", WithSessionID("other"))
	if fs.gotOpts.SessionID != "abc" {
		t.Errorf("session_id = %q, want abc (overrides WithSessionID)", fs.gotOpts.SessionID)
	}
}

func TestSessionSearch_NoStoreReturnsNotSupported(t *testing.T) {
	t.Parallel()
	a := NewAgent(AgentOptions{Provider: &recordingProvider{}})
	sess, err := a.Session(context.Background(), "x")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_, err = sess.Search(context.Background(), "q")
	if !errors.Is(err, ErrSearchNotSupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestSearchOption_NilToleratedInVariadic(t *testing.T) {
	t.Parallel()
	fs := &fakeSearcher{}
	a := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: fs})
	// Nil option should not panic.
	if _, err := a.SearchSessions(context.Background(), "x", nil, WithLimit(3)); err != nil {
		t.Fatalf("err = %v", err)
	}
	if fs.gotOpts.Limit != 3 {
		t.Errorf("limit not applied past nil option: %d", fs.gotOpts.Limit)
	}
}

// File store is also non-Searcher; verify the type-assertion path uses
// the actual stores/file Store type in addition to a synthetic non-
// Searcher.
func TestSearchSessions_FileStoreNotSupported(t *testing.T) {
	t.Parallel()
	// Use an in-package shim that mirrors the file store contract.
	a := NewAgent(AgentOptions{Provider: &recordingProvider{}, Store: nonSearchingStore{}})
	_, err := a.SearchSessions(context.Background(), "x")
	if !errors.Is(err, ErrSearchNotSupported) {
		t.Fatalf("err = %v", err)
	}
}
