// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/jobs"
)

func browserRequired() bool {
	return os.Getenv("CI") == "true" || os.Getenv("GLUE_E2E_BROWSER_STRICT") == "1"
}

func findChromeBinary() (string, error) {
	if bin := strings.TrimSpace(os.Getenv("GLUE_CHROME_BIN")); bin != "" {
		return exec.LookPath(bin)
	}
	for _, candidate := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "headless-shell", "chrome"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no Chrome/Chromium executable found in PATH")
}

type noopHost struct{}

func (noopHost) Session(context.Context, string, ...glue.SessionOption) (*glue.Session, error) {
	return nil, fmt.Errorf("unused session host")
}

type scriptedE2EProvider struct {
	mu            sync.Mutex
	calls         int
	turns         [][]glue.ProviderEvent
	midJobOnce    sync.Once
	midJobReached chan struct{}
	releaseFinish chan struct{}
}

func textEvents(text string) []glue.ProviderEvent {
	return []glue.ProviderEvent{
		{Type: glue.ProviderEventStart},
		{Type: glue.ProviderEventTextDelta, Delta: text},
		{Type: glue.ProviderEventDone, Message: &glue.Message{Role: glue.MessageRoleAssistant, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: text}}}},
	}
}

func (p *scriptedE2EProvider) Stream(ctx context.Context, _ glue.ProviderRequest) (<-chan glue.ProviderEvent, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	var events []glue.ProviderEvent
	if call <= len(p.turns) {
		events = p.turns[call-1]
	}
	p.mu.Unlock()

	if call == 3 {
		p.midJobOnce.Do(func() { close(p.midJobReached) })
		select {
		case <-p.releaseFinish:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	ch := make(chan glue.ProviderEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func waitJS(t *testing.T, bctx context.Context, step, expr string) {
	t.Helper()
	_, err := chromedp.Run(bctx, chromedp.Poll[bool](expr, chromedp.WithPollingTimeout(10*time.Second), chromedp.WithPollingInterval(50*time.Millisecond)))
	if err != nil {
		t.Fatalf("%s: polling %s failed: %v", step, expr, err)
	}
}

func renderableTranscriptSeqs(events []eventlog.Envelope) (allSeqs []int64, itemSeqs []int64) {
	var currentTextKey string
	for _, ev := range events {
		allSeqs = append(allSeqs, ev.Seq)
		switch ev.Type {
		case "text_delta":
			if ev.Phase != "" && ev.Phase != "maker" {
				continue
			}
			iter := ""
			if ev.Iteration != nil {
				iter = fmt.Sprintf("%d", *ev.Iteration)
			}
			key := fmt.Sprintf("%s:%s", ev.Phase, iter)
			if currentTextKey != key {
				currentTextKey = key
				itemSeqs = append(itemSeqs, ev.Seq)
			}
		case "tool_start", "permission_request", "plan", "iteration_start", "verdict", "operator_message", "run_interrupted", "job_resumed", "job_done":
			currentTextKey = ""
			itemSeqs = append(itemSeqs, ev.Seq)
		default:
			currentTextKey = ""
		}
	}
	return allSeqs, itemSeqs
}

func TestE2EJobBrowserWorkflow(t *testing.T) {
	if os.Getenv("GLUE_E2E_BROWSER") != "1" {
		t.Skip("skipping browser E2E test; set GLUE_E2E_BROWSER=1 to run")
	}
	chromeBin, err := findChromeBinary()
	if err != nil {
		if browserRequired() {
			t.Fatalf("Chrome/Chromium binary is required when CI=true and GLUE_E2E_BROWSER=1 (%v); install google-chrome-stable / chromium or set GLUE_CHROME_BIN", err)
		}
		t.Skipf("Chrome/Chromium binary not found (%v); install Chrome or set CI=true to require it", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-qb", "main"},
		{"config", "user.email", "e2e@example.com"},
		{"config", "user.name", "e2e"},
		{"commit", "--allow-empty", "-qm", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	provider := &scriptedE2EProvider{
		midJobReached: make(chan struct{}),
		releaseFinish: make(chan struct{}),
		turns: [][]glue.ProviderEvent{
			textEvents(`{"items":[{"title":"Create hello.txt"}]}`),
			{
				{Type: glue.ProviderEventStart},
				{Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "call-write-1", Name: "write_file", Arguments: json.RawMessage(`{"path":"hello.txt","content":"hello browser e2e\n"}`)}},
				{Type: glue.ProviderEventDone},
			},
			textEvents("Created hello.txt."),
			textEvents(`{"done":true,"items":[{"title":"Create hello.txt","done":true,"evidence":"hello.txt exists"}]}`),
		},
	}

	stateDir := t.TempDir()
	mgr, err := jobs.New(jobs.Options{
		StateDir:        stateDir,
		ProviderFactory: func(string) (glue.Provider, error) { return provider, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	const daemonToken = "e2e-daemon-token"
	srv, err := daemon.New(daemon.Options{
		Host:              noopHost{},
		Token:             daemonToken,
		StateDir:          stateDir,
		Jobs:              mgr,
		KeepaliveInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	job, err := mgr.Create(ctx, jobs.Spec{
		Objective: "Create hello.txt via browser E2E",
		Repo:      jobs.RepoSpec{Path: repo},
		Policy:    "ask-all",
		Verify:    [][]string{{"git", "status", "--short"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(ctx, job.ID); err != nil {
		t.Fatal(err)
	}

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromeBin),
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.WindowSize(1280, 900),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, allocOpts...)
	t.Cleanup(cancelAlloc)

	bctx, cancelBrowser := chromedp.NewContext(allocCtx)
	t.Cleanup(cancelBrowser)

	var consoleMu sync.Mutex
	var consoleLines []string
	consoleDone := make(chan struct{})
	consoleSeq := chromedp.Console(bctx)
	go func() {
		defer close(consoleDone)
		for msg, err := range consoleSeq {
			if err != nil {
				return
			}
			consoleMu.Lock()
			consoleLines = append(consoleLines, msg.String())
			consoleMu.Unlock()
		}
	}()

	// Register artifact capture after cancelBrowser so LIFO cleanup runs while the tab is still alive.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		artDir := strings.TrimSpace(os.Getenv("GLUE_TEST_ARTIFACTS_DIR"))
		if artDir != "" {
			_ = os.MkdirAll(artDir, 0o755)
		} else {
			artDir, _ = os.MkdirTemp("", "glue-e2e-artifacts-*")
		}
		if artDir == "" {
			return
		}
		screenshotPath := filepath.Join(artDir, "failure-screenshot.png")
		consolePath := filepath.Join(artDir, "failure-console.log")

		shotCtx, shotCancel := context.WithTimeout(bctx, 5*time.Second)
		defer shotCancel()
		if pngBuf, err := chromedp.Run(shotCtx, chromedp.FullScreenshot(100)); err == nil && len(pngBuf) > 0 {
			_ = os.WriteFile(screenshotPath, pngBuf, 0o644)
		}
		consoleMu.Lock()
		logData := strings.Join(consoleLines, "\n") + "\n"
		consoleMu.Unlock()
		_ = os.WriteFile(consolePath, []byte(logData), 0o644)
		t.Logf("saved failure artifacts: screenshot=%s console=%s", screenshotPath, consolePath)
	})

	// 1. Login with the token.
	if err := chromedp.Do(bctx, chromedp.Navigate(ts.URL)); err != nil {
		t.Fatalf("step 1 navigate: %v", err)
	}
	waitJS(t, bctx, "step 1 wait login form", `Boolean(document.querySelector('[data-testid="login-token"]') && document.querySelector('[data-testid="login-submit"]'))`)
	if err := chromedp.Do(bctx,
		chromedp.SendKeys(`[data-testid="login-token"]`, daemonToken),
		chromedp.Click(`[data-testid="login-submit"]`),
	); err != nil {
		t.Fatalf("step 1 submit login: %v", err)
	}

	// 2. Jobs list shows the job; open it.
	jobRowSel := fmt.Sprintf(`[data-testid="job-row"][data-job-id=%q]`, job.ID)
	waitJS(t, bctx, "step 2 wait job row", fmt.Sprintf(`Boolean(document.querySelector(%q)?.textContent?.includes("Create hello.txt via browser E2E"))`, jobRowSel))
	if err := chromedp.Do(bctx, chromedp.Click(jobRowSel)); err != nil {
		t.Fatalf("step 2 open job: %v", err)
	}

	// 3. Transcript shows the plan and the first tool call (plus the pending permission card).
	waitJS(t, bctx, "step 3 wait plan, tool call, and permission card", `Boolean(
		document.querySelector('[data-testid="goal-divider"][data-event-type="plan"]') &&
		document.querySelector('[data-testid="tool-call"][data-tool-name="write_file"]') &&
		document.querySelector('[data-testid="permission-card"] [data-testid="permission-allow-once"]') &&
		document.querySelector('[data-testid="detail-status"]')?.textContent === "waiting_for_input"
	)`)

	// 4. Approve the permission from the card; status returns to running.
	if err := chromedp.Do(bctx, chromedp.Click(`[data-testid="permission-allow-once"]`)); err != nil {
		t.Fatalf("step 4 click allow once: %v", err)
	}
	waitJS(t, bctx, "step 4 status returns to running", `Boolean(
		document.querySelector('[data-testid="permission-state"]')?.textContent === "allowed" &&
		document.querySelector('[data-testid="detail-status"]')?.textContent === "running" &&
		document.querySelector('[data-testid="tool-status"]')?.textContent === "done"
	)`)

	// 5. Reload the page mid-job; rendered transcript items match distinct seqs in the event log (no duplicates, no gaps).
	select {
	case <-provider.midJobReached:
	case <-time.After(10 * time.Second):
		t.Fatal("step 5 timed out waiting for mid-job pause")
	}
	eventsBeforeReload, err := mgr.Events(ctx, job.ID, 0)
	if err != nil || len(eventsBeforeReload) == 0 {
		t.Fatalf("step 5 read events: %v (len=%d)", err, len(eventsBeforeReload))
	}
	wantAllSeqs, wantItemSeqs := renderableTranscriptSeqs(eventsBeforeReload)
	for i, seq := range wantAllSeqs {
		if seq != int64(i+1) {
			t.Fatalf("step 5 event log seq gap/duplicate at index %d: got %d, want %d", i, seq, i+1)
		}
	}
	lastSeq := wantAllSeqs[len(wantAllSeqs)-1]
	waitJS(t, bctx, "step 5 wait pre-reload lastSeq", fmt.Sprintf(`Number(document.querySelector('[data-testid="job-transcript"]')?.dataset.lastSeq || 0) === %d`, lastSeq))

	if err := chromedp.Do(bctx, chromedp.Reload()); err != nil {
		t.Fatalf("step 5 reload page: %v", err)
	}
	waitJS(t, bctx, "step 5 wait post-reload replay", fmt.Sprintf(`Number(document.querySelector('[data-testid="job-transcript"]')?.dataset.lastSeq || 0) === %d`, lastSeq))

	type seqCheckResult struct {
		SeenCount int     `json:"seenCount"`
		LastSeq   int     `json:"lastSeq"`
		ItemSeqs  []int64 `json:"itemSeqs"`
	}
	seqCheck, err := chromedp.Run(bctx, chromedp.Evaluate[seqCheckResult](`(() => {
		const tr = document.querySelector('[data-testid="job-transcript"]');
		const items = [...tr.querySelectorAll('.transcript-item')].map((el) => Number(el.getAttribute('data-seq') || 0));
		return {
			seenCount: Number(tr.dataset.seenCount || 0),
			lastSeq: Number(tr.dataset.lastSeq || 0),
			itemSeqs: items,
		};
	})()`))
	if err != nil {
		t.Fatalf("step 5 evaluate transcript seqs: %v", err)
	}
	if seqCheck.SeenCount != len(wantAllSeqs) || seqCheck.LastSeq != int(lastSeq) {
		t.Fatalf("step 5 seenCount=%d lastSeq=%d, want %d distinct event seqs (1..%d)", seqCheck.SeenCount, seqCheck.LastSeq, len(wantAllSeqs), lastSeq)
	}
	if !slices.Equal(seqCheck.ItemSeqs, wantItemSeqs) {
		t.Fatalf("step 5 rendered transcript item seqs = %v, want %v", seqCheck.ItemSeqs, wantItemSeqs)
	}

	// 6. Send a message; operator_message appears.
	waitJS(t, bctx, "step 6 wait steer form", `Boolean(document.querySelector('[data-testid="steer-input"]') && document.querySelector('[data-testid="steer-submit"]'))`)
	if err := chromedp.Do(bctx,
		chromedp.SendKeys(`[data-testid="steer-input"]`, "verify hello.txt carefully"),
		chromedp.Click(`[data-testid="steer-submit"]`),
	); err != nil {
		t.Fatalf("step 6 submit steer message: %v", err)
	}
	waitJS(t, bctx, "step 6 wait operator_message in transcript", `Boolean(
		document.querySelector('[data-testid="goal-divider"][data-event-type="operator_message"]')?.textContent?.includes("Operator: verify hello.txt carefully")
	)`)

	// 7. Job finishes; the result panel shows status and verify results.
	close(provider.releaseFinish)
	waitJS(t, bctx, "step 7 wait result panel", `Boolean(
		document.querySelector('[data-testid="job-result-panel"]:not([hidden])') &&
		document.querySelector('[data-testid="result-status"]')?.textContent === "achieved" &&
		document.querySelector('[data-testid="verify-exit-code"]')?.textContent === "exit 0" &&
		document.querySelector('[data-testid="result-diffstat"]')?.textContent?.includes("hello.txt")
	)`)

	sessionCookieHTTPOnly, err := chromedp.Run(bctx, chromedp.Evaluate[bool](`!document.cookie.includes("glue_session")`))
	if err != nil || !sessionCookieHTTPOnly {
		t.Fatalf("expected HttpOnly session cookie to be invisible to document.cookie (err=%v, ok=%v)", err, sessionCookieHTTPOnly)
	}
}
