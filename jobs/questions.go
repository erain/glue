// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/erain/glue"
)

// DefaultQuestionTimeout is how long a job waits for an answer before it
// proceeds on its best assumption.
const DefaultQuestionTimeout = 30 * time.Minute

// OnQuestion values: wait for an answer (up to the question timeout), or
// assume at once without asking.
const (
	OnQuestionWait   = "wait"
	OnQuestionAssume = "assume"
)

// QuestionInfo is a question a job asked its developer.
type QuestionInfo struct {
	ID       string    `json:"id"`
	Question string    `json:"question"`
	Options  []string  `json:"options,omitempty"`
	AskedAt  time.Time `json:"asked_at"`
}

type pendingQuestion struct {
	info QuestionInfo
	done chan string
}

// askUserPrompt is added to a job's system prompt.
const askUserPrompt = "\n\nYou are running unattended for a developer who can answer questions. If the objective depends on something only they know (a value, a name, a decision), or the repository contradicts what the objective assumes, call ask_user before you write code that guesses. Do not invent such values."

const assumeText = "No one answered. Proceed with your best assumption, and state that assumption in your summary."

// askUserTool lets the model ask the developer a question instead of guessing.
func (m *Manager) askUserTool(id string, run *execution) glue.Tool {
	return glue.Tool{
		ToolSpec: glue.ToolSpec{
			Name: "ask_user",
			Description: "Ask the developer who started this job a question and wait for the answer. " +
				"Use it when the repository contradicts the objective (for example a file, function, or branch the objective names does not exist), " +
				"or when a choice belongs to the developer. Ask once, briefly, and offer options when there are a few obvious answers. " +
				"Do not search outside the workspace for things the objective assumes exist; ask instead.",
			Parameters:    json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"The question, in one or two sentences."},"options":{"type":"array","items":{"type":"string"},"description":"Optional suggested answers."}},"required":["question"]}`),
			PromptSnippet: "ask_user: ask the developer when the objective's premise fails or a decision is theirs",
		},
		Execute: func(ctx context.Context, call glue.ToolCall) (glue.ToolResult, error) {
			var args struct {
				Question string   `json:"question"`
				Options  []string `json:"options"`
			}
			if err := json.Unmarshal(call.Arguments, &args); err != nil || strings.TrimSpace(args.Question) == "" {
				return glue.ErrorResult(fmt.Errorf("ask_user needs a non-empty question")), nil
			}
			answer, err := m.ask(ctx, id, run, strings.TrimSpace(args.Question), args.Options)
			if err != nil {
				return glue.ToolResult{}, err
			}
			return glue.TextResult(answer), nil
		},
	}
}

// ask records a question and blocks until it is answered, times out, or the
// job stops. An answer recorded for the same question earlier, for example
// before a restart, is returned at once.
func (m *Manager) ask(ctx context.Context, id string, run *execution, question string, options []string) (string, error) {
	job, err := m.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if ans, ok := m.recordedAnswer(ctx, id, question); ok {
		return "The developer answered: " + ans, nil
	}
	if job.Spec.OnQuestion == OnQuestionAssume {
		if err := m.record(id, "question_assumed", map[string]any{"question": question}, "", nil, ""); err != nil {
			return "", err
		}
		return assumeText, nil
	}
	timeout := job.Spec.QuestionTimeout
	if timeout <= 0 {
		timeout = DefaultQuestionTimeout
	}
	run.mu.Lock()
	info := QuestionInfo{ID: fmt.Sprintf("%s:question:%d", id, job.LastSeq+1), Question: question, Options: options, AskedAt: m.options.Now().UTC()}
	pending := &pendingQuestion{info: info, done: make(chan string, 1)}
	if run.questions == nil {
		run.questions = map[string]*pendingQuestion{}
	}
	run.questions[info.ID] = pending
	err = m.record(id, "question", map[string]any{"question_id": info.ID, "question": question, "options": options}, "", nil, WaitingForInput)
	run.mu.Unlock()
	if err != nil {
		return "", err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ans := <-pending.done:
		return "The developer answered: " + ans, nil
	case <-timer.C:
		run.mu.Lock()
		delete(run.questions, info.ID)
		err := m.record(id, "question_timeout", map[string]any{"question_id": info.ID, "question": question}, "", nil, m.activeStatus(run))
		run.mu.Unlock()
		if err != nil {
			return "", err
		}
		return assumeText, nil
	case <-ctx.Done():
		run.mu.Lock()
		delete(run.questions, info.ID)
		run.mu.Unlock()
		return "", ctx.Err()
	}
}

// activeStatus is the status of a running job given what it still waits on.
// The caller holds run.mu.
func (m *Manager) activeStatus(run *execution) Status {
	if len(run.pending) > 0 || len(run.questions) > 0 {
		return WaitingForInput
	}
	return Running
}

// Answer records the developer's answer to a job's question. questionID may
// be empty when exactly one question is open. The answer is durable: if the
// job restarts and asks the same question again, it gets this answer.
func (m *Manager) Answer(ctx context.Context, id, questionID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return invalid("text", "required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	open, err := m.openQuestions(ctx, id)
	if err != nil {
		return err
	}
	var q *QuestionInfo
	for i := range open {
		if questionID == "" || open[i].ID == questionID {
			if q != nil && questionID == "" {
				return fmt.Errorf("%w: several questions are open; pass the question ID", ErrConflict)
			}
			q = &open[i]
		}
	}
	if q == nil {
		return fmt.Errorf("%w: no open question", ErrConflict)
	}
	m.mu.Lock()
	run := m.active[id]
	m.mu.Unlock()
	if run == nil {
		return m.record(id, "answer", map[string]any{"question_id": q.ID, "question": q.Question, "text": text}, "", nil, "")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	pending := run.questions[q.ID]
	delete(run.questions, q.ID)
	if err := m.record(id, "answer", map[string]any{"question_id": q.ID, "question": q.Question, "text": text}, "", nil, m.activeStatus(run)); err != nil {
		return err
	}
	if pending != nil {
		pending.done <- text
	}
	return nil
}

// openQuestions lists questions without an answer, timeout, or later restart answer.
func (m *Manager) openQuestions(ctx context.Context, id string) ([]QuestionInfo, error) {
	events, err := m.Events(ctx, id, 0)
	if err != nil {
		return nil, err
	}
	var out []QuestionInfo
	for _, ev := range events {
		var p struct {
			ID       string   `json:"question_id"`
			Question string   `json:"question"`
			Options  []string `json:"options"`
		}
		raw, _ := json.Marshal(ev.Payload)
		_ = json.Unmarshal(raw, &p)
		switch ev.Type {
		case "question":
			out = append(out, QuestionInfo{ID: p.ID, Question: p.Question, Options: p.Options, AskedAt: ev.Time})
		case "answer", "question_timeout":
			for i := range out {
				if out[i].ID == p.ID {
					out = append(out[:i], out[i+1:]...)
					break
				}
			}
		}
	}
	return out, nil
}

// recordedAnswer returns the latest recorded answer to the same question text.
func (m *Manager) recordedAnswer(ctx context.Context, id, question string) (string, bool) {
	events, err := m.Events(ctx, id, 0)
	if err != nil {
		return "", false
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "answer" {
			continue
		}
		var p struct {
			Question string `json:"question"`
			Text     string `json:"text"`
		}
		raw, _ := json.Marshal(events[i].Payload)
		if json.Unmarshal(raw, &p) == nil && p.Question == question {
			return p.Text, true
		}
	}
	return "", false
}

// assumptions lists questions the job proceeded past without an answer.
func (m *Manager) assumptions(id string) []string {
	events, err := m.Events(context.Background(), id, 0)
	if err != nil {
		return nil
	}
	var out []string
	for _, ev := range events {
		if ev.Type == "question_timeout" || ev.Type == "question_assumed" {
			var p struct {
				Question string `json:"question"`
			}
			raw, _ := json.Marshal(ev.Payload)
			if json.Unmarshal(raw, &p) == nil && p.Question != "" {
				out = append(out, "No answer to: "+p.Question)
			}
		}
	}
	return out
}
