package daemon

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type eventBatch struct {
	events []EventEnvelope
	done   bool
	notify <-chan struct{}
}
type eventReader func(int64) (eventBatch, error)

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, read eventReader) {
	cursor := r.Header.Get("Last-Event-ID")
	if values, ok := r.URL.Query()["after"]; ok {
		cursor = values[0]
	}
	var after int64
	if cursor != "" {
		var err error
		after, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || after < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "after must be a non-negative sequence", false)
			return
		}
	}
	batch, err := read(after)
	if err != nil {
		status, code := http.StatusInternalServerError, "internal"
		if errors.Is(err, sql.ErrNoRows) {
			status, code = http.StatusNotFound, "not_found"
			err = errors.New("stream not found")
		}
		writeError(w, status, code, err.Error(), false)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported", false)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	keepalive := time.NewTicker(s.keepaliveInterval)
	defer keepalive.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		for _, event := range batch.events {
			if err := writeSSE(w, event); err != nil {
				return
			}
			after = event.Seq
			flusher.Flush()
		}
		batch.events = nil
		if batch.done {
			return
		}
		var pollC <-chan time.Time
		if batch.notify == nil {
			pollC = poll.C
		}
		select {
		case <-batch.notify:
		case <-pollC:
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		case <-r.Context().Done():
			return
		}
		batch, err = read(after)
		if err != nil {
			return
		}
	}
}
