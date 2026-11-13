// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"strings"

	"github.com/erain/glue/cmd/glue/tui"
)

// submitRemoteFromTUI runs the CLI submit path for /remote, so the TUI gets
// the same local-state checks, --snapshot and .glue/remote.yaml handling.
func submitRemoteFromTUI(ctx context.Context, workDir string, req tui.RemoteRequest, progress func(string)) (string, error) {
	args := []string{"submit", "--remote", "--repo", cmp.Or(workDir, ".")}
	if req.Context != "" {
		args = append(args, "--notes", req.Context)
	}
	if req.Snapshot {
		args = append(args, "--snapshot")
	}
	var stdout bytes.Buffer
	lines := &lineWriter{emit: progress}
	err := jobCommand(ctx, append(args, "--objective", req.Objective), strings.NewReader(""), &stdout, lines, nil)
	lines.flush()
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(stdout.String())
	if id == "" {
		return "", errors.New("submit printed no job ID")
	}
	return id, nil
}

// lineWriter calls emit for each complete line written to it.
type lineWriter struct {
	emit func(string)
	buf  []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		if w.emit != nil {
			w.emit(string(w.buf[:i]))
		}
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 && w.emit != nil {
		w.emit(string(w.buf))
	}
	w.buf = nil
}
