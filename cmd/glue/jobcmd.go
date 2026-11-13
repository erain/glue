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

	"github.com/erain/glue/internal/axbackend"
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
		_, err := fmt.Fprint(stdout, "Usage:\n  glue job <login|doctor|submit|start|list|show|attach|result|diff|pull|approve|deny|say|continue|cancel|delete|prune> [flags]\n")
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
	snapshot := fs.Bool("snapshot", false, "remote jobs: start from a snapshot of the working tree, including uncommitted changes")
	snapshotMax := fs.Int64("snapshot-max-size", defaultSnapshotMaxBytes, "remote jobs: largest new file content a snapshot may push, in bytes")
	ignoreLocal := fs.Bool("ignore-local", false, "remote jobs: start from the remote base ref even if local work would be missed")
	pullCheckout, pullApply, pullForce := fs.Bool("checkout", false, "pull: switch to the job branch"), fs.Bool("apply", false, "pull: apply the job's changes to the working tree without committing"), fs.Bool("force", false, "pull: overwrite an existing local branch or apply over local changes")
	noBudget := fs.Bool("no-budget", false, "remote jobs: do not apply the default wall-clock and token budgets")
	olderThan, dryRun := fs.String("older-than", "7d", "prune: delete finished jobs last updated before this age (e.g. 12h, 7d)"), fs.Bool("dry-run", false, "prune: list what would be deleted without deleting")
	quiet := fs.Bool("quiet", false, "submit: print only the job ID")
	verbose := fs.Bool("verbose", false, "attach: show full tool output")
	selectFlag, start, attach, asJSON := fs.Bool("select", false, "select model"), fs.Bool("start", true, "start job"), fs.Bool("attach", false, "attach to submitted job"), fs.Bool("json", false, "output JSON")
	maxIter, limit, budget, wallClock := fs.Int("max-iterations", 0, "max iterations"), fs.Int("limit", 50, "limit"), fs.Int64("budget", 0, "token budget"), fs.Duration("wall-clock", 0, "wall-clock")
	var notify repeatedStrings
	fs.Var(&notify, "notify", "remote jobs: URL to POST to when the job waits for input or finishes; repeatable (default GLUE_NOTIFY_URL)")
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
	if sub != "submit" && sub != "list" && sub != "prune" && sub != "doctor" && len(pos) == 0 {
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
	target.conn, target.remote, target.backend, target.notify = conn, remote.enabled, *backend, notify
	for _, u := range notify {
		if err := jobs.ValidNotifyURL(u); err != nil {
			return err
		}
	}
	submitStart := time.Now()
	if sub == "submit" && !*quiet {
		target.progress = func(stage string) {
			fmt.Fprintf(stderr, "  %-14s %5.1fs\n", stage, time.Since(submitStart).Seconds())
		}
	}
	if sub == "doctor" {
		return jobDoctor(ctx, target, *repo, *baseRef, *provider, *model, target.image, *asJSON, stdout, httpClient)
	}
	c, directAX, err := resolveJobAPI(ctx, target, httpClient)
	if err != nil {
		return err
	}
	defer c.Close()
	if len(pos) > 0 && sub != "submit" && sub != "list" && sub != "prune" && sub != "doctor" {
		if pos[0], err = resolveJobID(ctx, c, pos[0]); err != nil {
			return err
		}
	}
	switch sub {
	case "submit":
		obj := cmp.Or(strings.TrimSpace(*objective), strings.TrimSpace(remote.objective), strings.TrimSpace(strings.Join(pos, " ")))
		var repoSpec jobs.RepoSpec
		var jobID string
		if *snapshot {
			if !directAX {
				return errors.New("--snapshot needs direct AX execution (--ax or an AX context)")
			}
			dir := cmp.Or(strings.TrimSpace(*repo), ".")
			if isRemoteRepoRef(dir) || strings.TrimSpace(*baseRef) != "" {
				return errors.New("--snapshot works from a local checkout; drop --repo <url> and --base")
			}
			if jobID, err = jobs.NewID(); err != nil {
				return err
			}
			var ref snapshotRef
			if repoSpec, ref, err = resolveSnapshotRepo(ctx, dir, jobID, *snapshotMax); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "snapshot: %d changed file(s), %d KiB on top of %s -> origin %s\n", ref.files, ref.bytes>>10, repoSpec.SnapshotOf, repoSpec.BaseRef)
		} else if repoSpec, err = resolveSubmitRepo(ctx, directAX, remote.enabled, *ignoreLocal, *backend, *repo, *baseRef, obj); err != nil {
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
		budgets := jobs.Budgets{MaxIterations: *maxIter, TokenBudget: *budget, WallClock: *wallClock}
		if directAX {
			budgets = remoteBudgets(budgets, *noBudget)
		}
		job, err := c.Submit(ctx, jobs.Spec{
			ID: jobID, Objective: obj, Repo: repoSpec, Provider: strings.TrimSpace(*provider), Model: normalizeModel(strings.TrimSpace(*model)), Thinking: strings.TrimSpace(*thinking),
			Budgets: budgets,
			Policy:  strings.TrimSpace(*policy), Verify: verify, Delivery: strings.TrimSpace(*deliver), Backend: strings.TrimSpace(*backend),
		}, *start)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, job.ID)
		if !*quiet {
			summary := job.Spec
			if summary.Objective == "" {
				summary = jobs.Spec{
					Repo: repoSpec, Provider: strings.TrimSpace(*provider), Model: normalizeModel(strings.TrimSpace(*model)), Thinking: strings.TrimSpace(*thinking),
					Budgets: jobs.Budgets{MaxIterations: *maxIter, TokenBudget: *budget, WallClock: *wallClock},
					Policy:  strings.TrimSpace(*policy), Delivery: strings.TrimSpace(*deliver), Verify: verify,
				}
			}
			printSubmitSummary(stderr, job.ID, directAX, *start, summary)
		}
		if *attach {
			return attachJob(ctx, c, job.ID, stdin, stdout, stderr, *verbose)
		}
		return nil
	case "attach":
		return attachJob(ctx, c, pos[0], stdin, stdout, stderr, *verbose)
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
		if len(list) == 0 {
			_, err := fmt.Fprintln(stdout, "no jobs")
			return err
		}
		printJobTable(stdout, list, time.Now(), terminalWidth(stdout))
		return nil
	case "show":
		d, err := c.Show(ctx, pos[0])
		if err != nil || *asJSON {
			return writeJSONOrErr(stdout, d, err)
		}
		fmt.Fprintf(stdout, "id: %s\nstatus: %s\nobjective: %s\niteration: %d\ntokens: %d\npending_permissions: %d\n", d.ID, d.Status, d.Spec.Objective, d.Iteration, d.Usage.TotalTokens, d.PendingPermissionCount)
		if d.Spec.Repo.SnapshotOf != "" {
			fmt.Fprintf(stdout, "snapshot_of: %s\n", d.Spec.Repo.SnapshotOf)
		}
		fmt.Fprintf(stdout, "wall_clock: %s\ntoken_budget: %s\n", budgetText(d.Spec.Budgets.WallClock.String(), d.Spec.Budgets.WallClock == 0), budgetText(fmt.Sprint(d.Spec.Budgets.TokenBudget), d.Spec.Budgets.TokenBudget == 0))
		if a := d.Activity; !a.LastEventAt.IsZero() {
			fmt.Fprintf(stdout, "activity: %s, %d model turns, last tool %s, last event %s ago\n", cmp.Or(a.Phase, "-"), a.ModelTurns, cmp.Or(a.LastTool, "-"), shortAge(time.Since(a.LastEventAt)))
		}
		perms, _ := c.Permissions(ctx, d.ID, "pending")
		for _, p := range perms {
			fmt.Fprintf(stdout, "pending_permission: %s\t%s\t%s\t%s\n", p.ID, p.Request.Tool, p.Request.Action, p.Request.Target)
		}
		return nil
	case "pull":
		return pullJob(ctx, c, pos[0], ".", pullOptions{checkout: *pullCheckout, apply: *pullApply, force: *pullForce}, stdout)
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
		if res.BudgetExceeded != "" {
			fmt.Fprintf(stdout, "budget_exceeded: %s\n", res.BudgetExceeded)
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
		d, _ := c.Show(ctx, pos[0])
		if err := c.Delete(ctx, pos[0]); err != nil {
			return err
		}
		if d.Spec.Repo.SnapshotOf != "" {
			if err := deleteSnapshotBranch(ctx, ".", pos[0]); err != nil {
				fmt.Fprintf(stderr, "could not delete origin branch %s (%v); delete it by hand from a checkout of the repo\n", snapshotBranch(pos[0]), err)
			}
		}
		return nil
	case "prune":
		age, err := parseAge(*olderThan)
		if err != nil {
			return err
		}
		return pruneJobs(ctx, c, time.Now().Add(-age), *dryRun, stdout)
	default:
		return fmt.Errorf("unknown job subcommand %q", sub)
	}
}
func budgetText(v string, unlimited bool) string {
	if unlimited {
		return "unlimited"
	}
	return v
}

// printSubmitSummary reports what a submitted job got and how to follow it.
// It writes to stderr so stdout stays the bare job ID for scripts.
func printSubmitSummary(w io.Writer, id string, directAX, started bool, spec jobs.Spec) {
	where := "server"
	if directAX {
		where = "AX"
	}
	state := "started"
	if !started {
		state = "created (not started; run glue job start " + id + ")"
	}
	fmt.Fprintf(w, "job %s %s on %s\n", id, state, where)
	repo := cmp.Or(spec.Repo.URL, spec.Repo.Path)
	if spec.Repo.BaseRef != "" {
		repo += " @ " + spec.Repo.BaseRef
	}
	fmt.Fprintf(w, "  %-9s %s\n", "repo", repo)
	if prov, mod, err := resolveSelection(true, spec.Provider, spec.Model); err == nil {
		fmt.Fprintf(w, "  %-9s %s/%s (thinking: %s)\n", "model", prov, mod, cmp.Or(spec.Thinking, "default"))
	}
	fmt.Fprintf(w, "  %-9s %s · delivery %s\n", "policy", cmp.Or(spec.Policy, "auto"), cmp.Or(spec.Delivery, "none"))
	b := spec.Budgets
	fmt.Fprintf(w, "  %-9s wall clock %s · tokens %s · max %d iterations\n", "budgets", budgetSummary(b.WallClock.String(), b.WallClock == 0), budgetSummary(fmt.Sprint(b.TokenBudget), b.TokenBudget == 0), cmp.Or(b.MaxIterations, 10))
	for _, v := range spec.Verify {
		fmt.Fprintf(w, "  %-9s %s\n", "verify", strings.Join(v, " "))
	}
	fmt.Fprintf(w, "  %-9s glue job attach %s\n  %-9s glue job show %s\n  %-9s glue job cancel %s\n", "watch", id, "status", id, "stop", id)
}

func budgetSummary(v string, unlimited bool) string {
	if unlimited {
		return "unlimited"
	}
	return v
}

// parseAge parses a Go duration or a whole number of days such as "7d".
func parseAge(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		var n int
		if _, err := fmt.Sscan(days, &n); err == nil && n >= 0 && fmt.Sprint(n) == days {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid --older-than %q (use a duration such as 12h or a day count such as 7d)", raw)
	}
	return d, nil
}

// jobPruner deletes a job's backend resources while keeping its cached result.
type jobPruner interface {
	Prune(ctx context.Context, id string) error
}

// pruneJobs deletes finished jobs last updated before cutoff.
func pruneJobs(ctx context.Context, c jobAPI, cutoff time.Time, dryRun bool, stdout io.Writer) error {
	list, err := c.List(ctx, jobs.Filter{Limit: 10000})
	if err != nil {
		return err
	}
	var errs []error
	n := 0
	for _, j := range list {
		if !axbackend.IsTerminal(j.Status) || j.UpdatedAt.After(cutoff) {
			continue
		}
		n++
		if dryRun {
			fmt.Fprintf(stdout, "would delete %s\t%s\tupdated %s\n", j.ID, j.Status, j.UpdatedAt.Format(time.RFC3339))
			continue
		}
		if p, ok := c.(jobPruner); ok {
			err = p.Prune(ctx, j.ID)
		} else {
			err = c.Delete(ctx, j.ID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", j.ID, err))
			continue
		}
		fmt.Fprintf(stdout, "deleted %s\t%s\n", j.ID, j.Status)
	}
	if n == 0 {
		fmt.Fprintln(stdout, "nothing to prune")
	}
	return errors.Join(errs...)
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
func attachJob(ctx context.Context, c jobAPI, id string, stdin io.Reader, stdout, stderr io.Writer, verbose bool) error {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	input, after := bufio.NewReader(stdin), c.LoadSeq(id)
	r := newJobRenderer(stdout, verbose)
	backoff := 20 * time.Millisecond
	for {
		var done bool
		err := c.StreamEvents(ctx, id, after, func(ev eventlog.Envelope) error {
			backoff = 20 * time.Millisecond
			if err := renderJobEvent(ctx, c, id, ev, r, input, stderr); err != nil {
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
			r.flush()
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
			r.flush()
			return nil
		case <-time.After(backoff):
			backoff = min(backoff*2, 500*time.Millisecond)
		}
	}
}

// renderJobEvent renders ev and handles the interactive parts of attach:
// pending permission prompts and the closing summary.
func renderJobEvent(ctx context.Context, c jobAPI, id string, ev eventlog.Envelope, r *jobRenderer, input *bufio.Reader, stderr io.Writer) error {
	switch ev.Type {
	case "permission_request":
		r.flush()
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
		r.summary(ctx, c, id, ev)
	default:
		r.render(ev)
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
