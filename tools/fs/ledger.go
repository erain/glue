package fs

import (
	"sync"
	"time"
)

// ReadLedger tracks the modification time last observed per file so
// edit_file can flag files that changed on disk since the model read
// them — an external edit, a formatter, a git operation. read_file
// records observations; edit_file and write_file refresh them after
// their own writes so the harness's edits never count as external.
//
// Keys are the absolute resolved paths (post-SafeJoin), making "./x"
// and "x" the same entry. A nil ledger disables all tracking, so tools
// outside a coding bundle keep today's behavior.
type ReadLedger struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewReadLedger returns an empty ledger, typically shared by one run's
// read_file / edit_file / write_file instances.
func NewReadLedger() *ReadLedger {
	return &ReadLedger{seen: map[string]time.Time{}}
}

// Record stores the observed modification time for path. Nil-safe.
func (l *ReadLedger) Record(path string, mtime time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[path] = mtime
}

// Stale reports whether path was previously observed with a different
// modification time. Unobserved paths are never stale (edit-without-
// read stays allowed). Nil-safe: a nil ledger reports nothing stale.
func (l *ReadLedger) Stale(path string, current time.Time) (time.Time, bool) {
	if l == nil {
		return time.Time{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	recorded, ok := l.seen[path]
	if !ok || recorded.Equal(current) {
		return time.Time{}, false
	}
	return recorded, true
}
