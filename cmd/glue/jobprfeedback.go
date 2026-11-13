// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/erain/glue/jobs"
)

// continueFromPR builds continue instructions from the job's pull request:
// unresolved review threads, comments since the job last finished, and
// failed checks on its head commit.
func continueFromPR(ctx context.Context, c jobAPI, id, token string) (string, error) {
	if token == "" {
		return "", errors.New("--from-pr needs a GitHub token: set GLUE_GITHUB_TOKEN or GITHUB_TOKEN, or run gh auth login")
	}
	res, err := c.Result(ctx, id)
	if err != nil {
		return "", fmt.Errorf("job %s has no result yet: %w", id, err)
	}
	if res.PRURL == "" {
		return "", fmt.Errorf("job %s has no pull request; --from-pr works for jobs delivered with --deliver pr", id)
	}
	fb, err := jobs.FetchPRFeedback(ctx, res.PRURL, res.HeadSHA, res.FinishedAt, token)
	if err != nil {
		return "", err
	}
	return fb.Instructions(), nil
}
