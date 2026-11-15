// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/internal/redact"
	"github.com/erain/glue/jobs"
	filestore "github.com/erain/glue/stores/file"
)

const teleportBriefingLimit = 8000

func browserBaseURL(serverURL string, ucfg *userConfig) string {
	var uPub string
	if ucfg != nil {
		uPub = ucfg.PublicURL
	}
	if pub := strings.TrimRight(cmp.Or(strings.TrimSpace(os.Getenv("GLUE_PUBLIC_URL")), strings.TrimSpace(uPub)), "/"); pub != "" {
		return pub
	}
	srv := strings.TrimRight(cmp.Or(strings.TrimSpace(serverURL), strings.TrimSpace(os.Getenv("GLUE_SERVER"))), "/")
	if u, err := url.Parse(srv); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" {
		if h := strings.ToLower(u.Hostname()); h != "localhost" && (net.ParseIP(h) == nil || !net.ParseIP(h).IsLoopback()) {
			return srv
		}
	}
	return ""
}

func teleportSessionID(jobID string) string {
	s := strings.Map(func(r rune) rune {
		return map[bool]rune{true: r, false: '-'}[(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_']
	}, strings.TrimPrefix(strings.TrimSpace(jobID), "job-"))
	return "teleport-" + cmp.Or(strings.Trim(s, "-"), "job")
}

func dirtyWorkingTree(ctx context.Context, dir string) bool {
	out, _ := gitOutput(ctx, dir, "status", "--porcelain", "--untracked-files=all")
	for line := range strings.SplitSeq(out, "\n") {
		if p := strings.Trim(line[min(len(line), 3):], `"`); len(line) > 3 && p != ".glue" && !strings.HasPrefix(p, ".glue/") {
			return true
		}
	}
	return false
}

func teleportJob(ctx context.Context, c jobAPI, id, dir, storeDir string, o pullOptions, stdout io.Writer) (string, string, error) {
	if o.checkout && o.apply {
		return "", "", errors.New("--checkout and --apply cannot be combined")
	}
	d, err := c.Show(ctx, id)
	if err == nil {
		err = checkPullRepo(ctx, id, dir, d.Spec.Repo.URL)
	}
	if err != nil {
		return "", "", err
	}
	if s, ok := c.(jobSuspender); ok {
		if was, err := s.IsSuspended(ctx, id); err == nil && was {
			defer func() { _ = s.Suspend(context.WithoutCancel(ctx), id) }()
		}
	}
	res, rerr := c.Result(ctx, id)
	var patch []byte
	if rerr != nil || res.HeadSHA == "" {
		patch, _ = c.Patch(ctx, id)
	}
	if (rerr != nil || res.HeadSHA == "" || res.HeadSHA == res.BaseSHA) && len(bytes.TrimSpace(patch)) == 0 {
		fmt.Fprintf(stdout, "job %s made no file changes; hydrating session context only\n", id)
	} else {
		if rerr != nil {
			stat, _ := gitWithStdin(ctx, dir, patch, "apply", "--stat", "-")
			res = jobs.Result{JobID: id, Status: d.Status, Branch: "glue/" + id, BaseRef: d.Spec.Repo.BaseRef, BaseSHA: d.BaseSHA, HeadSHA: "working-tree", Diffstat: stat}
		}
		snapBranch, _, _ := strings.Cut(d.Spec.Repo.SnapshotOf, "@")
		curBranch, _ := gitOutput(ctx, dir, "branch", "--show-current")
		dirty := dirtyWorkingTree(ctx, dir)
		if !o.apply && !o.checkout {
			o.apply = (snapBranch != "" && snapBranch == curBranch) || (dirty && o.force)
			o.checkout = !o.apply && !dirty
		}
		if (!o.apply && !o.checkout) || (o.checkout && !o.force && dirty) {
			return "", "", fmt.Errorf("working tree has uncommitted changes; pass --apply, --force, or commit/stash local edits before teleporting %s", id)
		}
		if curSHA, _ := gitOutput(ctx, dir, "rev-parse", "HEAD"); !(o.checkout && curBranch == res.Branch && curSHA == res.HeadSHA) {
			if err := pullJobChanges(ctx, c, id, dir, res, o, stdout); err != nil {
				return "", "", err
			}
		}
	}
	storeDir = cmp.Or(strings.TrimSpace(storeDir), filepath.Join(dir, ".glue", "sessions"))
	sid, now := teleportSessionID(id), time.Now().UTC()
	msg := func(r glue.MessageRole, t string) glue.Message {
		return glue.Message{Role: r, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: t}}, CreatedAt: now}
	}
	u := redact.String(fmt.Sprintf("Handoff from remote job %s: %s", id, cmp.Or(strings.TrimSpace(d.Spec.Objective), "(no objective)")))
	return sid, storeDir, filestore.New(storeDir).Save(ctx, sid, glue.SessionState{Version: glue.SessionStateVersion, ID: sid, CreatedAt: now, UpdatedAt: now, Messages: []glue.Message{msg(glue.MessageRoleUser, u), msg(glue.MessageRoleAssistant, buildTeleportBriefing(ctx, c, id, d, res, patch))}})
}

func buildTeleportBriefing(ctx context.Context, c jobAPI, id string, d jobs.Detail, res jobs.Result, patch []byte) string {
	type qa struct{ id, q, a, opts string }
	var qas []qa
	var says []string
	streamBuf, goalNote, doneErr, checklist := "", "", "", d.Checklist
	if d.LastSeq > 0 {
		_ = c.StreamEvents(ctx, id, 0, func(ev eventlog.Envelope) error {
			switch ev.Type {
			case "question":
				if qi, _ := decodePayload[jobs.QuestionInfo](ev.Payload); qi.Question != "" {
					qas = append(qas, qa{id: cmp.Or(payloadString(ev.Payload, "question_id"), qi.ID), q: qi.Question, opts: strings.Join(qi.Options, " | ")})
				}
			case "answer", "question_timeout", "question_assumed":
				qid, qq, raw := payloadString(ev.Payload, "question_id"), payloadString(ev.Payload, "question"), strings.TrimSpace(payloadString(ev.Payload, "text"))
				ans := cmp.Or(raw, "(unanswered; proceeded on assumption)")
				if i := slices.IndexFunc(qas, func(x qa) bool { return (qid != "" && x.id == qid) || (qq != "" && x.q == qq) }); i >= 0 {
					qas[i].a = ans
				} else if qq != "" || raw != "" {
					qas = append(qas, qa{id: qid, q: cmp.Or(qq, "(question)"), a: ans})
				}
			case "operator_message":
				if t := strings.TrimSpace(payloadString(ev.Payload, "text")); t != "" {
					says = append(says, t)
				}
			case string(glue.EventTextDelta):
				if ev.Phase == "" || ev.Phase == "maker" || ev.Phase == "chat" {
					streamBuf = tailRunes(streamBuf+payloadString(ev.Payload, "delta"), 1000)
				}
			case "plan", "verdict", "maker_done", "done", "blocked", "budget_limited", "max_iterations", "job_done":
				ge, _ := decodePayload[glue.GoalEvent](ev.Payload)
				if td, _ := decodePayload[struct{ Result *glue.GoalResult }](ev.Payload); td.Result != nil {
					ge.Checklist, ge.Message = td.Result.Checklist, cmp.Or(td.Result.Summary, ge.Message)
				}
				if len(ge.Checklist) > 0 {
					checklist = ge.Checklist
				}
				goalNote = cmp.Or(strings.TrimSpace(ge.Message), goalNote)
				doneErr = cmp.Or(strings.TrimSpace(payloadString(ev.Payload, "error")), doneErr)
			}
			return map[bool]error{true: jobclient.ErrDone}[ev.Seq >= d.LastSeq]
		})
	}
	if q := d.Question; q != nil && !slices.ContainsFunc(qas, func(x qa) bool { return (q.ID != "" && x.id == q.ID) || x.q == q.Question }) {
		qas = append(qas, qa{id: q.ID, q: q.Question, opts: strings.Join(q.Options, " | ")})
	}
	var b strings.Builder
	opt := func(ok bool, s string) string { return map[bool]string{true: s}[ok] }
	sec := func(title, body string) {
		if strings.TrimSpace(body) != "" {
			fmt.Fprintf(&b, "\n## %s\n%s\n", title, strings.TrimRight(body, "\n"))
		}
	}
	st := string(cmp.Or(res.Status, d.Status)) + opt(d.StatusReason != "", " ("+d.StatusReason+")") + opt(res.Partial, " [partial]") + opt(res.BudgetExceeded != "", " [budget: "+res.BudgetExceeded+"]")
	bSHA, hSHA := cmp.Or(res.BaseSHA, d.BaseSHA), res.HeadSHA
	fmt.Fprintf(&b, "# Remote job handoff: %s\n- mode: %s · status: %s\n- repo: %s · base_ref: %s (%s -> %s)\n", id, cmp.Or(strings.TrimSpace(d.Spec.Mode), jobs.ModeGoal), st, cmp.Or(strings.TrimSpace(d.Spec.Repo.URL), strings.TrimSpace(d.Spec.Repo.Path), "(local)"), cmp.Or(strings.TrimSpace(res.BaseRef), strings.TrimSpace(d.Spec.Repo.BaseRef), "HEAD"), cmp.Or(bSHA[:min(len(bSHA), 8)], "-"), cmp.Or(hSHA[:min(len(hSHA), 8)], bSHA[:min(len(bSHA), 8)], "-"))
	for _, kv := range [][2]string{{"snapshot_of", d.Spec.Repo.SnapshotOf}, {"issue", d.Spec.Issue}, {"pr_url", cmp.Or(res.PRURL, d.PRURL)}} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "- %s: %s\n", kv[0], kv[1])
		}
	}
	sec("Objective", cmp.Or(strings.TrimSpace(d.Spec.Objective), "(none)"))
	sec("Session / issue context", truncateRunes(strings.TrimSpace(d.Spec.Context), 1200))
	var items, notes []string
	for _, it := range checklist {
		ev := strings.TrimSpace(it.Evidence)
		items = append(items, fmt.Sprintf("- [%s] %s%s", opt(it.Done, "x")+opt(!it.Done, " "), strings.TrimSpace(it.Title), opt(ev != "", " — "+ev)))
	}
	sec("Plan checklist", strings.Join(items, "\n"))
	for _, x := range qas {
		notes = append(notes, "- "+cmp.Or(opt(x.a != "", "ask_user: "+x.q+" → answer: "+x.a), "open question: "+x.q+opt(x.opts != "", " (options: "+x.opts+")")))
	}
	for _, a := range res.Assumptions {
		notes = append(notes, "- assumption: "+a)
	}
	for _, m := range says {
		notes = append(notes, "- say: "+m)
	}
	sec("Questions & operator messages", strings.Join(notes, "\n"))
	fmt.Fprintf(&b, "\n## Changes & verification\n%s\n", cmp.Or(strings.TrimSpace(res.Diffstat), opt(len(bytes.TrimSpace(patch)) > 0, fmt.Sprintf("patch (%d bytes)", len(patch))), "(no file changes)"))
	for _, v := range res.Verify {
		fmt.Fprintf(&b, "- %s %s (exit %d)\n", opt(v.ExitCode == 0, "✓")+opt(v.ExitCode != 0, "✗"), strings.Join(v.Argv, " "), v.ExitCode)
		if tail := strings.TrimSpace(v.OutputTail); tail != "" {
			fmt.Fprintf(&b, "```\n%s\n```\n", tailRunes(tail, map[bool]int{true: 400, false: 1500}[v.ExitCode == 0]))
		}
	}
	sec("Summary / notes", truncateRunes(strings.TrimSpace(cmp.Or(strings.TrimSpace(res.Summary), doneErr, goalNote, strings.TrimSpace(streamBuf))+opt(res.DeliveryError != "", "\ndelivery_error: "+res.DeliveryError)), 1500))
	return truncateRunes(redact.String(strings.TrimSpace(b.String())), teleportBriefingLimit)
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	return map[bool]string{true: "…" + string(r[max(len(r)-n+1, 0):]), false: s}[len(r) > n && n > 1]
}
