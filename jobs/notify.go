// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/redact"
)

// notifyCloseTimeout bounds how long notifications may run, and how long
// Manager.Close waits for those still in flight.
const notifyCloseTimeout = 20 * time.Second
const maxNtfyReplyBytes = 4 << 10

// waitNotifications stops new notifications and waits up to d for the rest.
func (m *Manager) waitNotifications(d time.Duration) {
	m.notifyMu.Lock()
	m.notifyClosed = true
	m.replyCancel()
	m.notifyMu.Unlock()
	done := make(chan struct{})
	go func() { m.notifyWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// notifyBackoff is the wait before each notification attempt.
var notifyBackoff = []time.Duration{0, 2 * time.Second, 5 * time.Second}
var ntfyReplyBackoff = 200 * time.Millisecond

// Notification is the JSON body posted to notify URLs. Text is a one-line
// summary that Slack-style incoming webhooks render as is; ntfy topic URLs
// (hosts starting with "ntfy.") get Text as a plain-text body instead.
type Notification struct {
	JobID     string `json:"job_id"`
	Event     string `json:"event"` // "waiting_for_input" or "done"
	Status    Status `json:"status"`
	Objective string `json:"objective"`
	Summary   string `json:"summary,omitempty"`
	PRURL     string `json:"pr_url,omitempty"`
	Question  string `json:"question,omitempty"`
	Text      string `json:"text"`

	clickURL, actions, ntfyText string
}

// ValidNotifyURL reports whether raw is an http(s) URL with a host.
func ValidNotifyURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("invalid notify URL %q: use an http(s) URL", raw)
	}
	return nil
}

// notifyURLs returns the pushed notify URLs, or GLUE_NOTIFY_URL for a host
// that sets it in its own environment.
func (m *Manager) notifyURLs() []string {
	m.credsMu.RLock()
	urls := m.creds.NotifyURLs
	m.credsMu.RUnlock()
	if len(urls) == 0 {
		if v := strings.TrimSpace(os.Getenv("GLUE_NOTIFY_URL")); v != "" {
			urls = []string{v}
		}
	}
	return urls
}

// maybeNotify sends notifications for events a developer should hear about.
// It never blocks the caller; failures are recorded as notify_failed events.
func (m *Manager) maybeNotify(id, typ string, payload any) {
	var event string
	switch typ {
	case "permission_request", "question":
		event = "waiting_for_input"
	case "job_done":
		event = "done"
	default:
		return
	}
	urls := m.notifyURLs()
	if len(urls) == 0 {
		return
	}
	ctx := context.Background()
	job, err := m.Get(ctx, id)
	if err != nil {
		return
	}
	n := Notification{JobID: id, Event: event, Status: job.Status, Objective: job.Spec.Objective, clickURL: m.jobClickURL(id)}
	itemID, opts := "", []string(nil)
	if event == "done" {
		if res, err := m.Result(ctx, id); err == nil {
			n.Status, n.Summary, n.PRURL = res.Status, res.Summary, res.PRURL
		}
		n.Text = strings.TrimSpace(fmt.Sprintf("Glue job %s %s: %s %s", id, n.Status, cmp.Or(n.Summary, oneLineObjective(n.Objective)), n.PRURL))
	} else {
		n.Status = WaitingForInput
		if p, ok := payload.(map[string]any); ok {
			str := func(k string) string { v, _ := p[k].(string); return v }
			itemID, n.Question = cmp.Or(str("question_id"), str("permission_id")), str("question")
			opts, _ = p["options"].([]string)
			if req, ok := p["request"]; ok {
				raw, _ := json.Marshal(req)
				var r struct{ Tool, Action, Target string }
				_ = json.Unmarshal(raw, &r)
				n.Question = strings.TrimSpace("approve " + strings.TrimSpace(r.Tool+" "+r.Action) + " " + r.Target)
			}
		}
		n.Text = strings.TrimSpace(fmt.Sprintf("Glue job %s needs you: %s (%s)", id, cmp.Or(n.Question, "waiting for input"), oneLineObjective(n.Objective)))
	}
	m.notifyMu.Lock()
	if m.notifyClosed {
		m.notifyMu.Unlock()
		return
	}
	waitCtx := m.waits[itemID]
	m.notifyWG.Add(1)
	m.notifyMu.Unlock()
	go func() {
		defer m.notifyWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), notifyCloseTimeout)
		defer cancel()
		for _, u := range urls {
			if err := postNotification(ctx, u, m.prepareNtfyWait(waitCtx, event, id, typ, itemID, opts, u, n)); err != nil {
				_ = m.record(id, "notify_failed", map[string]any{"event": event, "error": sanitizeToken(redactURL(err.Error(), u))}, "", nil, "")
			}
		}
	}()
}

func oneLineObjective(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > 80 {
		return string(r[:79]) + "…"
	}
	return s
}

// redactURL removes the notify URL, which is a secret, from an error message.
func redactURL(msg, u string) string { return strings.ReplaceAll(msg, u, "[notify URL]") }

// isNtfy reports whether u looks like an ntfy topic URL. ntfy shows the
// request body as the message, so it gets plain text instead of JSON.
func isNtfy(u string) bool { return ntfyOrigin(u) != "" }

func postNotification(ctx context.Context, u string, n Notification) error {
	body, contentType := []byte(nil), "application/json"
	headers := map[string]string{}
	if isNtfy(u) {
		body, contentType = []byte(cmp.Or(n.ntfyText, n.Text)), "text/plain; charset=utf-8"
		headers["Title"] = "Glue job " + string(n.Status)
		if click := cmp.Or(n.PRURL, n.clickURL); click != "" {
			headers["Click"] = click
		}
		if n.actions != "" {
			headers["Actions"] = n.actions
		}
	} else {
		body, _ = json.Marshal(n)
	}
	var last error
	for _, wait := range notifyBackoff {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("Content-Type", contentType)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
			err = fmt.Errorf("notify: HTTP %d", resp.StatusCode)
		}
		last = err
	}
	return last
}

func (m *Manager) trackWait(itemID string, parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	m.notifyMu.Lock()
	m.waits[itemID] = ctx
	m.notifyMu.Unlock()
	return func() { cancel(); m.notifyMu.Lock(); delete(m.waits, itemID); m.notifyMu.Unlock() }
}

func (m *Manager) jobClickURL(id string) string {
	m.credsMu.RLock()
	pub := cmp.Or(m.creds.PublicURL, strings.TrimRight(strings.TrimSpace(os.Getenv("GLUE_PUBLIC_URL")), "/"))
	m.credsMu.RUnlock()
	if ValidNotifyURL(pub) != nil {
		return ""
	}
	return pub + "/#/jobs/" + id
}

func ntfyOrigin(u string) string {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" || (!strings.HasPrefix(p.Hostname(), "ntfy.") && p.Query().Get("ntfy") != "1") {
		return ""
	}
	return p.Scheme + "://" + p.Host
}

func ntfyHTTPAction(label, replyURL string, body any) string {
	label = strings.Join(strings.Fields(label), " ")
	if r := []rune(label); len(r) > 50 {
		label = string(r[:49]) + "…"
	}
	if label == "" || strings.ContainsAny(label, `,;"'=`) {
		label = strconv.Quote(label)
	}
	b, _ := json.Marshal(body)
	return fmt.Sprintf("http, %s, %s, method=POST, body='%s', clear=true", label, replyURL, strings.ReplaceAll(string(b), "'", `\u0027`))
}

func (m *Manager) prepareNtfyWait(waitCtx context.Context, event, id, typ, itemID string, opts []string, target string, n Notification) Notification {
	origin := ntfyOrigin(target)
	if event != "waiting_for_input" || itemID == "" || waitCtx == nil || origin == "" {
		return n
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	replyTopic, tok := "glue-reply-"+hex.EncodeToString(b[:16]), hex.EncodeToString(b[16:])
	replyURL := origin + "/" + replyTopic
	redact.Register(replyURL, replyTopic, tok)
	var parts []string
	if typ == "permission_request" {
		btn := func(l, r string, a bool) string {
			return ntfyHTTPAction(l, replyURL, map[string]any{"token": tok, "permission_id": itemID, "allow": a, "remember_for": r})
		}
		parts = []string{btn("Allow once", "once", true), btn("Allow session", "session", true), btn("Deny", "once", false)}
	} else {
		for _, o := range opts {
			if o = strings.TrimSpace(o); o != "" {
				parts = append(parts, ntfyHTTPAction(o, replyURL, map[string]any{"token": tok, "question_id": itemID, "text": o}))
			}
		}
		if len(parts) == 0 || len(parts) > 3 {
			parts, n.ntfyText = append(parts[:min(len(parts), 2)], "view, Reply in ntfy, "+replyURL+", clear=true"), n.Text+"\nReply: "+replyURL
		}
	}
	n.actions = strings.Join(parts, "; ")
	m.notifyMu.Lock()
	if !m.notifyClosed {
		m.notifyWG.Add(1)
		go m.listenNtfyReply(waitCtx, id, typ, itemID, replyURL, tok, target)
	}
	m.notifyMu.Unlock()
	return n
}

func (m *Manager) listenNtfyReply(waitCtx context.Context, jobID, typ, itemID, replyURL, tok, target string) {
	subCtx, subCancel := context.WithCancel(waitCtx)
	stopAfter := context.AfterFunc(m.replyCtx, subCancel)
	defer func() { stopAfter(); subCancel(); m.notifyWG.Done() }()
	for since, wait := time.Now().Unix()-5, ntfyReplyBackoff; subCtx.Err() == nil; {
		req, _ := http.NewRequestWithContext(subCtx, http.MethodGet, fmt.Sprintf("%s/json?since=%d", replyURL, since), nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			wait = ntfyReplyBackoff
			for sc := bufio.NewScanner(resp.Body); sc.Scan(); {
				var ev struct{ Event, Message string }
				if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Event == "message" && len(ev.Message) <= maxNtfyReplyBytes {
					if confirm, done := m.applyNtfyReply(jobID, typ, itemID, tok, ev.Message); done {
						resp.Body.Close()
						if confirm != "" {
							cctx, cancel := context.WithTimeout(context.Background(), notifyCloseTimeout)
							_ = postNotification(cctx, target, Notification{JobID: jobID, Status: Running, Text: confirm, clickURL: m.jobClickURL(jobID)})
							cancel()
						}
						return
					}
				}
			}
		}
		if resp != nil {
			resp.Body.Close()
		}
		select {
		case <-subCtx.Done():
			return
		case <-time.After(wait):
			wait = min(wait*2, 10*time.Second)
		}
	}
}

func (m *Manager) applyNtfyReply(jobID, typ, itemID, tok, msg string) (string, bool) {
	answer := func(text string) (string, bool) {
		if text = strings.TrimSpace(text); text == "" {
			return "", false
		}
		if err := m.answerFrom(context.Background(), jobID, itemID, text, "ntfy"); err != nil {
			return "", errors.Is(err, ErrConflict)
		}
		return fmt.Sprintf("✓ Glue job %s: answered %q — continuing", jobID, oneLineObjective(text)), true
	}
	var p map[string]any
	if strings.HasPrefix(strings.TrimSpace(msg), "{") && json.Unmarshal([]byte(msg), &p) == nil && len(p) > 0 {
		str := func(k string) string { v, _ := p[k].(string); return v }
		if subtle.ConstantTimeCompare([]byte(str("token")), []byte(tok)) != 1 {
			return "", false
		}
		if typ == "question" && str("question_id") == itemID {
			return answer(str("text"))
		}
		allow, hasAllow := p["allow"].(bool)
		scope, ok := map[string]glue.RememberScope{"": glue.RememberNever, "once": glue.RememberNever, "never": glue.RememberNever, "session": glue.RememberSession}[strings.TrimSpace(str("remember_for"))]
		if typ != "permission_request" || str("permission_id") != itemID || !hasAllow || !ok {
			return "", false
		}
		if err := m.decideFrom(context.Background(), jobID, itemID, glue.PermissionDecision{Allow: allow, RememberFor: scope}, "ntfy"); err != nil {
			return "", errors.Is(err, ErrConflict)
		}
		label := map[bool]string{true: map[bool]string{true: "allowed for session", false: "allowed once"}[scope == glue.RememberSession], false: "denied"}[allow]
		return fmt.Sprintf("✓ Glue job %s: %s — continuing", jobID, label), true
	}
	if typ == "question" {
		return answer(msg)
	}
	return "", false
}
