// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

type messageProvider struct {
	inner            scriptProvider
	mu               sync.Mutex
	requests         []glue.ProviderRequest
	started, release chan struct{}
}

func (p *messageProvider) Stream(ctx context.Context, req glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	i := len(p.requests)
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if i == 1 {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.inner.Stream(ctx, req)
}
func TestGuidanceDeliveredToNextMakerPrompt(t *testing.T) {
	p := &messageProvider{started: make(chan struct{}), release: make(chan struct{}), inner: scriptProvider{turns: [][]glue.ProviderEvent{
		turn(`{"items":[{"title":"A"}]}`), turn("first maker"), turn(`{"items":[{"title":"A"}]}`), turn("second maker"), turn(`{"done":true,"items":[{"title":"A","done":true}]}`),
	}}}
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "finish A", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.started:
	case <-time.After(10 * time.Second):
		t.Fatal("maker did not start")
	}
	if err := m.Message(context.Background(), job.ID, "Include examples."); err != nil {
		t.Fatal(err)
	}
	events, err := m.Events(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Type != "operator_message" {
		t.Fatalf("not queued immediately: %v", events)
	}
	for _, e := range events {
		if e.Type == "operator_message_delivered" {
			t.Fatal("delivered during active maker")
		}
	}
	close(p.release)
	if got := waitJob(t, m, job.ID); got.Status != Achieved {
		t.Fatal(got)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) != 5 {
		t.Fatalf("requests %d", len(p.requests))
	}
	for i, req := range p.requests {
		var text string
		for _, msg := range req.Messages {
			for _, part := range msg.Content {
				text += part.Text
			}
		}
		if strings.Contains(text, "Include examples.") != (i >= 3) {
			t.Fatalf("request %d: %s", i, text)
		}
	}
	events, err = m.Events(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var queued, delivered int64
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatal("event sequence gap")
		}
		if e.Type == "operator_message" {
			queued = e.Seq
		}
		if e.Type == "operator_message_delivered" {
			if delivered != 0 {
				t.Fatal("duplicate delivery")
			}
			delivered = e.Seq
			if e.Iteration == nil || *e.Iteration != 2 {
				t.Fatal(e)
			}
		}
	}
	if queued == 0 || delivered <= queued {
		t.Fatalf("queued %d delivered %d", queued, delivered)
	}
}

func TestMessageQueueSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: dir}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Message(context.Background(), job.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Message(context.Background(), job.ID, "second"); err != nil {
		t.Fatal(err)
	}
	text, err := m.guidance(context.Background(), job.ID, 1)
	if err != nil || text != "first\n\nsecond" {
		t.Fatalf("guidance %q err %v", text, err)
	}
	before, err := m.Events(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	text, err = m.guidance(context.Background(), job.ID, 2)
	if err != nil || text != "first\n\nsecond" {
		t.Fatalf("guidance %q err %v", text, err)
	}
	after, err := m.Events(context.Background(), job.ID, 0)
	if err != nil || len(after) != len(before) {
		t.Fatalf("redelivery events %d -> %d err %v", len(before), len(after), err)
	}
}
func TestMessageValidationAndConflict(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: "unused"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Message(context.Background(), job.ID, " \n"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Message(ctx, job.ID, "text"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := m.Cancel(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Message(context.Background(), job.ID, "text"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestMessageDeliveryFailureKeepsQueue(t *testing.T) {
	m, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "A", Repo: RepoSpec{Path: "unused"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Message(context.Background(), job.ID, "keep guidance"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.DB.Exec(`CREATE TRIGGER reject_delivery BEFORE INSERT ON events WHEN json_extract(NEW.payload_json,'$.type')='operator_message_delivered' BEGIN SELECT RAISE(FAIL,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.guidance(context.Background(), job.ID, 1); err == nil {
		t.Fatal("ignored delivery write failure")
	}
	if _, err := m.store.DB.Exec("DROP TRIGGER reject_delivery"); err != nil {
		t.Fatal(err)
	}
	text, err := m.guidance(context.Background(), job.ID, 2)
	if err != nil || text != "keep guidance" {
		t.Fatalf("guidance %q err %v", text, err)
	}
	events, err := m.Events(context.Background(), job.ID, 0)
	if err != nil || len(events) != 3 {
		t.Fatalf("events %v err %v", events, err)
	}
}
