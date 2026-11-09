// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/erain/glue"
)

func TestLazyProviderRetriesAndReuses(t *testing.T) {
	calls := 0
	script := &scriptedProvider{turns: [][]glue.ProviderEvent{textTurn("one"), textTurn("two")}}
	p := &lazyProvider{name: "fixture", factory: func(name string) (glue.Provider, error) {
		if name != "fixture" {
			t.Fatal(name)
		}
		calls++
		if calls == 1 {
			return nil, errors.New("missing credentials")
		}
		return script, nil
	}}
	if _, err := p.Stream(context.Background(), glue.ProviderRequest{}); err == nil {
		t.Fatal("expected construction error")
	}
	for i := 0; i < 2; i++ {
		events, err := p.Stream(context.Background(), glue.ProviderRequest{Model: "selected"})
		if err != nil {
			t.Fatal(err)
		}
		for range events {
		}
	}
	if calls != 2 || len(script.requests) != 2 || script.requests[0].Model != "selected" {
		t.Fatalf("construction %d requests %v", calls, script.requests)
	}
	nilProvider := &lazyProvider{factory: func(string) (glue.Provider, error) { return nil, nil }}
	if _, err := nilProvider.Stream(context.Background(), glue.ProviderRequest{}); err == nil {
		t.Fatal("accepted nil provider")
	}
}
