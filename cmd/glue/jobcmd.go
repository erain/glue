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
	"strings"

	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
)

var jobSelectModel, jobIsTTY = selectModel, isFileTTY

func jobCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, httpClient httpDoer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, err := fmt.Fprint(stdout, "Usage:\n  glue job <login|submit|start|list|show|result|approve|deny|say|continue|cancel> [flags]\n")
		return err
	}
	fs := flag.NewFlagSet("glue job", flag.ContinueOnError)
	fs.SetOutput(stderr)
	conn := jobclient.Config{MetadataPath: defaultMetadataPath()}
	fs.StringVar(&conn.ServerURL, "server", "", "job server URL")
	fs.StringVar(&conn.Token, "token", "", "bearer token")
	fs.StringVar(&conn.MetadataPath, "metadata", conn.MetadataPath, "local daemon metadata JSON path")
	repo, objective, baseRef, backend := fs.String("repo", "", "repo"), fs.String("objective", "", "objective"), fs.String("base", "", "base ref"), fs.String("backend", "", "backend")
	provider, model, thinking := fs.String("provider", "", "provider"), fs.String("model", "", "model"), fs.String("thinking", "", "thinking")
	policy, deliver := fs.String("policy", "", "permission policy"), fs.String("deliver", "", "delivery mode")
	fs.StringVar(deliver, "delivery", "", "delivery mode alias")
	status, before, remember, reason := fs.String("status", "", "status"), fs.String("before", "", "before cursor"), fs.String("remember", "", "remember scope"), fs.String("reason", "", "reason")
	selectFlag, start, asJSON := fs.Bool("select", false, "select model"), fs.Bool("start", true, "start job"), fs.Bool("json", false, "output JSON")
	maxIter, limit, budget, wallClock := fs.Int("max-iterations", 0, "max iterations"), fs.Int("limit", 50, "limit"), fs.Int64("budget", 0, "token budget"), fs.Duration("wall-clock", 0, "wall-clock")
	var verifyRaw repeatedStrings
	fs.Var(&verifyRaw, "verify", "verification command")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) == 0 {
		return err
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
	c, err := jobclient.New(conn, httpClient)
	if err != nil {
		return err
	}
	switch sub {
	case "submit":
		obj, r := cmp.Or(strings.TrimSpace(*objective), strings.TrimSpace(strings.Join(pos, " "))), strings.TrimSpace(*repo)
		if r == "" || obj == "" {
			return errors.New("missing required --repo and --objective")
		}
		var verify [][]string
		for _, raw := range verifyRaw {
			verify = append(verify, strings.Fields(raw))
		}
		repoSpec := jobs.RepoSpec{Path: r, BaseRef: strings.TrimSpace(*baseRef)}
		if strings.Contains(r, "://") || (!strings.HasPrefix(r, "/") && !strings.HasPrefix(r, ".") && strings.Contains(r, ":") && strings.Contains(r, "@")) {
			repoSpec = jobs.RepoSpec{URL: r, BaseRef: strings.TrimSpace(*baseRef)}
		}
		job, err := c.Submit(ctx, jobs.Spec{
			Objective: obj, Repo: repoSpec, Provider: strings.TrimSpace(*provider), Model: normalizeModel(strings.TrimSpace(*model)), Thinking: strings.TrimSpace(*thinking),
			Budgets: jobs.Budgets{MaxIterations: *maxIter, TokenBudget: *budget, WallClock: *wallClock},
			Policy:  strings.TrimSpace(*policy), Verify: verify, Delivery: strings.TrimSpace(*deliver), Backend: strings.TrimSpace(*backend),
		}, *start)
		if err == nil {
			fmt.Fprintln(stdout, job.ID)
		}
		return err
	case "start":
		job, err := c.Start(ctx, pos[0])
		fmt.Fprintf(stdout, "%s\t%s\n", job.ID, job.Status)
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
	case "result":
		res, err := c.Result(ctx, pos[0])
		if err != nil || *asJSON {
			return writeJSONOrErr(stdout, res, err)
		}
		fmt.Fprintf(stdout, "job_id: %s\nstatus: %s\nbranch: %s\nbase_sha: %s\nhead_sha: %s\npr_url: %s\nsummary: %s\n", res.JobID, res.Status, res.Branch, res.BaseSHA, res.HeadSHA, res.PRURL, res.Summary)
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
	default:
		return fmt.Errorf("unknown job subcommand %q", sub)
	}
}
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
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
