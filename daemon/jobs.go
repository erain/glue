// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package daemon

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/erain/glue"
	"github.com/erain/glue/jobs"
)

func jobError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusInternalServerError, "internal", err.Error()
	switch {
	case errors.Is(err, sql.ErrNoRows):
		status, code, message = http.StatusNotFound, "not_found", "job not found"
	case errors.Is(err, jobs.ErrInvalid):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, jobs.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	}
	writeError(w, status, code, message, false)
}
func jobBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body: "+err.Error(), false)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must contain one JSON object", false)
		return false
	}
	return true
}
func jobMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
	return false
}
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if s.jobs == nil {
		writeError(w, http.StatusNotFound, "not_found", "jobs are not enabled", false)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/jobs"), "/")
	if path == "" {
		s.handleJobCollection(w, r)
		return
	}
	parts := strings.Split(path, "/")
	id := parts[0]
	job, err := s.jobs.Get(r.Context(), id)
	if err != nil {
		jobError(w, err)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			detail, err := s.jobs.Inspect(r.Context(), id)
			if err != nil {
				jobError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, detail)
		case http.MethodDelete:
			if err := s.jobs.Cancel(r.Context(), id); err != nil {
				jobError(w, err)
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"job_id": id, "canceled": true})
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
		}
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "start":
			if !jobMethod(w, r, http.MethodPost) {
				return
			}
			if job.Status != jobs.Running && job.Status != jobs.WaitingForInput {
				if err := s.jobs.Start(r.Context(), id); err != nil {
					current, getErr := s.jobs.Get(r.Context(), id)
					if getErr != nil {
						jobError(w, getErr)
						return
					}
					if !errors.Is(err, jobs.ErrConflict) || (current.Status != jobs.Running && current.Status != jobs.WaitingForInput) {
						jobError(w, err)
						return
					}
				}
			}
			current, err := s.jobs.Get(r.Context(), id)
			if err != nil {
				jobError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"job": current})
			return
		case "continue":
			if !jobMethod(w, r, http.MethodPost) {
				return
			}
			var body struct {
				Text          string `json:"text"`
				MaxIterations int    `json:"max_iterations"`
			}
			if !jobBody(w, r, &body) {
				return
			}
			if err := s.jobs.Continue(r.Context(), id, body.Text, body.MaxIterations); err != nil {
				jobError(w, err)
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"job_id": id, "accepted": true})
			return
		case "messages":
			if !jobMethod(w, r, http.MethodPost) {
				return
			}
			var body struct {
				Text string `json:"text"`
			}
			if !jobBody(w, r, &body) {
				return
			}
			if err := s.jobs.Message(r.Context(), id, body.Text); err != nil {
				jobError(w, err)
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"job_id": id, "accepted": true})
			return
		case "events":
			if !jobMethod(w, r, http.MethodGet) {
				return
			}
			s.streamEvents(w, r, func(after int64) (eventBatch, error) {
				current, err := s.jobs.Get(r.Context(), id)
				if err != nil {
					return eventBatch{}, err
				}
				stored, err := s.jobs.Events(r.Context(), id, after)
				if err != nil {
					return eventBatch{}, err
				}
				events := make([]EventEnvelope, len(stored))
				for i, e := range stored {
					events[i] = EventEnvelope(e)
				}
				done := current.Status != jobs.Created && current.Status != jobs.Running && current.Status != jobs.WaitingForInput
				return eventBatch{events: events, done: done}, nil
			})
			return
		case "permissions":
			if !jobMethod(w, r, http.MethodGet) {
				return
			}
			permissions, err := s.jobs.Permissions(r.Context(), id, r.URL.Query().Get("state"))
			if err != nil {
				jobError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"permissions": permissions})
			return
		}
	}
	if len(parts) == 4 && parts[1] == "permissions" && parts[2] != "" && parts[3] == "decision" {
		if !jobMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Allow       *bool  `json:"allow"`
			Reason      string `json:"reason"`
			RememberFor string `json:"remember_for"`
		}
		if !jobBody(w, r, &body) {
			return
		}
		if body.Allow == nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "allow is required", false)
			return
		}
		scope, ok := parseRememberScope(body.RememberFor)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid remember_for", false)
			return
		}
		if err := s.jobs.Decide(r.Context(), id, parts[2], glue.PermissionDecision{Allow: *body.Allow, Reason: body.Reason, RememberFor: scope}); err != nil {
			jobError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"permission_id": parts[2], "accepted": true})
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "job route not found", false)
}
func (s *Server) handleJobCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		start := false
		if raw := r.URL.Query().Get("start"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "start must be a boolean", false)
				return
			}
			start = value
		}
		var spec jobs.Spec
		if !jobBody(w, r, &spec) {
			return
		}
		job, err := s.jobs.Create(r.Context(), spec)
		if err != nil {
			jobError(w, err)
			return
		}
		if start {
			if err := s.jobs.Start(r.Context(), job.ID); err != nil {
				jobError(w, err)
				return
			}
			job, err = s.jobs.Get(r.Context(), job.ID)
			if err != nil {
				jobError(w, err)
				return
			}
		}
		writeJSON(w, http.StatusCreated, map[string]any{"job": job})
	case http.MethodGet:
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				writeError(w, http.StatusBadRequest, "invalid_request", "limit must be positive", false)
				return
			}
			limit = n
		}
		list, err := s.jobs.Query(r.Context(), jobs.Filter{Status: jobs.Status(r.URL.Query().Get("status")), Limit: limit, Before: r.URL.Query().Get("before")})
		if err != nil {
			jobError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": list})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
	}
}
