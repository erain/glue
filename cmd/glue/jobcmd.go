// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
)

type jobAPI interface {
	Submit(ctx context.Context, spec jobs.Spec, start bool) (jobs.Job, error)
	Start(ctx context.Context, id string) (jobs.Job, error)
	List(ctx context.Context, f jobs.Filter) ([]jobs.Job, error)
	Show(ctx context.Context, id string) (jobs.Detail, error)
	Permissions(ctx context.Context, id, state string) ([]jobs.PermissionInfo, error)
	Result(ctx context.Context, id string) (jobs.Result, error)
	Patch(ctx context.Context, id string) ([]byte, error)
	Decide(ctx context.Context, id, permID string, allow bool, rememberFor, reason string) error
	Say(ctx context.Context, id, text string) error
	Continue(ctx context.Context, id, text string, maxIterations int) error
	Cancel(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	StreamEvents(ctx context.Context, id string, after int64, handle func(eventlog.Envelope) error) error
	LoadSeq(id string) int64
	SaveSeq(id string, seq int64) error
	Close() error
}

var (
	jobSelectModel, jobIsTTY = selectModel, isFileTTY
	errJobDetached           = errors.New("job attach detached")
)

func jobCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, httpClient httpDoer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, err := fmt.Fprint(stdout, "Usage:\n  glue job <login|submit|start|list|show|attach|result|diff|approve|deny|say|continue|cancel|delete> [flags]\n")
		return err
	}
	fs := flag.NewFlagSet("glue job", flag.ContinueOnError)
	fs.SetOutput(stderr)
	conn := jobclient.Config{MetadataPath: defaultMetadataPath()}
	fs.StringVar(&conn.ServerURL, "server", "", "job server URL")
	fs.StringVar(&conn.Token, "token", "", "bearer token")
	fs.StringVar(&conn.MetadataPath, "metadata", conn.MetadataPath, "local daemon metadata JSON path")
	var remote remoteFlag
	var target jobTargetOpts
	var envs envFiles
	fs.Var(&remote, "remote", "submit job directly to AX")
	fs.BoolVar(&target.ax, "ax", false, "force direct AX execution")
	fs.StringVar(&target.kubeconfig, "kubeconfig", "", "kubeconfig path")
	fs.StringVar(&target.kubectx, "context", "", "Kubernetes context")
	fs.StringVar(&target.atespace, "atespace", "default", "AX atespace")
	fs.StringVar(&target.image, "image", "", "AX task image")
	fs.Var(&envs, "env", "env file path; repeatable")
	repo, objective, baseRef, backend := fs.String("repo", "", "repo"), fs.String("objective", "", "objective"), fs.String("base", "", "base ref"), fs.String("backend", "", "backend")
	provider, model, thinking := fs.String("provider", "", "provider"), fs.String("model", "", "model"), fs.String("thinking", "", "thinking")
	policy, deliver := fs.String("policy", "", "permission policy: auto, workspace-auto, ask-all, or yolo"), fs.String("deliver", "", "delivery mode")
	fs.StringVar(deliver, "delivery", "", "delivery mode alias")
	status, before, remember, reason := fs.String("status", "", "status"), fs.String("before", "", "before cursor"), fs.String("remember", "", "remember scope"), fs.String("reason", "", "reason")
	stat := fs.Bool("stat", false, "diff: print only the diffstat")
	selectFlag, start, attach, asJSON := fs.Bool("select", false, "select model"), fs.Bool("start", true, "start job"), fs.Bool("attach", false, "attach to submitted job"), fs.Bool("json", false, "output JSON")
	maxIter, limit, budget, wallClock := fs.Int("max-iterations", 0, "max iterations"), fs.Int("limit", 50, "limit"), fs.Int64("budget", 0, "token budget"), fs.Duration("wall-clock", 0, "wall-clock")
	var verifyRaw repeatedStrings
	fs.Var(&verifyRaw, "verify", "verification command")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) == 0 {
		return err
	}
	fs.Visit(func(f *flag.Flag) { target.metadataExplicit = target.metadataExplicit || f.Name == "metadata" })
	if len(envs) > 0 {
		if err := loadEnvFiles(envs); err != nil {
			return err
		}
	}
	sub, pos := pos[0], pos[1:]
	if sub == "login" {
		if strings.TrimSpace(conn.Token) == "" && stdin != nil {
			conn.Token, _ = bufio.NewReader(stdin).ReadString('\n')
		}
		if err := jobclient.SaveServerToken(conn.ServerURL, conn.Token); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "saved token in %s\n", jobclient.ServersPath())
		return err
	}
	if sub != "submit" && sub != "list" && len(pos) == 0 {
		return fmt.Errorf("usage: glue job %s <id>", sub)
	}
	if sub == "submit" && *selectFlag {
		if !jobIsTTY(stdin) || !jobIsTTY(stderr) {
			return errors.New("--select requires terminal stdin and stderr; use glue models and --provider/--model/--thinking for scripts")
		}
		initP, initM, err := resolveSelection(true, *provider, *model)
		if err != nil {
			return err
		}
		sel, err := jobSelectModel(ctx, stdin, stderr, modelSelection{initP, initM, *thinking})
		if err != nil {
			return err
		}
		*provider, *model, *thinking = sel.Provider, sel.Model, sel.Thinking
	}
	target.conn, target.remote, target.backend = conn, remote.enabled, *backend
	c, directAX, err := resolveJobAPI(ctx, target, httpClient)
	if err != nil {
		return err
	}
	defer c.Close()
	switch sub {
	case "submit":
		obj := cmp.Or(strings.TrimSpace(*objective), strings.TrimSpace(remote.objective), strings.TrimSpace(strings.Join(pos, " ")))
		repoSpec, err := resolveSubmitRepo(ctx, directAX, remote.enabled, *backend, *repo, *baseRef, obj)
		if err != nil {
			return err
		}
		var verify [][]string
		for _, raw := range verifyRaw {
			if fields := strings.Fields(raw); len(fields) > 0 {
				verify = append(verify, fields)
			}
		}
		if strings.TrimSpace(*deliver) == "pr" {
			*deliver = "draft_pr"
		}
		job, err := c.Submit(ctx, jobs.Spec{
			Objective: obj, Repo: repoSpec, Provider: strings.TrimSpace(*provider), Model: normalizeModel(strings.TrimSpace(*model)), Thinking: strings.TrimSpace(*thinking),
			Budgets: jobs.Budgets{MaxIterations: *maxIter, TokenBudget: *budget, WallClock: *wallClock},
			Policy:  strings.TrimSpace(*policy), Verify: verify, Delivery: strings.TrimSpace(*deliver), Backend: strings.TrimSpace(*backend),
		}, *start)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, job.ID)
		if *attach {
			return attachJob(ctx, c, job.ID, stdin, stdout, stderr)
		}
		return nil
	case "attach":
		return attachJob(ctx, c, pos[0], stdin, stdout, stderr)
	case "start":
		job, err := c.Start(ctx, pos[0])
		if err == nil {
			fmt.Fprintf(stdout, "%s\t%s\n", job.ID, job.Status)
		}
		return err
	case "list":
		list, err := c.List(ctx, jobs.Filter{Status: jobs.Status(strings.TrimSpace(*status)), Limit: *limit, Before: strings.TrimSpace(*before)})
		if err != nil || *asJSON {
			return writeJSONOrErr(stdout, map[string]any{"jobs": list}, err)
		}
		for _, j := range list {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", j.ID, j.Status, j.Spec.Objective)
		}
		return nil
	case "show":
		d, err := c.Show(ctx, pos[0])
		if err != nil || *asJSON {
			return writeJSONOrErr(stdout, d, err)
		}
		fmt.Fprintf(stdout, "id: %s\nstatus: %s\nobjective: %s\niteration: %d\ntokens: %d\npending_permissions: %d\n", d.ID, d.Status, d.Spec.Objective, d.Iteration, d.Usage.TotalTokens, d.PendingPermissionCount)
		perms, _ := c.Permissions(ctx, d.ID, "pending")
		for _, p := range perms {
			fmt.Fprintf(stdout, "pending_permission: %s\t%s\t%s\t%s\n", p.ID, p.Request.Tool, p.Request.Action, p.Request.Target)
		}
		return nil
	case "diff":
		if *stat {
			res, err := c.Result(ctx, pos[0])
			if err == nil {
				_, err = fmt.Fprintln(stdout, cmp.Or(strings.TrimRight(res.Diffstat, "\n"), "(no changes)"))
			}
			return err
		}
		patch, err := c.Patch(ctx, pos[0])
		if err == nil {
			if len(patch) == 0 {
				_, err = fmt.Fprintln(stderr, "(no changes)")
			} else {
				_, err = stdout.Write(patch)
			}
		}
		return err
	case "result":
		res, err := c.Result(ctx, pos[0])
		if err != nil || *asJSON {
			return writeJSONOrErr(stdout, res, err)
		}
		fmt.Fprintf(stdout, "job_id: %s\nstatus: %s\nbranch: %s\nbase_sha: %s\nhead_sha: %s\npr_url: %s\nsummary: %s\n", res.JobID, res.Status, res.Branch, res.BaseSHA, res.HeadSHA, res.PRURL, res.Summary)
		if res.Partial {
			fmt.Fprintln(stdout, "partial: true (unfinished work is on the branch)")
		}
		if res.DeliveryError != "" {
			fmt.Fprintf(stdout, "delivery_error: %s\n", res.DeliveryError)
		}
		for _, v := range res.Verify {
			fmt.Fprintf(stdout, "verify: %s (exit %d, %dms)\n", strings.Join(v.Argv, " "), v.ExitCode, v.DurationMS)
		}
		return nil
	case "approve", "deny", "say":
		if len(pos) < 2 {
			return fmt.Errorf("usage: glue job %s <id> <arg>", sub)
		}
		if sub == "say" {
			return c.Say(ctx, pos[0], strings.TrimSpace(strings.Join(pos[1:], " ")))
		}
		return c.Decide(ctx, pos[0], pos[1], sub == "approve", *remember, *reason)
	case "continue":
		return c.Continue(ctx, pos[0], strings.TrimSpace(strings.Join(pos[1:], " ")), *maxIter)
	case "cancel":
		return c.Cancel(ctx, pos[0])
	case "delete":
		return c.Delete(ctx, pos[0])
	default:
		return fmt.Errorf("unknown job subcommand %q", sub)
	}
}
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(args[i], "-") {
			pos = append(pos, args[i])
		} else if flags = append(flags, args[i]); !strings.Contains(args[i], "=") && i+1 < len(args) {
			if f := fs.Lookup(strings.TrimLeft(args[i], "-")); f != nil && f.DefValue != "false" && f.DefValue != "true" {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	return pos, fs.Parse(flags)
}
func writeJSONOrErr(w io.Writer, v any, err error) error {
	data, _ := json.MarshalIndent(v, "", "  ")
	if err == nil {
		_, err = fmt.Fprintf(w, "%s\n", data)
	}
	return err
}
func attachJob(ctx context.Context, c jobAPI, id string, stdin io.Reader, stdout, stderr io.Writer) error {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	input, after := bufio.NewReader(stdin), c.LoadSeq(id)
	var wroteDelta bool
	backoff := 20 * time.Millisecond
	for {
		var done bool
		err := c.StreamEvents(ctx, id, after, func(ev eventlog.Envelope) error {
			backoff = 20 * time.Millisecond
			if err := renderJobEvent(ctx, c, id, ev, &wroteDelta, input, stdout, stderr); err != nil {
				return err
			}
			after = ev.Seq
			_ = c.SaveSeq(id, after)
			if ev.Type == "job_done" {
				done = true
				return jobclient.ErrDone
			}
			return nil
		})
		if ctx.Err() != nil || errors.Is(err, errJobDetached) {
			if wroteDelta {
				fmt.Fprintln(stdout)
			}
			return nil
		}
		if done {
			return nil
		}
		var statusErr *jobclient.StatusError
		if errors.As(err, &statusErr) && statusErr.Code >= 400 && statusErr.Code < 500 {
			return err
		}
		if err == nil && after > 0 {
			if d, showErr := c.Show(ctx, id); showErr == nil && d.Status != "" && d.Status != jobs.Created && d.Status != jobs.Running && d.Status != jobs.WaitingForInput && d.Status != jobs.WaitingForCredentials && d.Status != jobs.Suspended && after >= d.LastSeq {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			if wroteDelta {
				fmt.Fprintln(stdout)
			}
			return nil
		case <-time.After(backoff):
			backoff = min(backoff*2, 500*time.Millisecond)
		}
	}
}
func renderJobEvent(ctx context.Context, c jobAPI, id string, ev eventlog.Envelope, wroteDelta *bool, input *bufio.Reader, stdout, stderr io.Writer) error {
	if ev.Type == string(glue.EventTextDelta) {
		if delta := payloadString(ev.Payload, "delta"); delta != "" && (ev.Phase == "" || ev.Phase == "maker") {
			fmt.Fprint(stdout, delta)
			*wroteDelta = !strings.HasSuffix(delta, "\n")
		}
		return nil
	}
	if *wroteDelta {
		fmt.Fprintln(stdout)
		*wroteDelta = false
	}
	switch ev.Type {
	case string(glue.GoalEventPlan), string(glue.GoalEventIterationStart), string(glue.GoalEventVerdict):
		ge, _ := decodePayload[glue.GoalEvent](ev.Payload)
		switch ev.Type {
		case string(glue.GoalEventPlan):
			fmt.Fprintf(stdout, "plan (%d items):\n%s\n", len(ge.Checklist), plainChecklist(ge.Checklist))
		case string(glue.GoalEventIterationStart):
			fmt.Fprintf(stdout, "— iteration %d (%d tokens so far)\n", ge.Iteration, ge.Usage.TotalTokens)
		case string(glue.GoalEventVerdict):
			fmt.Fprintf(stdout, "verdict: %s ✓ — %s\n", plainFraction(ge.Checklist), ge.Message)
		}
	case string(glue.EventToolStart), string(glue.EventToolEnd):
		le, _ := decodePayload[glue.Event](ev.Payload)
		name := le.ToolName
		if name == "" && le.ToolCall != nil {
			name = le.ToolCall.Name
		}
		if ev.Type == string(glue.EventToolStart) {
			if le.ToolCall != nil && len(le.ToolCall.Arguments) > 0 {
				fmt.Fprintf(stdout, "[tool] %s %s\n", name, compactJSON(le.ToolCall.Arguments))
			} else {
				fmt.Fprintf(stdout, "[tool] %s\n", name)
			}
			return nil
		}
		state, text := "done", ""
		if le.ToolResult != nil {
			if le.ToolResult.IsError {
				state = "error"
			}
			for _, p := range le.ToolResult.Content {
				if p.Text != "" {
					text = oneLine(p.Text)
					break
				}
			}
		}
		if text != "" {
			fmt.Fprintf(stdout, "[tool %s] %s: %s\n", state, name, text)
		} else {
			fmt.Fprintf(stdout, "[tool %s] %s\n", state, name)
		}
	case "operator_message":
		if text := payloadString(ev.Payload, "text"); text != "" {
			fmt.Fprintf(stdout, "operator: %s\n", text)
		}
	case "run_interrupted":
		fmt.Fprintf(stdout, "interrupted: %s\n", cmp.Or(payloadString(ev.Payload, "reason"), payloadString(ev.Payload, "message"), "run interrupted"))
	case "task_suspended":
		fmt.Fprintf(stdout, "task suspended (%s)\n", cmp.Or(payloadString(ev.Payload, "reason"), "suspended"))
	case "task_resumed":
		fmt.Fprintln(stdout, "task resumed")
	case "job_resumed":
		p, _ := decodePayload[struct {
			From   int    `json:"from_iteration"`
			Reason string `json:"reason"`
		}](ev.Payload)
		fmt.Fprintf(stdout, "resumed: iteration %d (%s)\n", p.From, cmp.Or(p.Reason, "resumed"))
	case "permission_request":
		perm, err := decodePayload[connectPermissionPayload](ev.Payload)
		if err != nil {
			return err
		}
		perms, err := c.Permissions(ctx, id, "pending")
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(perms, func(p jobs.PermissionInfo) bool { return p.ID == perm.PermissionID }) {
			return nil
		}
		decision, err := promptJobPermission(ctx, input, stderr, perm)
		if err != nil {
			return err
		}
		return c.Decide(ctx, id, perm.PermissionID, decision.Allow, decision.RememberFor, decision.Reason)
	case "job_done":
		p, _ := decodePayload[struct {
			Status jobs.Status     `json:"status"`
			Result glue.GoalResult `json:"result"`
			Error  string          `json:"error"`
		}](ev.Payload)
		if summary := cmp.Or(strings.TrimSpace(p.Result.Summary), strings.TrimSpace(p.Error)); summary != "" {
			fmt.Fprintf(stdout, "job %s — %s\n", p.Status, summary)
		} else {
			fmt.Fprintf(stdout, "job %s\n", p.Status)
		}
	}
	return nil
}
func promptJobPermission(ctx context.Context, input *bufio.Reader, stderr io.Writer, perm connectPermissionPayload) (connectPermissionDecision, error) {
	req := perm.Request
	fmt.Fprintf(stderr, "\nPermission requested: %s", strings.TrimSpace(req.Tool+" "+req.Action))
	if req.Target != "" {
		fmt.Fprintf(stderr, "\nTarget: %s", req.Target)
	}
	fmt.Fprint(stderr, "\n[d]eny, [a]llow once, allow for [s]ession, session [t]arget, [f]orever: ")
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := input.ReadString('\n')
		ch <- res{line, err}
	}()
	select {
	case <-ctx.Done():
		return connectPermissionDecision{}, errJobDetached
	case r := <-ch:
		if errors.Is(r.err, io.EOF) && strings.TrimSpace(r.line) == "" {
			return connectPermissionDecision{}, errJobDetached
		}
		if r.err != nil && !errors.Is(r.err, io.EOF) {
			return connectPermissionDecision{}, r.err
		}
		switch strings.ToLower(strings.TrimSpace(r.line)) {
		case "a", "allow", "allow once", "y", "yes":
			return connectPermissionDecision{Allow: true}, nil
		case "s", "session":
			return connectPermissionDecision{Allow: true, RememberFor: "session"}, nil
		case "t", "target", "session_target", "session target":
			return connectPermissionDecision{Allow: true, RememberFor: "session_target"}, nil
		case "f", "forever":
			return connectPermissionDecision{Allow: true, RememberFor: "forever"}, nil
		default:
			return connectPermissionDecision{Allow: false, Reason: "permission denied by user"}, nil
		}
	}
}
