// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	pauseCall     int
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

	pauseAt := p.pauseCall
	if pauseAt == 0 {
		pauseAt = 4
	}
	if call == pauseAt { // after ask_user and write_file
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
		case "tool_start", "permission_request", "plan", "iteration_start", "verdict", "operator_message", "turn_interrupted", "run_interrupted", "job_resumed", "job_done":
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
				{Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "call-ask-1", Name: "ask_user", Arguments: json.RawMessage(`{"question":"Which greeting should hello.txt contain?","options":["hello browser e2e","hi"]}`)}},
				{Type: glue.ProviderEventDone},
			},
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

	// 3a. The agent's question appears as a card; answer it with an option.
	waitJS(t, bctx, "step 3a wait question card", `Boolean(
		document.querySelector('[data-testid="question-card"] [data-testid="question-text"]')?.textContent?.includes("Which greeting") &&
		document.querySelectorAll('[data-testid="question-option"]').length === 2
	)`)
	if err := chromedp.Do(bctx, chromedp.Click(`[data-testid="question-option"]`)); err != nil {
		t.Fatalf("step 3a answer question: %v", err)
	}
	waitJS(t, bctx, "step 3a question card closes", `!document.querySelector('[data-testid="question-card"]')`)

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
		document.querySelector('[data-tool-name="write_file"] [data-testid="tool-name"]')?.textContent === "write_file hello.txt" &&
		document.querySelector('[data-testid="tool-status"]')?.textContent === "✓"
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

	t.Run("cloudflare-access", func(t *testing.T) {
		var edgeAuth atomic.Bool
		edgeAuth.Store(true)
		cfTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ck, _ := r.Cookie("glue_session")
			if strings.HasPrefix(r.URL.Path, "/v1/") && (!edgeAuth.Load() || (r.URL.Path != "/v1/session" && (ck == nil || ck.Value != "cf-sess-1"))) {
				http.Error(w, `{"auth_mode":"cloudflare-access","error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
			} else if r.URL.Path == "/v1/session" {
				http.SetCookie(w, &http.Cookie{Name: "glue_session", Value: "cf-sess-1", Path: "/", HttpOnly: true})
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"authenticated":true,"auth_mode":"cloudflare-access","login":"alice@example.com","csrf_token":"cf-csrf-123"}`))
			} else {
				r.Header.Set("Authorization", "Bearer "+daemonToken)
				srv.ServeHTTP(w, r)
			}
		}))
		t.Cleanup(cfTS.Close)
		cfCtx, cancelCF := chromedp.NewContext(allocCtx)
		t.Cleanup(cancelCF)
		if err := chromedp.Do(cfCtx, chromedp.Navigate(cfTS.URL)); err != nil {
			t.Fatal(err)
		}
		waitJS(t, cfCtx, "cf jobs", `Boolean(document.querySelector('[data-testid="jobs-view"]') && !document.querySelector('[data-testid="login-view"]') && document.querySelector('#session-status')?.textContent === "Signed in as alice@example.com" && !document.cookie.includes("glue_session"))`)
		if err := chromedp.Do(cfCtx, chromedp.Click(jobRowSel)); err != nil {
			t.Fatal(err)
		}
		waitJS(t, cfCtx, "cf detail", `Boolean(document.querySelector('[data-testid="job-detail-view"]') && document.querySelector('#session-status')?.textContent === "Signed in as alice@example.com")`)
		edgeAuth.Store(false)
		if err := chromedp.Do(cfCtx, chromedp.Reload()); err != nil {
			t.Fatal(err)
		}
		waitJS(t, cfCtx, "cf notice", `Boolean(document.querySelector('[data-testid="cf-access-notice"]') && !document.querySelector('[data-testid="login-token"]'))`)
	})
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func startE2EBrowser(t *testing.T, p glue.Provider, wrap func(http.Handler) http.Handler) (context.Context, context.Context, *jobs.Manager, *httptest.Server) {
	t.Helper()
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
	stateDir := t.TempDir()
	mgr, err := jobs.New(jobs.Options{StateDir: stateDir, ProviderFactory: func(string) (glue.Provider, error) { return p, nil }})
	mustNoErr(t, err)
	const tok = "e2e-daemon-token"
	srv, err := daemon.New(daemon.Options{Host: noopHost{}, Token: tok, StateDir: stateDir, Jobs: mgr, KeepaliveInterval: 50 * time.Millisecond})
	mustNoErr(t, err)
	var h http.Handler = srv
	if wrap != nil {
		h = wrap(srv)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(func() { ts.Close(); _ = srv.Close(); _ = mgr.Close() })
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chromeBin), chromedp.NoSandbox, chromedp.DisableGPU, chromedp.WindowSize(1280, 900))...)
	bctx, cancelBrowser := chromedp.NewContext(allocCtx)
	t.Cleanup(func() { cancelBrowser(); cancelAlloc() })
	mustNoErr(t, chromedp.Do(bctx, chromedp.Navigate(ts.URL)))
	waitJS(t, bctx, "login form", `Boolean(document.querySelector('[data-testid="login-token"]'))`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.SendKeys(`[data-testid="login-token"]`, tok), chromedp.Click(`[data-testid="login-submit"]`)))
	waitJS(t, bctx, "jobs view", `Boolean(document.querySelector('[data-testid="jobs-view"]'))`)
	return ctx, bctx, mgr, ts
}

func initE2ERepo(t *testing.T, withOrigin bool) string {
	t.Helper()
	repo := t.TempDir()
	mustNoErr(t, os.WriteFile(filepath.Join(repo, "app.txt"), []byte("old line\nkeep\n"), 0o644))
	script := "git -C " + repo + " init -qb main && git -C " + repo + " -c user.name=e2e -c user.email=e2e@example.com add app.txt && git -C " + repo + " -c user.name=e2e -c user.email=e2e@example.com commit -qm base"
	if withOrigin {
		bare := filepath.Join(t.TempDir(), "erain", "sandbox.git")
		mustNoErr(t, os.MkdirAll(bare, 0o755))
		script += " && git -C " + bare + " init --bare -q && git -C " + repo + " remote add origin " + bare + " && git -C " + repo + " push -q origin main"
	}
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	return repo
}

func TestE2EDiffViewerAndOpenPR(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]string{"GET /repos/erain/sandbox": `{"default_branch":"main"}`, "GET /repos/erain/sandbox/pulls": `[]`}[r.Method+" "+r.URL.Path]
		if body == "" {
			body = `{"html_url":"https://github.com/erain/sandbox/pull/42"}`
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(gh.Close)
	t.Setenv("GLUE_GITHUB_API_URL", gh.URL)
	t.Setenv("GLUE_GITHUB_TOKEN", "gh-tok")
	p := &scriptedE2EProvider{midJobReached: make(chan struct{}), releaseFinish: make(chan struct{}), turns: [][]glue.ProviderEvent{
		textEvents(`{"items":[{"title":"Update files"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "e1", Name: "edit_file", Arguments: json.RawMessage(`{"path":"app.txt","old_string":"old line","new_string":"new line ` + strings.Repeat("x", 220) + `"}`)}}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: json.RawMessage(`{"path":"xss.html","content":"<script>window.__xss=1</script>\n"}`)}}, {Type: glue.ProviderEventDone}},
		textEvents("Updated files."),
		textEvents(`{"done":true,"items":[{"title":"Update files","done":true}],"summary":"Updated app and xss"}`),
	}}
	close(p.releaseFinish)
	ctx, bctx, mgr, ts := startE2EBrowser(t, p, nil)
	job, err := mgr.Create(ctx, jobs.Spec{Objective: "Update files safely", Repo: jobs.RepoSpec{Path: initE2ERepo(t, true), BaseRef: "main"}, Policy: "yolo", Delivery: "branch"})
	mustNoErr(t, err)
	mustNoErr(t, mgr.Start(ctx, job.ID))
	mustNoErr(t, chromedp.Do(bctx, chromedp.Navigate(ts.URL+"/#/jobs/"+job.ID)))
	waitJS(t, bctx, "diff & open PR button", `document.querySelectorAll('[data-testid="diff-file"]').length === 2 && document.querySelector('[data-testid="diff-summary"]')?.textContent?.includes("2 files") && [...document.querySelectorAll('[data-testid="diff-line-add"]')].some((e) => e.textContent.includes("<script>window.__xss=1</script>")) && [...document.querySelectorAll('[data-testid="diff-line-del"]')].some((e) => e.textContent === "-old line") && document.querySelector('[data-testid="diff-line-hunk"]')?.textContent?.startsWith("@@") && window.__xss === undefined && Boolean(document.querySelector('[data-testid="result-open-pr-btn"]'))`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile)))
	waitJS(t, bctx, "mobile 390x844 layout", `(() => { const pr = document.querySelector('[data-testid="result-open-pr-btn"]')?.getBoundingClientRect(), ld = document.querySelector('[data-testid="load-diff-btn"]')?.getBoundingClientRect(), hk = document.querySelector('[data-testid="diff-hunks"]'); return Boolean(pr && ld && hk && document.documentElement.scrollWidth <= 390 && pr.left >= 0 && pr.right <= 390 && ld.left >= 0 && ld.right <= 390 && hk.scrollWidth > hk.clientWidth); })()`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Click(`[data-testid="result-open-pr-btn"]`)))
	waitJS(t, bctx, "pr link swapped in", `!document.querySelector('[data-testid="result-open-pr-btn"]') && document.querySelector('[data-testid="result-pr-link"]')?.getAttribute("href") === "https://github.com/erain/sandbox/pull/42"`)
}

func TestE2EInterruptSteeringAndToolSummary(t *testing.T) {
	var interrupts atomic.Int32
	p := &scriptedE2EProvider{pauseCall: 3, midJobReached: make(chan struct{}), releaseFinish: make(chan struct{}), turns: [][]glue.ProviderEvent{
		textEvents(`{"items":[{"title":"Edit and verify"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "e1", Name: "edit_file", Arguments: json.RawMessage(`{"path":"app.txt","old_string":"old line","new_string":"live edited"}`)}}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "s1", Name: "shell_exec", Arguments: json.RawMessage(`{"argv":["git","rev-parse","--verify","missing-ref"]}`)}}, {Type: glue.ProviderEventDone}},
		textEvents("blocked"),
		textEvents("Interrupted cleanly."),
		textEvents(`{"done":true,"items":[{"title":"Edit and verify","done":true}],"summary":"Done"}`),
	}}
	ctx, bctx, mgr, ts := startE2EBrowser(t, p, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") && r.URL.Query().Get("interrupt") == "true" {
				interrupts.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	job, err := mgr.Create(ctx, jobs.Spec{Objective: "Edit and verify", Repo: jobs.RepoSpec{Path: initE2ERepo(t, false)}, Policy: "yolo"})
	mustNoErr(t, err)
	mustNoErr(t, mgr.Start(ctx, job.ID))
	select {
	case <-p.midJobReached:
	case <-time.After(10 * time.Second):
		t.Fatal("job did not reach blocking turn")
	}
	mustNoErr(t, chromedp.Do(bctx, chromedp.Navigate(ts.URL+"/#/jobs/"+job.ID)))
	waitJS(t, bctx, "tool summaries & activity", `(() => { const edit = document.querySelector('[data-testid="tool-call"][data-tool-name="edit_file"]'), sh = document.querySelector('[data-testid="tool-call"][data-tool-name="shell_exec"]'), act = document.querySelector('[data-testid="job-activity"]'); return Boolean(edit && !edit.open && edit.querySelector('[data-testid="tool-name"]')?.textContent === "edit_file app.txt" && edit.querySelector('[data-testid="tool-status"]')?.textContent === "✓" && sh && sh.open && sh.querySelector('[data-testid="tool-name"]')?.textContent === "shell_exec git rev-parse --verify missing-ref" && sh.querySelector('[data-testid="tool-status"]')?.textContent === "✗" && act && !act.hidden && act.textContent.includes("maker") && act.textContent.includes("shell_exec")); })()`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Click(`[data-testid="load-diff-btn"]`)))
	waitJS(t, bctx, "live diff while running", `[...document.querySelectorAll('[data-testid="diff-line-add"]')].some((e) => e.textContent === "+live edited")`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.SendKeys(`[data-testid="steer-input"]`, "stop and finish"), chromedp.Click(`[data-testid="interrupt-btn"]`)))
	waitJS(t, bctx, "interrupted & finished", `Boolean(document.querySelector('[data-testid="goal-divider"][data-event-type="turn_interrupted"]') && document.querySelector('[data-testid="result-status"]')?.textContent === "achieved")`)
	if interrupts.Load() != 1 {
		t.Fatalf("interrupt POST count = %d, want 1", interrupts.Load())
	}
}

func TestE2ENewJobSubmitFromBrowser(t *testing.T) {
	p := &scriptedE2EProvider{midJobReached: make(chan struct{}), releaseFinish: make(chan struct{}), turns: [][]glue.ProviderEvent{
		textEvents(`{"items":[{"title":"Update app.txt"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "e1", Name: "edit_file", Arguments: json.RawMessage(`{"path":"app.txt","old_string":"old line","new_string":"mobile new job"}`)}}, {Type: glue.ProviderEventDone}},
		textEvents("Updated app.txt."),
		textEvents(`{"done":true,"items":[{"title":"Update app.txt","done":true}],"summary":"Updated app.txt from #/new"}`),
		textEvents("Interactive workspace ready."),
	}}
	close(p.releaseFinish)
	ctx, bctx, mgr, _ := startE2EBrowser(t, p, nil)
	repo := initE2ERepo(t, true)
	_, err := mgr.Create(ctx, jobs.Spec{Objective: "Seed recent repo", Repo: jobs.RepoSpec{Path: repo, BaseRef: "main"}})
	mustNoErr(t, err)
	mustNoErr(t, chromedp.Do(bctx, chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Reload()))
	waitJS(t, bctx, "new-job-btn on mobile", `Boolean(document.querySelector('[data-testid="new-job-btn"]'))`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Click(`[data-testid="new-job-btn"]`)))
	waitJS(t, bctx, "new-job-view & chip & models", `Boolean(document.querySelector('[data-testid="new-job-view"]') && document.documentElement.scrollWidth <= 390 && document.querySelector('[data-testid="recent-repo-chip"]') && [...document.querySelectorAll('[data-testid="new-job-model"] option')].some((o) => o.value === "gemini-3-flash-preview"))`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Click(`[data-testid="recent-repo-chip"]`), chromedp.Click(`[data-testid="new-job-options-summary"]`), chromedp.SendKeys(`[data-testid="new-job-verify"]`, "git status --short"), chromedp.SendKeys(`[data-testid="new-job-objective"]`, "Ship mobile goal from #/new"), chromedp.Click(`[data-testid="new-job-submit"]`)))
	waitJS(t, bctx, "goal achieved & diff rendered", `window.location.hash.startsWith("#/jobs/job-") && document.querySelector('[data-testid="result-status"]')?.textContent === "achieved" && document.querySelector('[data-testid="verify-exit-code"]')?.textContent === "exit 0" && [...document.querySelectorAll('[data-testid="diff-line-add"]')].some((e) => e.textContent === "+mobile new job")`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Click(`[data-testid="back-to-jobs"]`)))
	waitJS(t, bctx, "back on jobs list", `Boolean(document.querySelector('[data-testid="new-job-btn"]'))`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Click(`[data-testid="new-job-btn"]`)))
	waitJS(t, bctx, "new-job-view for interactive", `Boolean(document.querySelector('[data-testid="mode-interactive"]'))`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Click(`[data-testid="mode-interactive"]`), chromedp.Click(`[data-testid="recent-repo-chip"]`), chromedp.SendKeys(`[data-testid="new-job-objective"]`, "Explore the repo interactively"), chromedp.Click(`[data-testid="new-job-submit"]`)))
	waitJS(t, bctx, "interactive parked waiting_for_input", `document.querySelector('[data-testid="detail-status"]')?.textContent === "waiting_for_input" && [...document.querySelectorAll('[data-testid="assistant-text"]')].some((e) => e.textContent.includes("Interactive workspace ready."))`)
	if list, err := mgr.List(ctx); err != nil || len(list) != 3 || !slices.ContainsFunc(list, func(j jobs.Job) bool { return j.Spec.Mode == jobs.ModeInteractive && j.Status == jobs.WaitingForInput }) {
		t.Fatalf("expected parked interactive job in %+v (err=%v)", list, err)
	}
}

func TestE2ENewJobFromGitHubIssue(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "owner", "repo.git")
	mustNoErr(t, os.MkdirAll(bare, 0o755))
	seed := initE2ERepo(t, false)
	if out, err := exec.Command("sh", "-c", "git -C "+bare+" init --bare -q && git -C "+seed+" remote add origin "+bare+" && git -C "+seed+" push -q origin main").CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v (%s)", err, out)
	}
	gitcfg := filepath.Join(t.TempDir(), "gitconfig")
	mustNoErr(t, os.WriteFile(gitcfg, []byte("[url \""+bare+"\"]\n\tinsteadOf = https://github.com/owner/repo\n"), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", gitcfg)
	var prMu sync.Mutex
	var prReq map[string]any
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/owner/repo":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case "GET /repos/owner/repo/issues/7":
			_, _ = w.Write([]byte(`{"number":7,"title":"PR","state":"open","pull_request":{}}`))
		case "GET /repos/owner/repo/issues/8":
			_, _ = w.Write([]byte(`{"number":8,"title":"Closed bug","state":"closed"}`))
		case "GET /repos/owner/repo/issues/42":
			_, _ = w.Write([]byte(`{"number":42,"title":"Add badge.txt from issue 42","body":"Create badge.txt","state":"open"}`))
		case "GET /repos/owner/repo/issues/42/comments", "GET /repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[]`))
		case "POST /repos/owner/repo/pulls":
			prMu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&prReq)
			prMu.Unlock()
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/owner/repo/pull/99"}`))
		}
	}))
	t.Cleanup(gh.Close)
	t.Setenv("GLUE_GITHUB_API_URL", gh.URL)
	t.Setenv("GLUE_GITHUB_TOKEN", "gh-tok")
	p := &scriptedE2EProvider{midJobReached: make(chan struct{}), releaseFinish: make(chan struct{}), turns: [][]glue.ProviderEvent{
		textEvents(`{"items":[{"title":"Create badge.txt"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "w1", Name: "write_file", Arguments: json.RawMessage(`{"path":"badge.txt","content":"ok\n"}`)}}, {Type: glue.ProviderEventDone}},
		textEvents("Created badge.txt."),
		textEvents(`{"done":true,"items":[{"title":"Create badge.txt","done":true}],"summary":"Created badge.txt"}`),
	}}
	close(p.releaseFinish)
	ctx, bctx, mgr, ts := startE2EBrowser(t, p, nil)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Navigate(ts.URL+"/#/new")))
	waitJS(t, bctx, "issue mode button", `Boolean(document.querySelector('[data-testid="mode-issue"]'))`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Click(`[data-testid="mode-issue"]`), chromedp.SetValue(`[data-testid="new-job-issue"]`, "owner/repo#7"), chromedp.Click(`[data-testid="new-job-submit"]`)))
	waitJS(t, bctx, "PR rejected inline", `!document.querySelector('[data-testid="new-job-error"]')?.hidden && document.querySelector('[data-testid="new-job-error"]')?.textContent?.includes("pull request") && document.querySelector('[data-testid="new-job-issue"]')?.value === "owner/repo#7"`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.SetValue(`[data-testid="new-job-issue"]`, "owner/repo#8"), chromedp.Click(`[data-testid="new-job-submit"]`)))
	waitJS(t, bctx, "closed issue rejected inline", `document.querySelector('[data-testid="new-job-error"]')?.textContent?.includes("closed")`)
	if list, err := mgr.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("jobs after rejected issues = %d (err=%v), want 0", len(list), err)
	}
	mustNoErr(t, chromedp.Do(bctx, chromedp.SetValue(`[data-testid="new-job-issue"]`, "owner/repo#42"), chromedp.Click(`[data-testid="new-job-submit"]`)))
	waitJS(t, bctx, "issue 42 achieved & draft PR linked", `document.querySelector('[data-testid="detail-objective"]')?.textContent === "Add badge.txt from issue 42" && document.querySelector('[data-testid="result-status"]')?.textContent === "achieved" && document.querySelector('[data-testid="result-pr-link"]')?.getAttribute("href") === "https://github.com/owner/repo/pull/99"`)
	prMu.Lock()
	defer prMu.Unlock()
	if body, _ := prReq["body"].(string); prReq["draft"] != true || !strings.HasPrefix(body, "Closes #42\n") {
		t.Fatalf("unexpected draft PR request: %#v", prReq)
	}
}

func TestE2EInteractiveWorkspaceChatAndPromoteToGoal(t *testing.T) {
	var mockSuspended atomic.Bool
	var mgrPtr atomic.Pointer[jobs.Manager]
	var wakeOnce sync.Once
	wakeStarted, releaseWake := make(chan struct{}), make(chan struct{})
	p := &scriptedE2EProvider{midJobReached: make(chan struct{}), releaseFinish: make(chan struct{}), turns: [][]glue.ProviderEvent{
		textEvents("Checked `app.txt`:\n```html\n<script>window.__xss_chat=1</script>\n```\nReady for next step."),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "c1", Name: "write_file", Arguments: json.RawMessage(`{"path":"note.txt","content":"from chat\n"}`)}}, {Type: glue.ProviderEventDone}},
		textEvents("Wrote `note.txt` in workspace."),
		textEvents(`{"items":[{"title":"Update app.txt"}]}`),
		{{Type: glue.ProviderEventStart}, {Type: glue.ProviderEventToolCall, ToolCall: &glue.ToolCall{ID: "m1", Name: "edit_file", Arguments: json.RawMessage(`{"path":"app.txt","old_string":"old line","new_string":"promoted goal line"}`)}}, {Type: glue.ProviderEventDone}},
		textEvents("Updated app.txt autonomously."),
		textEvents(`{"done":true,"items":[{"title":"Update app.txt","done":true}],"summary":"Completed interactive-to-goal handoff"}`),
	}}
	close(p.releaseFinish)
	_, bctx, mgr, ts := startE2EBrowser(t, p, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if mockSuspended.Load() && len(parts) == 3 && parts[0] == "v1" && parts[1] == "jobs" && r.Method == http.MethodGet {
				rec := httptest.NewRecorder()
				next.ServeHTTP(rec, r)
				for k, v := range rec.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(rec.Code)
				_, _ = w.Write([]byte(strings.Replace(rec.Body.String(), `"status":"waiting_for_input"`, `"status":"suspended"`, 1)))
				return
			}
			if mockSuspended.Load() && len(parts) == 4 && parts[1] == "jobs" && parts[3] == "events" && r.URL.Query().Get("cached") == "1" {
				evs, _ := mgrPtr.Load().Events(r.Context(), parts[2], 0)
				w.Header().Set("Content-Type", "text/event-stream")
				for _, ev := range evs {
					raw, _ := json.Marshal(ev)
					_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, raw)
				}
				return
			}
			if mockSuspended.Load() && len(parts) == 4 && parts[1] == "jobs" && parts[3] == "goal" && r.Method == http.MethodPost {
				wakeOnce.Do(func() { close(wakeStarted) })
				<-releaseWake
			}
			next.ServeHTTP(w, r)
		})
	})
	mgrPtr.Store(mgr)
	repo := initE2ERepo(t, true)
	mustNoErr(t, chromedp.Do(bctx, chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate(ts.URL+"/#/new")))
	waitJS(t, bctx, "mobile #/new interactive", `Boolean(document.querySelector('[data-testid="mode-interactive"]') && document.documentElement.scrollWidth <= 390)`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Click(`[data-testid="mode-interactive"]`), chromedp.SetValue(`[data-testid="new-job-repo"]`, repo), chromedp.SendKeys(`[data-testid="new-job-objective"]`, "Inspect app.txt"), chromedp.Click(`[data-testid="new-job-submit"]`)))
	waitJS(t, bctx, "turn 1 reply, code block XSS safe & awaiting_turn", `(() => { const cb = document.querySelector('[data-testid="code-block"] code'), ic = document.querySelector('[data-testid="inline-code"]'), ws = document.querySelector('[data-testid="workspace-status"]'), ta = document.querySelector('[data-testid="steer-input"]'), cp = document.querySelector('[data-testid="workspace-composer"]')?.getBoundingClientRect(); const shiftOk = ta?.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", shiftKey: true, cancelable: true })), imeOk = ta?.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", isComposing: true, cancelable: true })); return Boolean(window.location.hash.startsWith("#/jobs/job-") && document.documentElement.scrollWidth <= 390 && cp && cp.left >= 0 && cp.right <= 390 && shiftOk && imeOk && document.querySelectorAll('[data-testid="user-turn"]').length === 1 && ic?.textContent === "app.txt" && cb?.textContent === "<script>window.__xss_chat=1</script>" && window.__xss_chat === undefined && ws?.textContent === "Ready for your message"); })()`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.SendKeys(`[data-testid="workspace-composer"] [data-testid="steer-input"]`, "Create note.txt\n")))
	waitJS(t, bctx, "turn 2 via Enter key", `document.querySelectorAll('[data-testid="user-turn"]').length === 2 && document.querySelector('[data-testid="tool-call"][data-tool-name="write_file"] [data-testid="tool-name"]')?.textContent === "write_file note.txt" && [...document.querySelectorAll('[data-testid="assistant-text"]')].some((e) => e.textContent.includes("Wrote note.txt in workspace.")) && document.querySelector('[data-testid="workspace-status"]')?.textContent === "Ready for your message"`)
	mockSuspended.Store(true)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Reload()))
	waitJS(t, bctx, "suspended workspace status", `document.querySelector('[data-testid="workspace-status"]')?.textContent === "Suspended (wakes on message)" && document.querySelectorAll('[data-testid="user-turn"]').length === 2`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.Click(`[data-testid="promote-goal-btn"]`)))
	waitJS(t, bctx, "promote goal form open & prefilled", `!document.querySelector('[data-testid="promote-goal-form"]')?.hidden && document.querySelector('[data-testid="promote-objective"]')?.value === "Inspect app.txt" && document.documentElement.scrollWidth <= 390`)
	mustNoErr(t, chromedp.Do(bctx, chromedp.SetValue(`[data-testid="promote-objective"]`, "Update app.txt and verify"), chromedp.SendKeys(`[data-testid="promote-verify"]`, "git status --short"), chromedp.Click(`[data-testid="promote-submit"]`)))
	select {
	case <-wakeStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for POST /goal wake")
	}
	waitJS(t, bctx, "waking workspace indicator", `document.querySelector('[data-testid="workspace-status"]')?.textContent === "Waking workspace…"`)
	mockSuspended.Store(false)
	close(releaseWake)
	waitJS(t, bctx, "promoted goal achieved with phase badges, diff & open PR", `["planner", "maker", "checker"].every((ph) => document.querySelector('[data-testid="phase-badge"][data-phase="' + ph + '"]')) && document.querySelector('[data-testid="result-status"]')?.textContent === "achieved" && document.querySelector('[data-testid="verify-exit-code"]')?.textContent === "exit 0" && document.querySelectorAll('[data-testid="job-diff"] [data-testid="diff-file"]').length === 2 && Boolean(document.querySelector('[data-testid="result-open-pr-btn"]')) && document.documentElement.scrollWidth <= 390`)
}
