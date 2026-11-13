package inbox

import (
	"context"
	"testing"

	"github.com/martinsuchenak/skopos/internal/qa"
)

type fakeRuns []RunSummary

func (f fakeRuns) ListRuns(context.Context, string) ([]RunSummary, error) { return f, nil }

// fakeQuestions answers Record only; the rule under test reads nothing else.
type fakeQuestions struct {
	QuestionLog
	rec *qa.Record
}

func (f fakeQuestions) Record(context.Context, string) (*qa.Record, error) { return f.rec, nil }

// What a retry starts after QA: QA again only when the newest QA round
// stopped with a question. An earlier QA that asked says nothing about a
// later one that tested and found something.
func TestQAAskedLooksAtTheNewestRoundOnly(t *testing.T) {
	asked := &qa.Record{Sets: []qa.Set{{RunID: "q1", Stage: qa.StageQA}}}
	cases := []struct {
		name string
		runs fakeRuns // newest first
		rec  *qa.Record
		want string
	}{
		{"QA asked", fakeRuns{
			{ID: "q1", Stage: "qa", Status: "failed", Branch: "b"},
			{ID: "i1", Stage: "implement", Status: "succeeded", Branch: "b"}}, asked, "b"},
		{"a later QA tested and found something", fakeRuns{
			{ID: "q2", Stage: "qa", Status: "failed", Branch: "b", Found: 1},
			{ID: "q1", Stage: "qa", Status: "failed", Branch: "b"},
			{ID: "i1", Stage: "implement", Status: "succeeded", Branch: "b"}}, asked, ""},
		{"a later QA failed for another reason", fakeRuns{
			{ID: "q2", Stage: "qa", Status: "failed", Branch: "b"},
			{ID: "q1", Stage: "qa", Status: "failed", Branch: "b"},
			{ID: "i1", Stage: "implement", Status: "succeeded", Branch: "b"}}, asked, ""},
		{"a second opinion after the question is left aside", fakeRuns{
			{ID: "s1", Stage: "second_opinion", Status: "succeeded", Branch: "b"},
			{ID: "q1", Stage: "qa", Status: "failed", Branch: "b"}}, asked, "b"},
		{"a panel: one member asked, none found anything", fakeRuns{
			{ID: "q2", Stage: "qa", Status: "succeeded", Branch: "b", GroupID: "g"},
			{ID: "q1", Stage: "qa", Status: "failed", Branch: "b", GroupID: "g"}}, asked, "b"},
		{"a panel: one member asked, another found something", fakeRuns{
			{ID: "q2", Stage: "qa", Status: "failed", Branch: "b", GroupID: "g", Found: 2},
			{ID: "q1", Stage: "qa", Status: "failed", Branch: "b", GroupID: "g"}}, asked, ""},
		{"the newest stage is not QA", fakeRuns{
			{ID: "i2", Stage: "implement", Status: "failed", Branch: "b"},
			{ID: "q1", Stage: "qa", Status: "failed", Branch: "b"}}, asked, ""},
		{"no question on record", fakeRuns{
			{ID: "q1", Stage: "qa", Status: "failed", Branch: "b"}}, &qa.Record{}, ""},
	}
	for _, c := range cases {
		s := &Service{runLister: c.runs, questions: fakeQuestions{rec: c.rec}}
		if got := s.qaAsked(context.Background(), &Item{ID: "item"}); got != c.want {
			t.Errorf("%s: qaAsked = %q, want %q", c.name, got, c.want)
		}
	}
}
