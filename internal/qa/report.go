package qa

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Filter narrows the cross-item read (Search). Zero values match all.
type Filter struct {
	WorkspaceID string
	ItemID      string
	State       string // a question state, or "free" for answers to no question
	Asker       string
	AnsweredBy  string // the answering actor (exact, "key alex", "root")
	From, To    time.Time
}

// Row is one line of the decision log: a question with its state and
// current answer, or a free answer given when no question was open
// (Kind "free"). Item titles are filled by the caller.
type Row struct {
	Kind        string    `json:"kind"` // question | free
	WorkspaceID string    `json:"workspace_id"`
	ItemID      string    `json:"item_id"`
	ItemTitle   string    `json:"item_title,omitempty"`
	SetID       string    `json:"set_id,omitempty"`
	Asker       string    `json:"asker,omitempty"`
	Label       string    `json:"label,omitempty"`
	RunID       string    `json:"run_id,omitempty"`
	SubjectKind string    `json:"subject_kind,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	Blocking    bool      `json:"blocking,omitempty"`
	QuestionID  string    `json:"question_id,omitempty"`
	N           int       `json:"n,omitempty"`
	Question    string    `json:"question,omitempty"`
	State       string    `json:"state,omitempty"`
	AskedAt     time.Time `json:"asked_at,omitzero"`
	// The current answer (the free text for answered_free_text, the
	// skipping person for skipped).
	AnswerID    string    `json:"answer_id,omitempty"`
	Option      string    `json:"option,omitempty"`
	OptionText  string    `json:"option_text,omitempty"`
	Answer      string    `json:"answer,omitempty"`
	AnsweredBy  string    `json:"answered_by,omitempty"`
	Via         string    `json:"via,omitempty"`
	Action      string    `json:"action,omitempty"`
	ApprovalID  string    `json:"approval_id,omitempty"`
	AnsweredAt  time.Time `json:"answered_at,omitzero"`
	UsedByRun   string    `json:"used_by_run,omitempty"`
	Corrections int       `json:"corrections,omitempty"`
	AfterUse    bool      `json:"after_use,omitempty"`
	// Waited: from asked to the first answer or skip (seconds), for answered,
	// answered in free text and skipped questions asked and answered live.
	WaitedSeconds int64 `json:"waited_seconds,omitempty"`
	waited        bool  // WaitedSeconds is measured (it can be 0)
}

// Search is the decision log across items, oldest question first: every
// question in the filter's range (asked between From and To) with its state
// and current answer, and the free answers given in the range. Callers
// check scope (the inbox service filters rows by workspace).
func (s *Service) Search(ctx context.Context, f Filter) ([]Row, error) {
	items, err := s.storage.ItemsWithRecord(ctx, f.WorkspaceID, f.ItemID, f.From, f.To)
	if err != nil {
		return nil, err
	}
	var rows []Row
	for _, it := range items {
		rec, err := s.Record(ctx, it)
		if err != nil {
			return nil, err
		}
		rows = append(rows, rowsOf(rec, it)...)
	}
	var out []Row
	for _, r := range rows {
		if f.match(r) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at().Before(out[j].at()) })
	return out, nil
}

func (r Row) at() time.Time {
	if r.Kind == "free" {
		return r.AnsweredAt
	}
	return r.AskedAt
}

func (f Filter) match(r Row) bool {
	at := r.at()
	switch {
	case !f.From.IsZero() && at.Before(f.From):
		return false
	case !f.To.IsZero() && !at.Before(f.To):
		return false
	case f.State == "free" && r.Kind != "free":
		return false
	case f.State != "" && f.State != "free" && r.State != f.State:
		return false
	case f.Asker != "" && r.Asker != f.Asker:
		return false
	case f.AnsweredBy != "" && r.AnsweredBy != f.AnsweredBy:
		return false
	}
	return true
}

func rowsOf(rec *Record, itemID string) []Row {
	var out []Row
	for _, st := range rec.Sets {
		for _, q := range st.Questions {
			r := Row{
				Kind: "question", WorkspaceID: st.WorkspaceID, ItemID: itemID, SetID: st.ID, Asker: st.Asker,
				Label: st.Label, RunID: st.RunID, SubjectKind: st.SubjectKind, Subject: st.Subject, Blocking: st.Blocking,
				QuestionID: q.ID, N: q.N, Question: q.Text, State: q.State, AskedAt: st.CreatedAt,
			}
			var first time.Time
			for _, a := range q.Answers {
				if first.IsZero() || a.CreatedAt.Before(first) {
					first = a.CreatedAt
				}
				if a.Replaces != "" {
					r.Corrections++
				}
			}
			switch q.State {
			case StateAnswered:
				a := current(q)
				r.fill(*a)
				r.Option, r.OptionText, r.AfterUse = a.Option, optionAt(q, a.Option), a.AfterUse
			case StateAnsweredFreeText:
				g := st.General[len(st.General)-1]
				r.fill(g)
				if first.IsZero() || g.CreatedAt.Before(first) {
					first = g.CreatedAt
				}
			case StateSkipped:
				for _, a := range q.Answers {
					if a.Kind == KindSkipped {
						r.fill(a)
						break
					}
				}
			}
			// The wait counts only what was asked and answered live: an
			// import dates both from the item, a meaningless wait.
			if !first.IsZero() && q.State != StateOpen && q.State != StateSuperseded && st.Source != SourceImported && r.Action != ActionImported {
				r.WaitedSeconds = int64(first.Sub(st.CreatedAt).Seconds())
				r.waited = true
			}
			out = append(out, r)
		}
	}
	for _, a := range rec.Free {
		r := Row{Kind: "free", WorkspaceID: a.WorkspaceID, ItemID: itemID}
		r.fill(a)
		out = append(out, r)
	}
	return out
}

func (r *Row) fill(a Answer) {
	r.AnswerID, r.Answer, r.AnsweredBy, r.Via, r.Action = a.ID, a.Text, a.Actor, a.Via, a.Action
	r.ApprovalID, r.AnsweredAt, r.UsedByRun = a.ApprovalID, a.CreatedAt, a.UsedByRun
}

// Counts is how many questions ended which way, and the answers given.
type Counts struct {
	Asked               int `json:"asked"`
	Answered            int `json:"answered"`
	AnsweredFreeText    int `json:"answered_free_text"`
	Skipped             int `json:"skipped"`
	Open                int `json:"open"`
	Superseded          int `json:"superseded"`
	FreeAnswers         int `json:"free_answers"`
	Corrections         int `json:"corrections"`
	CorrectionsAfterUse int `json:"corrections_after_use"`
	// MedianWaitSeconds: asked to first answer or skip, over the questions
	// asked and answered live (Measured of them; imports have no wait).
	MedianWaitSeconds int64 `json:"median_wait_seconds"`
	Measured          int   `json:"measured"`
}

// Summary is the report over a Search: the totals, per workspace, and per
// person (what each answered, in free text, skipped and corrected).
type Summary struct {
	Total       Counts            `json:"total"`
	ByWorkspace map[string]Counts `json:"by_workspace"`
	ByPerson    map[string]Counts `json:"by_person"`
}

// Summarize counts a Search's rows.
func Summarize(rows []Row) Summary {
	sum := Summary{ByWorkspace: map[string]Counts{}, ByPerson: map[string]Counts{}}
	waits := map[string][]int64{} // "ws:" / "person:" + key → the waits
	add := func(key string, m map[string]Counts, r Row, person bool, waitKey string) {
		c := m[key]
		count(&c, r, person)
		m[key] = c
		if r.decided() {
			waits[waitKey] = append(waits[waitKey], r.WaitedSeconds)
		}
	}
	var all []int64
	for _, r := range rows {
		count(&sum.Total, r, false)
		if r.decided() {
			all = append(all, r.WaitedSeconds)
		}
		add(r.WorkspaceID, sum.ByWorkspace, r, false, "ws:"+r.WorkspaceID)
		if r.AnsweredBy != "" {
			add(r.AnsweredBy, sum.ByPerson, r, true, "person:"+r.AnsweredBy)
		}
	}
	sum.Total.MedianWaitSeconds, sum.Total.Measured = median(all), len(all)
	for k, c := range sum.ByWorkspace {
		c.MedianWaitSeconds, c.Measured = median(waits["ws:"+k]), len(waits["ws:"+k])
		sum.ByWorkspace[k] = c
	}
	for k, c := range sum.ByPerson {
		c.MedianWaitSeconds, c.Measured = median(waits["person:"+k]), len(waits["person:"+k])
		sum.ByPerson[k] = c
	}
	return sum
}

// decided: a question that got an answer, a free-text answer or a skip,
// with a measured wait (live, not imported).
func (r Row) decided() bool {
	return r.waited && r.Kind == "question" && (r.State == StateAnswered || r.State == StateAnsweredFreeText || r.State == StateSkipped)
}

// count adds a row. Per person, asked/open/superseded don't apply: a person
// answers, answers in free text, skips and corrects.
func count(c *Counts, r Row, person bool) {
	if r.Kind == "free" {
		c.FreeAnswers++
		return
	}
	if !person {
		c.Asked++
	}
	switch r.State {
	case StateAnswered:
		c.Answered++
	case StateAnsweredFreeText:
		c.AnsweredFreeText++
	case StateSkipped:
		c.Skipped++
	case StateOpen:
		if !person {
			c.Open++
		}
	case StateSuperseded:
		if !person {
			c.Superseded++
		}
	}
	c.Corrections += r.Corrections
	if r.AfterUse {
		c.CorrectionsAfterUse++
	}
}

func median(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// CSVHeader and CSV are the decision log as comma-separated values (the
// report's export).
var CSVHeader = []string{"kind", "workspace_id", "item_id", "item_title", "asker", "label", "run_id", "subject_kind", "subject",
	"blocking", "question_id", "n", "question", "state", "asked_at", "option", "option_text", "answer", "answered_by", "via",
	"action", "approval_id", "answered_at", "used_by_run", "corrections", "after_use", "waited_seconds"}

func (r Row) CSV() []string {
	ts := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	b := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	itoa := func(n int64) string {
		if n == 0 {
			return ""
		}
		return strconv.FormatInt(n, 10)
	}
	out := []string{r.Kind, r.WorkspaceID, r.ItemID, r.ItemTitle, r.Asker, r.Label, r.RunID, r.SubjectKind, r.Subject,
		b(r.Blocking), r.QuestionID, itoa(int64(r.N)), r.Question, r.State, ts(r.AskedAt), r.Option, r.OptionText, r.Answer,
		r.AnsweredBy, r.Via, r.Action, r.ApprovalID, ts(r.AnsweredAt), r.UsedByRun, itoa(int64(r.Corrections)), b(r.AfterUse),
		itoa(r.WaitedSeconds)}
	for i, v := range out {
		out[i] = cell(v)
	}
	return out
}

// cell keeps a value from being read as a formula by a spreadsheet: text
// written by agents (questions, options) or by people (answers, titles)
// starting with =, +, -, @, tab or carriage return gets a leading
// apostrophe.
func cell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
