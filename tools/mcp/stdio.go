package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultMaxStderrBytes = 8192
	defaultShutdownWait   = 2 * time.Second
)

type stdioTransport struct {
	enc *json.Encoder
	dec *json.Decoder

	stdin io.WriteCloser
	cmd   *exec.Cmd

	stderr *limitedBuffer
	waitCh chan error

	closeOnce sync.Once
	closeErr  error
}

func startStdioTransport(cfg ServerConfig) (*stdioTransport, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("mcp: stdio command is required")
	}
	maxStderr := cfg.MaxStderrBytes
	if maxStderr <= 0 {
		maxStderr = defaultMaxStderrBytes
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = append([]string(nil), cfg.Env...)
	cmd.Dir = cfg.WorkDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stderr pipe: %w", err)
	}

	tr := &stdioTransport{
		enc:    json.NewEncoder(stdin),
		dec:    json.NewDecoder(stdout),
		stdin:  stdin,
		cmd:    cmd,
		stderr: newLimitedBuffer(maxStderr),
		waitCh: make(chan error, 1),
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start %s: %w", cfg.Command, err)
	}
	go func() {
		_, _ = io.Copy(tr.stderr, stderrPipe)
	}()
	go func() {
		tr.waitCh <- cmd.Wait()
	}()
	return tr, nil
}

func (t *stdioTransport) Encode(v any) error {
	if t == nil || t.enc == nil {
		return errors.New("mcp: nil stdio transport")
	}
	if err := t.enc.Encode(v); err != nil {
		return fmt.Errorf("mcp: write stdio message: %w", err)
	}
	return nil
}

func (t *stdioTransport) Decode(resp *rpcResponse) error {
	if t == nil || t.dec == nil {
		return errors.New("mcp: nil stdio transport")
	}
	if err := t.dec.Decode(resp); err != nil {
		return fmt.Errorf("mcp: read stdio message: %w", err)
	}
	return nil
}

func (t *stdioTransport) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		if t.stdin != nil {
			_ = t.stdin.Close()
		}
		select {
		case err := <-t.waitCh:
			t.closeErr = ignoreExpectedExit(err)
			return
		case <-time.After(defaultShutdownWait):
		}

		if t.cmd != nil && t.cmd.Process != nil {
			_ = t.cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case err := <-t.waitCh:
			t.closeErr = ignoreExpectedExit(err)
			return
		case <-time.After(defaultShutdownWait):
		}

		if t.cmd != nil && t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		if err := <-t.waitCh; err != nil {
			t.closeErr = fmt.Errorf("mcp: killed stdio server: %w", err)
		}
	})
	return t.closeErr
}

func (t *stdioTransport) Stderr() string {
	if t == nil || t.stderr == nil {
		return ""
	}
	return t.stderr.String()
}

func ignoreExpectedExit(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil && exitErr.ProcessState.Exited() {
		return nil
	}
	return err
}

type limitedBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(p)
	if b.limit <= 0 {
		return written, nil
	}
	space := b.limit - len(b.data)
	if space <= 0 {
		return written, nil
	}
	if len(p) > space {
		p = p[:space]
	}
	b.data = append(b.data, p...)
	return written, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}
