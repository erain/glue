// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package axbackend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/internal/eventlog"
	"github.com/erain/glue/jobs"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

// finishedJobHandler serves a task whose job reports status (and a result
// once done is true).
func finishedJobHandler(status *atomic.Value) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jobID := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/")[0]
		st := status.Load().(jobs.Status)
		switch {
		case r.URL.Path == "/v1/credentials":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			ev, _ := json.Marshal(eventlog.Envelope{Version: 2, ID: jobID + ":1", RunID: jobID, Seq: 1, Type: "job_done", Payload: map[string]any{"status": string(st)}})
			_, _ = fmt.Fprintf(w, "id: 1\ndata: %s\n\n", ev)
		case strings.HasSuffix(r.URL.Path, "/result"):
			if !IsTerminal(st) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"job is not finished"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(jobs.Result{JobID: jobID, Status: st, Summary: "done"})
		case r.Method == http.MethodDelete:
			status.Store(jobs.Canceled)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = fmt.Fprintf(w, `{"id":%q,"status":%q,"last_seq":5,"credentials":"present"}`, jobID, st)
		}
	}
}

func markRunning(cp *fakeAXControlPlane, id string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.tasks[TaskName(id)].Status = &v1alpha1.TaskStatus{
		State:      v1alpha1.TaskState_TASK_STATE_WORKING,
		Conditions: []*v1alpha1.Condition{{Type: v1alpha1.ConditionWorkspaceReady, Status: "True"}, {Type: v1alpha1.ConditionReady, Status: "True"}},
	}
}

func taskState(cp *fakeAXControlPlane, id string) v1alpha1.TaskState {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.tasks[TaskName(id)].GetStatus().GetState()
}

func TestListReconcilesFinishedRunningTask(t *testing.T) {
	var st atomic.Value
	st.Store(jobs.Achieved)
	cp, _, _, b := newFakeBackendEnv(t, finishedJobHandler(&st))
	id := "job-reconcile0001"
	must(b.Create(t.Context(), id, jobs.Spec{Objective: "Finish", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	b.recordState(t.Context(), id, jobs.Spec{Objective: "Finish"}, jobs.Running, time.Time{})
	markRunning(cp, id)

	list := must(b.ListJobs(t.Context(), jobs.Filter{}))(t)
	check(t, len(list) == 1 && list[0].Status == jobs.Achieved, "ListJobs = %+v", list)
	check(t, taskState(cp, id) == v1alpha1.TaskState_TASK_STATE_SUSPENDED, "finished task state = %v, want Suspended", taskState(cp, id))
	c := b.loadCache(id)
	check(t, c != nil && c.Result != nil && c.Result.Summary == "done", "cache = %+v", c)

	hits := cp.taskHTTPHits.Load()
	list = must(b.ListJobs(t.Context(), jobs.Filter{}))(t)
	check(t, list[0].Status == jobs.Achieved && cp.taskHTTPHits.Load() == hits, "second list woke the suspended task (%d hits)", cp.taskHTTPHits.Load()-hits)
}

func TestListLeavesActiveRunningTaskAwake(t *testing.T) {
	var st atomic.Value
	st.Store(jobs.Running)
	cp, _, _, b := newFakeBackendEnv(t, finishedJobHandler(&st))
	id := "job-reconcile0002"
	must(b.Create(t.Context(), id, jobs.Spec{Objective: "Work", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	markRunning(cp, id)
	list := must(b.ListJobs(t.Context(), jobs.Filter{}))(t)
	check(t, len(list) == 1 && list[0].Status == jobs.Running && taskState(cp, id) == v1alpha1.TaskState_TASK_STATE_WORKING, "active job: list=%+v state=%v", list, taskState(cp, id))
}

func TestAttachSuspendsAfterJobDone(t *testing.T) {
	var st atomic.Value
	st.Store(jobs.Achieved)
	cp, _, _, b := newFakeBackendEnv(t, finishedJobHandler(&st))
	id := "job-streamdone001"
	must(b.Create(t.Context(), id, jobs.Spec{Objective: "Stream", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	markRunning(cp, id)
	var types []string
	mustOK(t, b.Stream(t.Context(), id, 0, func(ev eventlog.Envelope) error { types = append(types, ev.Type); return nil }))
	check(t, len(types) == 1 && types[0] == "job_done", "events = %v", types)
	check(t, taskState(cp, id) == v1alpha1.TaskState_TASK_STATE_SUSPENDED, "state after job_done = %v", taskState(cp, id))
	check(t, b.loadCache(id).Result != nil && must(b.Show(t.Context(), id))(t).LastSeq == 5, "result/detail not cached after job_done")

	// When a caller such as glue-gateway suspends inside the Stream callback,
	// settleFinished must not contact the router and wake the task back up.
	id2 := "job-streamdone002"
	must(b.Create(t.Context(), id2, jobs.Spec{Objective: "Gateway stream", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	markRunning(cp, id2)
	var hitsAfterSuspend int32
	mustOK(t, b.Stream(t.Context(), id2, 0, func(ev eventlog.Envelope) error {
		mustOK(t, b.Suspend(t.Context(), id2))
		hitsAfterSuspend = cp.taskHTTPHits.Load()
		return nil
	}))
	check(t, cp.taskHTTPHits.Load() == hitsAfterSuspend && taskState(cp, id2) == v1alpha1.TaskState_TASK_STATE_SUSPENDED, "settleFinished contacted suspended task: hits %d->%d", hitsAfterSuspend, cp.taskHTTPHits.Load())
}

func TestCancelSuspends(t *testing.T) {
	var st atomic.Value
	st.Store(jobs.Running)
	cp, _, _, b := newFakeBackendEnv(t, finishedJobHandler(&st))
	id := "job-cancelsusp01"
	must(b.Create(t.Context(), id, jobs.Spec{Objective: "Cancel me", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	markRunning(cp, id)
	mustOK(t, b.Cancel(t.Context(), id))
	check(t, taskState(cp, id) == v1alpha1.TaskState_TASK_STATE_SUSPENDED, "state after cancel = %v", taskState(cp, id))
	check(t, b.loadCache(id).Status == jobs.Canceled, "cached status = %q", b.loadCache(id).Status)
}

func TestPruneKeepsCachedResult(t *testing.T) {
	var st atomic.Value
	st.Store(jobs.Achieved)
	cp, _, _, b := newFakeBackendEnv(t, finishedJobHandler(&st))
	id := "job-prunekeep001"
	must(b.Create(t.Context(), id, jobs.Spec{Objective: "Prune", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	markRunning(cp, id)
	mustOK(t, b.Prune(t.Context(), id))
	cp.mu.Lock()
	_, taskLeft := cp.tasks[TaskName(id)]
	_, gwLeft := cp.gateways[GatewayName(id)]
	cp.mu.Unlock()
	check(t, !taskLeft && !gwLeft, "prune left task=%v gateway=%v", taskLeft, gwLeft)
	res := must(b.Result(t.Context(), id))(t)
	check(t, res.Status == jobs.Achieved && res.Summary == "done", "result after prune = %+v", res)
}

func TestShowSuspendedUsesCachedResult(t *testing.T) {
	var st atomic.Value
	st.Store(jobs.Achieved)
	cp, _, _, b := newFakeBackendEnv(t, finishedJobHandler(&st))
	id := "job-showresult01"
	must(b.Create(t.Context(), id, jobs.Spec{Objective: "Show", Repo: jobs.RepoSpec{URL: "https://github.com/erain/glue-sandbox"}}))(t)
	b.updateCache(id, func(c *cachedJob) {
		c.Status, c.Result = jobs.Achieved, &jobs.Result{JobID: id, Status: jobs.Achieved, Iterations: 2, Usage: glue.Usage{TotalTokens: 71357}}
	})
	_ = cp
	d := must(b.Show(t.Context(), id))(t)
	check(t, d.Iteration == 2 && d.Usage.TotalTokens == 71357, "show of a suspended finished job = iteration %d tokens %d", d.Iteration, d.Usage.TotalTokens)
}
