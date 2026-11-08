package daemon

import (
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue"
)

func TestPendingPermissionListedAndDecidable(t *testing.T) {
	var executed atomic.Int32
	agent := glue.NewAgent(glue.AgentOptions{Provider: &turnProvider{turns: [][]glue.ProviderEvent{toolCallTurn(), textTurn("done")}}, Tools: []glue.Tool{permissionTool(&executed)}})
	s, err := New(Options{Host: agent, Token: "token", StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	start := startRun(t, ts.URL, "default")
	_, err = collectSSE(t, ts.URL+start.EventsURL, "token", func(e EventEnvelope) {
		if e.Type != "permission_request" {
			return
		}
		detail := registryRequest(t, s, "/v1/runs/"+start.RunID, 200)
		if len(detail["pending_permissions"].([]any)) != 1 {
			t.Fatal(detail)
		}
		pending := registryRequest(t, s, "/v1/runs/"+start.RunID+"/permissions?state=pending", 200)
		if len(pending["permissions"].([]any)) != 1 {
			t.Fatal(pending)
		}
		postDecision(t, ts.URL, start.RunID, permissionID(t, e), "token", `{"allow":true}`)
	})
	if err != nil {
		t.Fatal(err)
	}
	if executed.Load() != 1 {
		t.Fatal("tool did not execute")
	}
	decided := registryRequest(t, s, "/v1/runs/"+start.RunID+"/permissions?state=allowed", 200)
	if len(decided["permissions"].([]any)) != 1 {
		t.Fatal(decided)
	}
}
func TestDecisionOnExpiredPermissionConflicts(t *testing.T) {
	s := eventServer(t, t.TempDir())
	r := s.newRun("session", "client", func() {})
	r.emit("permission_request", permissionRequestPayload{PermissionID: "old", ExpiresAt: s.now().Add(-time.Hour)})
	if _, err := s.log.transitionPermission(r.id, "old", "expired", s.now()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	postDecisionStatus(t, ts.URL, r.id, "old", "token", "", `{"allow":true}`, 409)
	postDecisionStatus(t, ts.URL, r.id, "missing", "token", "", `{"allow":true}`, 409)
}

func pendingFixture(t *testing.T) (*Server, *run, *pendingPermission) {
	s := eventServer(t, t.TempDir())
	r := s.newRun("session", "client", func() {})
	p := &pendingPermission{id: "p", done: make(chan glue.PermissionDecision, 1)}
	r.addPermission(p)
	r.emit("permission_request", permissionRequestPayload{PermissionID: p.id, ExpiresAt: s.now().Add(time.Hour)})
	return s, r, p
}
func TestPendingDecisionRacesExpiry(t *testing.T) {
	s, r, p := pendingFixture(t)
	var wg sync.WaitGroup
	var changed atomic.Int32
	wg.Add(2)
	go func() {
		defer wg.Done()
		ok, err := r.resolvePermission(p.id, glue.PermissionDecision{Allow: true})
		if err != nil {
			t.Error(err)
		}
		if ok {
			changed.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		ok, err := r.expirePermission(p.id, p)
		if err != nil {
			t.Error(err)
		}
		if ok {
			changed.Add(1)
		}
	}()
	wg.Wait()
	records, err := s.log.permissions(r.id, "")
	if err != nil || changed.Load() != 1 || len(records) != 1 || records[0].State == "pending" {
		t.Fatalf("changed %d records %v err %v", changed.Load(), records, err)
	}
}
func TestPendingDecisionWriteFailureDoesNotRelease(t *testing.T) {
	s, r, p := pendingFixture(t)
	if _, err := s.log.db.Exec("DROP TABLE permissions"); err != nil {
		t.Fatal(err)
	}
	ok, err := r.resolvePermission(p.id, glue.PermissionDecision{Allow: true})
	if err == nil || ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	select {
	case <-p.done:
		t.Fatal("released without persistence")
	default:
	}
	if _, err := r.expirePermission(p.id, p); err == nil {
		t.Fatal("expiry hid storage error")
	}
}

func TestPendingDecisionRacesRestartRecovery(t *testing.T) {
	s, r, p := pendingFixture(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := s.recoverRuns(); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := r.resolvePermission(p.id, glue.PermissionDecision{Allow: true}); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	records, err := s.log.permissions(r.id, "")
	if err != nil || len(records) != 1 || (records[0].State != "allowed" && records[0].State != "expired") {
		t.Fatalf("records %v err %v", records, err)
	}
	record, err := s.log.record(r.id)
	if err != nil || record.Status != "interrupted" {
		t.Fatalf("record %v err %v", record, err)
	}
}
