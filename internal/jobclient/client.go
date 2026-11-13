// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

// Package jobclient provides an HTTP client and credential helpers for /v1/jobs.
package jobclient

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/erain/glue/daemon"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/jobs"
)

var ErrDone = errors.New("jobclient: stream done")

type StatusError struct {
	Method, Path, Message string
	Code                  int
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s %s: %s", e.Method, e.Path, e.Message) }

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}
type Config struct{ ServerURL, Token, MetadataPath string }
type Client struct {
	BaseURL, Token string
	HTTP           HTTPDoer
}
type envelope struct {
	Job         jobs.Job              `json:"job"`
	Jobs        []jobs.Job            `json:"jobs"`
	Permissions []jobs.PermissionInfo `json:"permissions"`
}

func NormalizeURL(raw string) string { return strings.TrimRight(strings.TrimSpace(raw), "/") }
func ConfigDir() string {
	home, _ := os.UserHomeDir()
	xdg := cmp.Or(strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")), filepath.Join(cmp.Or(strings.TrimSpace(home), "."), ".config"))
	return cmp.Or(strings.TrimSpace(os.Getenv("GLUE_CONFIG_DIR")), filepath.Join(xdg, "glue"))
}
func ServersPath() string { return filepath.Join(ConfigDir(), "servers.json") }
func loadServers() (map[string]string, error) {
	m := map[string]string{}
	data, err := os.ReadFile(ServersPath())
	if err == nil && len(bytes.TrimSpace(data)) > 0 {
		return m, json.Unmarshal(data, &m)
	}
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return m, err
}
func SaveServerToken(serverURL, token string) error {
	serverURL, token = cmp.Or(NormalizeURL(serverURL), NormalizeURL(os.Getenv("GLUE_SERVER"))), cmp.Or(strings.TrimSpace(token), strings.TrimSpace(os.Getenv("GLUE_TOKEN")))
	if serverURL == "" || token == "" {
		return errors.New("jobclient: server URL and token are required")
	}
	servers, err := loadServers()
	servers[serverURL] = token
	data, _ := json.MarshalIndent(servers, "", "  ")
	path := ServersPath()
	return errors.Join(err, os.MkdirAll(filepath.Dir(path), 0o700), os.WriteFile(path, append(data, '\n'), 0o600), os.Chmod(path, 0o600))
}
func ResolveConfig(cfg Config) (Config, error) {
	var meta daemon.Metadata
	var metaErr error
	if p := strings.TrimSpace(cfg.MetadataPath); p != "" {
		meta, metaErr = daemon.ReadMetadata(p)
	}
	server := cmp.Or(NormalizeURL(cfg.ServerURL), NormalizeURL(os.Getenv("GLUE_SERVER")), NormalizeURL(meta.BaseURL))
	servers, _ := loadServers()
	if NormalizeURL(meta.BaseURL) != server {
		meta.Token = ""
	}
	token := cmp.Or(strings.TrimSpace(cfg.Token), strings.TrimSpace(os.Getenv("GLUE_TOKEN")), strings.TrimSpace(servers[server]), strings.TrimSpace(meta.Token), strings.TrimSpace(os.Getenv("GLUE_DAEMON_TOKEN")))
	if server == "" || token == "" {
		return cfg, cmp.Or(metaErr, errors.New("server URL and token are required (pass --server/--token, set GLUE_SERVER/GLUE_TOKEN, run 'glue job login', or start local glue serve)"))
	}
	return Config{ServerURL: server, Token: token, MetadataPath: cfg.MetadataPath}, nil
}
func New(cfg Config, doer HTTPDoer) (*Client, error) {
	cfg, err := ResolveConfig(cfg)
	return &Client{BaseURL: cfg.ServerURL, Token: cfg.Token, HTTP: cmp.Or[HTTPDoer](doer, http.DefaultClient)}, err
}
func doDecode[T any](ctx context.Context, c *Client, method, path string, reqBody any, status int) (T, error) {
	var out T
	return out, c.doJSON(ctx, method, path, reqBody, status, &out)
}
func (c *Client) PushCredentials(ctx context.Context, creds jobs.Credentials) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/credentials", creds, http.StatusOK, nil)
}
func (c *Client) Submit(ctx context.Context, spec jobs.Spec, start bool) (jobs.Job, error) {
	out, err := doDecode[envelope](ctx, c, http.MethodPost, fmt.Sprintf("/v1/jobs?start=%t&backend=%s", start, url.QueryEscape(strings.TrimSpace(spec.Backend))), spec, http.StatusCreated)
	return out.Job, err
}
func (c *Client) Start(ctx context.Context, id string) (jobs.Job, error) {
	out, err := doDecode[envelope](ctx, c, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/start", nil, http.StatusOK)
	return out.Job, err
}
func (c *Client) List(ctx context.Context, f jobs.Filter) ([]jobs.Job, error) {
	out, err := doDecode[envelope](ctx, c, http.MethodGet, fmt.Sprintf("/v1/jobs?status=%s&limit=%d&before=%s", url.QueryEscape(string(f.Status)), max(1, f.Limit), url.QueryEscape(f.Before)), nil, http.StatusOK)
	return out.Jobs, err
}
func (c *Client) Show(ctx context.Context, id string) (jobs.Detail, error) {
	return doDecode[jobs.Detail](ctx, c, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, http.StatusOK)
}
func (c *Client) Permissions(ctx context.Context, id, state string) ([]jobs.PermissionInfo, error) {
	out, err := doDecode[envelope](ctx, c, http.MethodGet, "/v1/jobs/"+url.PathEscape(id)+"/permissions?state="+url.QueryEscape(state), nil, http.StatusOK)
	return out.Permissions, err
}
func (c *Client) Result(ctx context.Context, id string) (jobs.Result, error) {
	return doDecode[jobs.Result](ctx, c, http.MethodGet, "/v1/jobs/"+url.PathEscape(id)+"/result", nil, http.StatusOK)
}
func (c *Client) Decide(ctx context.Context, id, permID string, allow bool, rememberFor, reason string) error {
	if !allow && strings.TrimSpace(reason) == "" {
		reason = "permission denied by user"
	}
	return c.doJSON(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/permissions/"+url.PathEscape(permID)+"/decision", map[string]any{"allow": allow, "remember_for": strings.TrimSpace(rememberFor), "reason": strings.TrimSpace(reason)}, http.StatusOK, nil)
}
func (c *Client) Say(ctx context.Context, id, text string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/messages", map[string]any{"text": text}, http.StatusAccepted, nil)
}
func (c *Client) Continue(ctx context.Context, id, text string, maxIterations int) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/continue", map[string]any{"text": text, "max_iterations": maxIterations}, http.StatusAccepted, nil)
}

// Patch returns the job's binary git diff from its base to its final commit.
func (c *Client) Patch(ctx context.Context, id string) ([]byte, error) {
	path := "/v1/jobs/" + url.PathEscape(id) + "/patch"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeStatusErr(http.MethodGet, path, resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 6<<20))
}

// Answer answers a job's open question; questionID may be empty when one is open.
func (c *Client) Answer(ctx context.Context, id, questionID, text string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/answers", map[string]string{"question_id": questionID, "text": text}, http.StatusOK, nil)
}

func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/jobs/"+url.PathEscape(id), nil, http.StatusAccepted, nil)
}
func (c *Client) Delete(ctx context.Context, id string) error {
	_ = os.Remove(SeqPath(c.BaseURL, id))
	return c.doJSON(ctx, http.MethodDelete, "/v1/jobs/"+url.PathEscape(id)+"?purge=true", nil, http.StatusOK, nil)
}
func SeqPath(serverURL, id string) string {
	sum := sha256.Sum256([]byte(NormalizeURL(serverURL)))
	return filepath.Join(ConfigDir(), "jobs", hex.EncodeToString(sum[:8]), id+".seq")
}
func (c *Client) LoadSeq(id string) int64 {
	data, err := os.ReadFile(SeqPath(c.BaseURL, id))
	if err != nil {
		return 0
	}
	seq, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return max(0, seq)
}
func (c *Client) SaveSeq(id string, seq int64) error {
	path := SeqPath(c.BaseURL, id)
	return errors.Join(os.MkdirAll(filepath.Dir(path), 0o700), os.WriteFile(path, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o600), os.Chmod(path, 0o600))
}
func (c *Client) StreamEvents(ctx context.Context, id string, after int64, handle func(eventlog.Envelope) error) error {
	path := "/v1/jobs/" + url.PathEscape(id) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "text/event-stream")
	if after > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeStatusErr(http.MethodGet, path, resp)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev eventlog.Envelope
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			return err
		}
		if err := handle(ev); err != nil {
			if errors.Is(err, ErrDone) {
				return nil
			}
			return err
		}
	}
	return scanner.Err()
}
func decodeStatusErr(method, path string, resp *http.Response) error {
	var e struct{ Error struct{ Message string } }
	_ = json.NewDecoder(resp.Body).Decode(&e)
	return &StatusError{Method: method, Path: path, Code: resp.StatusCode, Message: cmp.Or(e.Error.Message, resp.Status)}
}
func (c *Client) doJSON(ctx context.Context, method, path string, reqBody any, wantStatus int, respBody any) error {
	data, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		return decodeStatusErr(method, path, resp)
	}
	if respBody == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(respBody)
}
