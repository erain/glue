// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// notifyCloseTimeout bounds how long notifications may run, and how long
// Manager.Close waits for those still in flight.
const notifyCloseTimeout = 20 * time.Second

// waitNotifications stops new notifications and waits up to d for the rest.
func (m *Manager) waitNotifications(d time.Duration) {
	m.notifyMu.Lock()
	m.notifyClosed = true
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
	case "permission_request":
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
	n := Notification{JobID: id, Event: event, Status: job.Status, Objective: job.Spec.Objective}
	if event == "done" {
		if res, err := m.Result(ctx, id); err == nil {
			n.Status, n.Summary, n.PRURL = res.Status, res.Summary, res.PRURL
		}
		n.Text = strings.TrimSpace(fmt.Sprintf("Glue job %s %s: %s %s", id, n.Status, cmp.Or(n.Summary, oneLineObjective(n.Objective)), n.PRURL))
	} else {
		n.Status = WaitingForInput
		if p, ok := payload.(map[string]any); ok {
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
	m.notifyWG.Add(1)
	m.notifyMu.Unlock()
	go func() {
		defer m.notifyWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), notifyCloseTimeout)
		defer cancel()
		for _, u := range urls {
			if err := postNotification(ctx, u, n); err != nil {
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
func isNtfy(u string) bool {
	p, err := url.Parse(u)
	return err == nil && strings.HasPrefix(p.Hostname(), "ntfy.")
}

func postNotification(ctx context.Context, u string, n Notification) error {
	body, contentType := []byte(nil), "application/json"
	headers := map[string]string{}
	if isNtfy(u) {
		body, contentType = []byte(n.Text), "text/plain; charset=utf-8"
		headers["Title"] = "Glue job " + string(n.Status)
		if n.PRURL != "" {
			headers["Click"] = n.PRURL
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
