// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

//go:build ignore

package main

import (
	"bufio"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/chromedp"
)

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "anywhere_driver: "+format+"\n", args...)
	os.Exit(1)
}

func check(step string, err error) {
	if err != nil {
		die("%s: %v", step, err)
	}
}

func writeEnv(path, content string) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil || os.Rename(tmp, path) != nil {
		die("write %s: %v", path, err)
	}
}

func findChrome() string {
	for _, c := range []string{strings.TrimSpace(os.Getenv("GLUE_CHROME_BIN")), "google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "headless-shell", "chrome"} {
		if p, err := exec.LookPath(c); c != "" && err == nil {
			return p
		}
	}
	die("no Chrome/Chromium binary found in PATH")
	return ""
}

func runEdge(envOut, upstreamPort string) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	check("rsa key", err)
	b64 := func(v any) string { raw, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(raw) }
	mint := func(email string) string {
		u := b64(map[string]string{"alg": "RS256", "kid": "k1"}) + "." + b64(map[string]any{"iss": "https://anywhere.cloudflareaccess.com", "aud": "aud-anywhere", "email": email, "exp": time.Now().Add(time.Hour).Unix()})
		sum := sha256.Sum256([]byte(u))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		return u + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	jwksLn, err := net.Listen("tcp4", "0.0.0.0:0")
	check("listen", err)
	go http.Serve(jwksLn, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "k1", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	target, _ := url.Parse("http://127.0.0.1:" + upstreamPort)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	edgeSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Cf-Access-Jwt-Assertion", mint("dev@anywhere.example"))
		r.Header.Set("X-Forwarded-Proto", "https")
		rp.ServeHTTP(w, r)
	}))
	defer edgeSrv.Close()
	_, jwksPort, _ := net.SplitHostPort(jwksLn.Addr().String())
	writeEnv(envOut, fmt.Sprintf("JWKS_PORT=%s\nEDGE_URL=%s\n", jwksPort, edgeSrv.URL))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}

func runBrowser(edgeURL, repoURL, envOut string, budgetSec int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(budgetSec)*time.Second)
	defer cancel()
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(findChrome()), chromedp.NoSandbox, chromedp.DisableGPU, chromedp.Flag("ignore-certificate-errors", true), chromedp.WindowSize(390, 844))...)
	defer cancelAlloc()
	bctx, cancelTab := chromedp.NewContext(allocCtx)
	defer cancelTab()
	poll := func(step, expr string, d time.Duration) {
		if _, err := chromedp.Run(bctx, chromedp.Poll[bool](expr, chromedp.WithPollingTimeout(d), chromedp.WithPollingInterval(150*time.Millisecond))); err != nil {
			diag, _ := chromedp.Run(bctx, chromedp.Evaluate[string](`JSON.stringify({href:location.href,status:document.querySelector('[data-testid="detail-status"]')?.textContent,ws:document.querySelector('[data-testid="workspace-status"]')?.textContent,errs:[...document.querySelectorAll('.error-text')].filter(e=>!e.hidden).map(e=>e.textContent)})`))
			die("%s failed: %v (diag=%s)", step, err, diag)
		}
		fmt.Printf("  [browser] %s\n", step)
	}
	evalStr := func(expr string) string {
		s, err := chromedp.Run(bctx, chromedp.Evaluate[string](expr))
		check("evaluate", err)
		return s
	}
	check("open #/", chromedp.Do(bctx, chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate(edgeURL+"/#/")))
	poll("zero-prompt Cloudflare Access login on #/", `Boolean(document.querySelector('[data-testid="jobs-view"]') && !document.querySelector('[data-testid="login-token"]') && !document.querySelector('[data-testid="login-view"]') && document.getElementById('session-status')?.textContent?.includes('dev@anywhere.example'))`, 20*time.Second)
	check("open #/new", chromedp.Do(bctx, chromedp.Navigate(edgeURL+"/#/new")))
	poll("mobile #/new interactive launcher (390x844)", `Boolean(document.querySelector('[data-testid="mode-interactive"]') && document.documentElement.scrollWidth <= 390)`, 15*time.Second)
	check("submit interactive workspace", chromedp.Do(bctx, chromedp.Click(`[data-testid="mode-interactive"]`), chromedp.SetValue(`[data-testid="new-job-repo"]`, repoURL), chromedp.SendKeys(`[data-testid="new-job-objective"]`, "Initialize go.mod (module example.com/sandbox, go 1.24) and create mul.go in package sandbox with Mul(a, b int) int returning a * b."), chromedp.Click(`[data-testid="new-job-submit"]`)))
	poll("interactive workspace started", `window.location.hash.startsWith('#/jobs/job-')`, 30*time.Second)
	jobID := evalStr(`window.location.hash.replace('#/jobs/', '')`)
	writeEnv(envOut, fmt.Sprintf("BROWSER_JOB_ID=%s\n", jobID))
	parked := `['Ready for your message', 'Suspended (wakes on message)'].includes(document.querySelector('[data-testid="workspace-status"]')?.textContent)`
	poll("turn 1 completed and parked", `document.querySelectorAll('[data-testid="user-turn"]').length === 1 && (document.querySelectorAll('[data-testid="assistant-text"]').length >= 1 || document.querySelectorAll('[data-testid="tool-call"]').length >= 1) && `+parked, 120*time.Second)
	check("send turn 2 follow-up", chromedp.Do(bctx, chromedp.SendKeys(`[data-testid="workspace-composer"] [data-testid="steer-input"]`, "Add mul_test.go in package sandbox testing Mul(3, 4) == 12 and Mul(0, 7) == 0.\n")))
	poll("turn 2 completed and parked", `document.querySelectorAll('[data-testid="user-turn"]').length === 2 && `+parked, 120*time.Second)
	check("open promote-goal-form", chromedp.Do(bctx, chromedp.Click(`[data-testid="promote-goal-btn"]`)))
	poll("promote-goal-form open", `!document.querySelector('[data-testid="promote-goal-form"]')?.hidden`, 10*time.Second)
	check("submit autonomous goal escalation", chromedp.Do(bctx, chromedp.SetValue(`[data-testid="promote-objective"]`, "Ensure Mul in mul.go and tests in mul_test.go pass go test ./..."), chromedp.SendKeys(`[data-testid="promote-verify"]`, "go test ./..."), chromedp.Click(`[data-testid="promote-submit"]`)))
	poll("goal achieved (exit 0) & unified diff rendered", `document.querySelector('[data-testid="result-status"]')?.textContent === 'achieved' && document.querySelector('[data-testid="verify-exit-code"]')?.textContent === 'exit 0' && document.querySelectorAll('[data-testid="job-diff"] [data-testid="diff-file"]').length >= 2 && Boolean(document.querySelector('[data-testid="result-open-pr-btn"]'))`, 180*time.Second)
	check("click Open Draft PR", chromedp.Do(bctx, chromedp.Click(`[data-testid="result-open-pr-btn"]`)))
	poll("draft PR created", `Boolean(document.querySelector('[data-testid="result-pr-link"]')?.getAttribute('href')?.startsWith('https://github.com/'))`, 30*time.Second)
	prURL := evalStr(`document.querySelector('[data-testid="result-pr-link"]').getAttribute('href')`)
	writeEnv(envOut, fmt.Sprintf("BROWSER_JOB_ID=%s\nPR_URL=%s\n", jobID, prURL))
	check("reload job detail", chromedp.Do(bctx, chromedp.Reload()))
	poll("draft PR URL persisted across reload", fmt.Sprintf(`document.querySelector('[data-testid="result-pr-link"]')?.getAttribute('href') === %q`, prURL), 20*time.Second)
}

func runNtfy(topicURL, jobID, wantClick string, budgetSec int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(budgetSec)*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, topicURL+"/json?since=2m", nil)
	resp, err := http.DefaultClient.Do(req)
	check("ntfy subscribe", err)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	tapped, confirmed := false, false
	for sc.Scan() {
		var ev struct {
			Event, Title, Message, Click string
			Actions                      []struct{ Action, Label, URL, Method, Body string }
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "message" || !strings.Contains(ev.Message, jobID) {
			continue
		}
		if ev.Click != wantClick {
			die("ntfy Click=%q, want %q", ev.Click, wantClick)
		}
		for _, a := range ev.Actions {
			if !tapped && a.Action == "http" && a.Label == "Beta" && a.Method == "POST" && strings.HasPrefix(a.URL, "https://ntfy.sh/glue-reply-") {
				pr, err := http.Post(a.URL, "application/json", strings.NewReader(a.Body))
				if err != nil || pr.Body.Close() != nil || pr.StatusCode >= 300 {
					die("ntfy action POST failed: status=%v err=%v", pr, err)
				}
				tapped = true
				fmt.Printf("  [ntfy] verified Click=%s and tapped HTTP action 'Beta'\n", wantClick)
			}
		}
		confirmed = confirmed || strings.Contains(ev.Message, "✓ Glue job "+jobID+": answered")
		if ev.Title == "Glue job achieved" && tapped && confirmed {
			fmt.Printf("  [ntfy] confirmed lock-screen answer and %s achieved\n", jobID)
			return
		}
	}
	die("ntfy stream ended early (tapped=%v confirmed=%v err=%v)", tapped, confirmed, sc.Err())
}

func main() {
	sec := 0
	if len(os.Args) > 5 {
		sec, _ = strconv.Atoi(os.Args[5])
	}
	switch os.Args[1] {
	case "edge":
		runEdge(os.Args[2], os.Args[3])
	case "browser":
		runBrowser(os.Args[2], os.Args[3], os.Args[4], sec)
	case "ntfy":
		runNtfy(os.Args[2], os.Args[3], os.Args[4], sec)
	default:
		die("unknown command %q", os.Args[1])
	}
}
