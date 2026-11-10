// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package gateway

import (
	"cmp"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/internal/jobclient"
	"github.com/erain/glue/jobs"
)

// Backend is the narrow interface implemented by job execution backends.
type Backend interface {
	Create(ctx context.Context, id string, spec jobs.Spec) (jobs.Job, error)
	Start(ctx context.Context, id string) (jobs.Job, error)
	Stream(ctx context.Context, id string, after int64, handle func(eventlog.Envelope) error) error
	Do(ctx context.Context, method, subpath string, body []byte) (int, []byte, error)
	Result(ctx context.Context, id string) (jobs.Result, error)
	Cancel(ctx context.Context, id string) error
	Status(ctx context.Context, id string) (jobs.Status, error)
}

// Suspender is implemented by backends whose execution sandboxes can be suspended and resumed.
type Suspender interface {
	Suspend(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
}

// TaskDeleter is implemented by backends whose execution sandboxes can be deleted.
type TaskDeleter interface {
	DeleteTask(ctx context.Context, id string) error
}

// TaskLister is implemented by backends that can enumerate existing sandbox task names.
type TaskLister interface {
	ListTasks(ctx context.Context) ([]string, error)
}

// LocalBackend proxies job operations to a local or HTTP-reachable glue serve instance.
type LocalBackend struct{ *jobclient.Client }

// NewLocalBackend constructs a LocalBackend for the given BackendConfig.
func NewLocalBackend(cfg BackendConfig, doer jobclient.HTTPDoer) *LocalBackend {
	return &LocalBackend{&jobclient.Client{BaseURL: jobclient.NormalizeURL(cfg.URL), Token: strings.TrimSpace(cfg.Token), HTTP: cmp.Or[jobclient.HTTPDoer](doer, http.DefaultClient)}}
}
func (b *LocalBackend) Stream(ctx context.Context, id string, after int64, h func(eventlog.Envelope) error) error {
	return b.StreamEvents(ctx, id, after, h)
}
func (b *LocalBackend) Create(ctx context.Context, id string, spec jobs.Spec) (jobs.Job, error) {
	spec.ID, spec.Backend = id, ""
	return b.Submit(ctx, spec, false)
}
func (b *LocalBackend) Status(ctx context.Context, id string) (jobs.Status, error) {
	d, err := b.Show(ctx, id)
	return d.Status, err
}
func (b *LocalBackend) Do(ctx context.Context, method, subpath string, body []byte) (int, []byte, error) {
	req, _ := http.NewRequestWithContext(ctx, method, b.BaseURL+"/v1/jobs/"+strings.TrimLeft(subpath, "/"), strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+b.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, data, err
}
