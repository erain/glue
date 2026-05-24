package daemon

import (
	"sync"
	"time"

	"github.com/erain/glue"
)

type run struct {
	id        string
	sessionID string
	clientID  string
	cancel    func()
	now       func() time.Time
	newID     func(prefix string) string

	mu     sync.Mutex
	seq    int64
	events []EventEnvelope
	done   bool
	notify chan struct{}

	pending map[string]*pendingPermission
}

type pendingPermission struct {
	id   string
	done chan glue.PermissionDecision
}

func (r *run) emit(typ string, payload any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.events = append(r.events, EventEnvelope{
		Version:   protocolVersion,
		ID:        r.newID("evt"),
		Seq:       r.seq,
		RunID:     r.id,
		SessionID: r.sessionID,
		Time:      r.now().UTC(),
		Type:      typ,
		Payload:   payload,
	})
	r.signalLocked()
}

func (r *run) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	r.signalLocked()
}

func (r *run) isDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

func (r *run) eventsFrom(index int) ([]EventEnvelope, bool, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < 0 {
		index = 0
	}
	if index > len(r.events) {
		index = len(r.events)
	}
	events := append([]EventEnvelope(nil), r.events[index:]...)
	return events, r.done, r.notify
}

func (r *run) addPermission(p *pendingPermission) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.pending[p.id]; exists {
		return false
	}
	r.pending[p.id] = p
	return true
}

func (r *run) resolvePermission(id string, decision glue.PermissionDecision) bool {
	r.mu.Lock()
	pending := r.pending[id]
	if pending != nil {
		delete(r.pending, id)
	}
	r.mu.Unlock()
	if pending == nil {
		return false
	}
	pending.done <- decision
	return true
}

func (r *run) expirePermission(id string, pending *pendingPermission) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[id] != pending {
		return false
	}
	delete(r.pending, id)
	return true
}

func (r *run) signalLocked() {
	close(r.notify)
	r.notify = make(chan struct{})
}
