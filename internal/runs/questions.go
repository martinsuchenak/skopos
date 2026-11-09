package runs

import (
	"context"
	"fmt"
	"strings"
)

// QuestionsInput is what a run asks Martin (POST /api/runs/{id}/questions):
// the planner's or the implementer's questions, what they are about, and
// whether the item waits on them.
type QuestionsInput struct {
	WorkerID    string       `json:"worker_id"`
	Asker       string       `json:"asker"`
	Blocking    bool         `json:"blocking"`
	SubjectKind string       `json:"subject_kind,omitempty"`
	Subject     string       `json:"subject,omitempty"`
	Questions   []QuestionIn `json:"questions"`
}

// QuestionIn is one question: one sentence, and the choices when there are.
type QuestionIn struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// QuestionAsker records a run's questions and renders them into its item
// (wired to internal/qa and the inbox in serve.go); it returns the record.
type QuestionAsker func(ctx context.Context, r *Run, in QuestionsInput) (any, error)

// SetQuestionAsker wires the question record.
func (s *Service) SetQuestionAsker(fn QuestionAsker) { s.questionAsker = fn }

// AskQuestions records a run's questions. Only the worker holding the run
// (its key and lease) asks for it; the run names the item, stage and
// runner, the worker what the questions are about.
func (s *Service) AskQuestions(ctx context.Context, runID, leaseToken string, in QuestionsInput) (any, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" || strings.TrimSpace(in.WorkerID) == "" || leaseToken == "" {
		return nil, fmt.Errorf("%w: run id, worker and lease are required", ErrInvalidInput)
	}
	if len(in.Questions) == 0 {
		return nil, fmt.Errorf("%w: no questions", ErrInvalidInput)
	}
	if s.questionAsker == nil {
		return nil, fmt.Errorf("%w: the question record is not configured", ErrInvalidInput)
	}
	if err := s.checkScope(ctx, runID); err != nil {
		return nil, err
	}
	if _, err := s.storage.ClaimHolder(ctx, runID, in.WorkerID, callerKeyID(ctx), hashLeaseToken(leaseToken)); err != nil {
		return nil, err
	}
	r, err := s.storage.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	return s.questionAsker(ctx, r, in)
}

// Label is how a plan group member's candidate is named ("focus@runner",
// or the runner); "" for a single run.
func (r *Run) Label() string {
	if r.GroupID == "" {
		return ""
	}
	if r.Focus != "" {
		return r.Focus + "@" + r.Runner
	}
	return r.Runner
}
