package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const maxHTTPErrorBodyBytes = 4096

type httpTransport struct {
	client  *http.Client
	url     string
	headers map[string]string

	mu              sync.RWMutex
	protocolVersion string
}

func startHTTPTransport(cfg ServerConfig) (*httpTransport, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	if rawURL == "" {
		return nil, errors.New("mcp: http url is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("mcp: parse http url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("mcp: http url scheme %q is not supported", parsed.Scheme)
	}
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		key := strings.TrimSpace(k)
		if key == "" {
			return nil, errors.New("mcp: http header name is required")
		}
		headers[key] = v
	}
	return &httpTransport{
		client:  http.DefaultClient,
		url:     parsed.String(),
		headers: headers,
	}, nil
}

func (t *httpTransport) Request(ctx context.Context, req rpcRequest) (rpcResponse, error) {
	return t.post(ctx, req, true)
}

func (t *httpTransport) Notify(ctx context.Context, req rpcRequest) error {
	resp, err := t.post(ctx, req, false)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	return nil
}

func (t *httpTransport) Close() error { return nil }

func (t *httpTransport) Stderr() string { return "" }

func (t *httpTransport) SetProtocolVersion(version string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.protocolVersion = version
}

func (t *httpTransport) post(ctx context.Context, msg rpcRequest, expectResponse bool) (rpcResponse, error) {
	if t == nil || t.client == nil {
		return rpcResponse{}, errors.New("mcp: nil http transport")
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(msg); err != nil {
		return rpcResponse{}, fmt.Errorf("mcp: encode http message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, &body)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("mcp: build http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if version := t.currentProtocolVersion(); version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("mcp: http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPErrorBodyBytes))
		detail := strings.TrimSpace(string(raw))
		if detail != "" {
			return rpcResponse{}, fmt.Errorf("mcp: http status %s: %s", resp.Status, detail)
		}
		return rpcResponse{}, fmt.Errorf("mcp: http status %s", resp.Status)
	}
	if resp.StatusCode == http.StatusAccepted && !expectResponse {
		return rpcResponse{}, nil
	}

	mediaType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if mediaType == "" {
		return rpcResponse{}, errors.New("mcp: http response missing content type")
	}
	mediaType, _, err = mime.ParseMediaType(mediaType)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("mcp: parse http content type: %w", err)
	}
	switch mediaType {
	case "application/json":
		if !expectResponse && resp.ContentLength == 0 {
			return rpcResponse{}, nil
		}
		var out rpcResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			if !expectResponse && errors.Is(err, io.EOF) {
				return rpcResponse{}, nil
			}
			return rpcResponse{}, fmt.Errorf("mcp: decode http json response: %w", err)
		}
		return out, nil
	case "text/event-stream":
		return readSSEResponse(resp.Body)
	default:
		return rpcResponse{}, fmt.Errorf("mcp: unsupported http content type %q", mediaType)
	}
}

func (t *httpTransport) currentProtocolVersion() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.protocolVersion
}

func readSSEResponse(r io.Reader) (rpcResponse, error) {
	scanner := bufio.NewScanner(r)
	var data []string
	flush := func() (rpcResponse, bool, error) {
		if len(data) == 0 {
			return rpcResponse{}, false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if strings.TrimSpace(payload) == "" {
			return rpcResponse{}, false, nil
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			return rpcResponse{}, false, fmt.Errorf("mcp: decode http sse response: %w", err)
		}
		return resp, true, nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if resp, ok, err := flush(); ok || err != nil {
				return resp, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			if strings.HasPrefix(value, " ") {
				value = strings.TrimPrefix(value, " ")
			}
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return rpcResponse{}, fmt.Errorf("mcp: read http sse response: %w", err)
	}
	if resp, ok, err := flush(); ok || err != nil {
		return resp, err
	}
	return rpcResponse{}, errors.New("mcp: http sse response contained no data")
}
