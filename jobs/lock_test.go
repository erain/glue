// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erain/glue"
)

func TestSecondManagerFailsOnLock(t *testing.T) {
	dir := t.TempDir()
	first, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := New(Options{StateDir: dir})
	if err == nil {
		second.Close()
		t.Fatal("second manager acquired owned state")
	}
	if !strings.Contains(err.Error(), "LOCK") || !strings.Contains(err.Error(), "another manager") {
		t.Fatalf("unclear lock error: %v", err)
	}
	independent, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	independent.Close()
	before, err := os.Stat(filepath.Join(dir, "LOCK"))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err = New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	after, err := os.Stat(filepath.Join(dir, "LOCK"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("lock inode replaced")
	}
}

func TestManagerLockReleasedAfterStartupFailure(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "events.sqlite")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if m, err := New(Options{StateDir: dir}); err == nil {
		m.Close()
		t.Fatal("expected storage error")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
}

func TestManagerLockReleasedAfterHardStop(t *testing.T) {
	if dir := os.Getenv("GLUE_TEST_MANAGER_LOCK_DIR"); dir != "" {
		m, err := New(Options{StateDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		fmt.Println("LOCK ACQUIRED")
		for {
			time.Sleep(time.Hour)
		}
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagerLockReleasedAfterHardStop$")
	cmd.Env = append(os.Environ(), "GLUE_TEST_MANAGER_LOCK_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	reader := bufio.NewScanner(stdout)
	if !reader.Scan() || reader.Text() != "LOCK ACQUIRED" {
		t.Fatalf("child did not acquire lock: %v", reader.Err())
	}
	if m, err := New(Options{StateDir: dir}); err == nil {
		m.Close()
		t.Fatal("parent acquired child lock")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected killed child")
	}
	m, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
}

func TestManagerLockHeldWhileWorkersDrain(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	m, err := New(Options{StateDir: t.TempDir(), ProviderFactory: func(string) (glue.Provider, error) { close(entered); <-release; return nil, fmt.Errorf("fixture stop") }})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); m.Close() })
	job, err := m.Create(context.Background(), Spec{Objective: "wait", Repo: RepoSpec{Path: repoFixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not enter factory")
	}
	m.mu.Lock()
	canceled := m.active[job.ID].ctx.Done()
	m.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- m.Close() }()
	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not cancel worker")
	}
	if other, err := New(Options{StateDir: m.options.StateDir}); err == nil {
		other.Close()
		t.Fatal("released ownership before worker drained")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	other, err := New(Options{StateDir: m.options.StateDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
}
