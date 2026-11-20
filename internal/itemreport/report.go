// Package itemreport builds one document per item: what was asked, what
// was decided, what was built and who checked it. It composes records the
// other packages keep (the item, its plans, runs, questions, findings and
// approvals) and stores nothing itself.
//
// GET /api/inbox/{id}/report serves it as Markdown, HTML or JSON, for a
// person to keep and for another system to fetch when an item closes.
package itemreport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/approvals"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/itemtext"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/runs"
	"github.com/martinsuchenak/skopos/internal/usage"
)

// Levels of the Markdown and HTML documents.
const (
	LevelSummary = "summary" // fits a comment
	LevelFull    = "full"    // everything, verbatim
)

// Items reads the item (scope-checked: a foreign item is not found).
type Items interface {
	ReadItem(ctx context.Context, id string) (*inbox.Item, error)
}

// Plans reads a plan revision (the approved one, for its steps).
type Plans interface {
	GetRevision(ctx context.Context, revisionID string) (*plans.Revision, error)
}

// Runs lists the item's runs, newest first.
type Runs interface {
	List(ctx context.Context, status runs.Status, itemID string) ([]runs.Run, error)
}

// RunDetail reads one run with what it observed (runs.Service.GetFull):
// the work list a finished run reported. A Runs without it leaves the
// lists out.
type RunDetail interface {
	GetFull(ctx context.Context, id string) (*runs.Run, error)
}

// Questions reads the item's question-and-answer record.
type Questions interface {
	Record(ctx context.Context, itemID string) (*qa.Record, error)
}

// Findings is the record of review findings.
type Findings interface {
	Item(ctx context.Context, itemID string) ([]findings.Finding, error)
}

// Approvals lists the approvals recorded at the item's gates: they name
// what was approved (a plan revision and its base, a head commit).
type Approvals interface {
	List(ctx context.Context, itemID string) ([]approvals.Entry, error)
}

// Audit reads the item's audit entries, newest first, a page at a time:
// every action a person took on the item is one of them.
type Audit interface {
	List(ctx context.Context, f audit.ListFilter) ([]audit.Entry, error)
}

// Service builds the report. Items is required; a nil reader leaves its
// part out (a server without that feature wired).
type Service struct {
	Items     Items
	Plans     Plans
	Runs      Runs
	Questions Questions
	Findings  Findings
	Approvals Approvals
	Audit     Audit
	// Now is the report's time (time.Now when nil).
	Now func() time.Time
}

// Report is the item's record as one structure. Lists are oldest first.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Item        Item      `json:"item"`
	// Outcome: where the item stands, put together from the records.
	Outcome     []string      `json:"outcome"`
	Brief       string        `json:"brief"`
	Plan        *Plan         `json:"plan,omitempty"`
	Questions   []QuestionSet `json:"questions"`
	FreeAnswers []Answer      `json:"free_answers,omitempty"`
	Decisions   []Decision    `json:"decisions"`
	// DecisionsCut: the item has more decisions than the report reads;
	// the oldest are left out.
	DecisionsCut    bool             `json:"decisions_cut,omitempty"`
	Implementations []Implementation `json:"implementations"`
	Reviews         []Review         `json:"reviews"`
	Findings        []Finding        `json:"findings"`
	// Sections: what the reviewers, QA and the implementer's amendments
	// wrote on the item, as written.
	Sections []Section `json:"sections,omitempty"`
	Runs     []Run     `json:"runs"`
	// CostUSD: the sum of what the runners reported (nil when none did).
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// Usage: the sum of what the runners reported the agents used (nil
	// when none did).
	Usage *runs.Usage `json:"usage,omitempty"`
}

// Item is the report's header.
type Item struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Title       string `json:"title"`
	Ref         string `json:"ref,omitempty"`
	ExternalURL string `json:"external_url,omitempty"`
	Status      string `json:"status"`
	Flow        string `json:"flow,omitempty"`
	Branch      string `json:"branch,omitempty"`
	HeadSHA     string `json:"head_sha,omitempty"`
	PullRequest string `json:"pull_request,omitempty"`
	// Merge: what became of the branch after sign-off, as the worker
	// reported it (nil = nothing reported).
	Merge     *inbox.BranchOutcome `json:"merge,omitempty"`
	Needs     []string             `json:"needs,omitempty"`
	CreatedAt time.Time            `json:"created_at"`
	UpdatedAt time.Time            `json:"updated_at"`
}

// Plan is the approved plan and the others written for the item.
type Plan struct {
	// Approved is the plan the approval names; nil while none is approved.
	Approved   *PlanDoc  `json:"approved,omitempty"`
	Approval   *Decision `json:"approval,omitempty"`
	RevisionID string    `json:"revision_id,omitempty"`
	RevisionNo int       `json:"revision_no,omitempty"`
	BaseSHA    string    `json:"base_sha,omitempty"`
	// Amended: the approval in force names a later revision than the
	// approved plan's text — an amendment the person approved. Steps are
	// that revision's.
	Amended bool `json:"amended,omitempty"`
	// Steps: the step titles of the revision in force, in order.
	Steps  []string `json:"steps,omitempty"`
	Rounds int      `json:"rounds"`
	// Others: every plan that was not approved, of every round.
	Others []PlanDoc  `json:"others,omitempty"`
	Notes  []PlanNote `json:"notes,omitempty"`
}

// PlanDoc is one plan as its planner wrote it.
type PlanDoc struct {
	Round      int    `json:"round"`
	Title      string `json:"title"`
	RevisionID string `json:"revision_id,omitempty"`
	Markdown   string `json:"markdown"`
}

// PlanNote is a comparison or a round's summary.
type PlanNote struct {
	Round    int    `json:"round"`
	Heading  string `json:"heading"`
	Markdown string `json:"markdown"`
}

// QuestionSet is the questions one agent asked at one time.
type QuestionSet struct {
	// Asked: "the planner", a plan candidate's label, "the implementer", "QA".
	Asked     string     `json:"asked"`
	Stage     string     `json:"stage,omitempty"`
	At        time.Time  `json:"at"`
	Blocking  bool       `json:"blocking"`
	Questions []Question `json:"questions"`
	// General: text given to the set as a whole.
	General []Answer `json:"general,omitempty"`
}

// Question is one question and what was answered.
type Question struct {
	N       int      `json:"n"`
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
	// State: open, answered, answered_free_text, skipped, superseded.
	State string `json:"state"`
	// Answer is the one in force; Replaced the ones it corrected.
	Answer   *Answer  `json:"answer,omitempty"`
	Replaced []Answer `json:"replaced,omitempty"`
}

// Answer is what a person said.
type Answer struct {
	Text string    `json:"text"`
	By   string    `json:"by"`
	Via  string    `json:"via,omitempty"`
	At   time.Time `json:"at"`
}

// Actions a person takes on an item, as the report names them.
const (
	ActionQueue         = "queue"           // started the pipeline
	ActionReview        = "review"          // sent a branch for review only
	ActionApprove       = "approve"         // approved a plan
	ActionRequestChange = "request_changes" // sent a plan or a branch back
	ActionRetry         = "retry"           // started a blocked or failed item again
	ActionReject        = "reject"          // discarded the item
	ActionMarkDone      = "mark_done"       // signed the branch off
	ActionSecondOpinion = "second_opinion"  // asked another runner for an opinion
	ActionWaive         = "finding_waive"   // accepted a finding as it is
	ActionAnswer        = "answer"          // answered questions without moving the item
	ActionCheckCommits  = "check_commits"   // had commits added after review and QA checked
	// ActionAutoHeld: the item asked for its plan to be approved
	// automatically and something kept it for the person (the notes say
	// what). Recorded by the server, not a person's action.
	ActionAutoHeld = "auto_approve_held"
	// ActionNothingToDo: the server closed the item because the planner
	// found nothing to do (the notes say why).
	ActionNothingToDo = "nothing_to_do"
)

// Decision is something a person decided on the item.
type Decision struct {
	At time.Time `json:"at"`
	// Action: one of the Action constants.
	Action string `json:"action"`
	// Subject: what an approval named — a plan revision and its base
	// commit ("<revision>+<base>") or the head commit signed off.
	Subject string `json:"subject,omitempty"`
	By      string `json:"by"`
	Via     string `json:"via,omitempty"`
	Notes   string `json:"notes,omitempty"`
	// Reason: why the approver chose this plan (a plan approval only).
	Reason string `json:"reason,omitempty"`
}

// Check is one setup or check command's result.
type Check struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Stopped bool   `json:"stopped,omitempty"`
	// Output: the end of a failed command's output.
	Output string `json:"output,omitempty"`
}

// Implementation is one finished implement run.
type Implementation struct {
	RunID      string    `json:"run_id"`
	Runner     string    `json:"runner,omitempty"`
	Status     string    `json:"status"`
	At         time.Time `json:"at"`
	HeadSHA    string    `json:"head_sha,omitempty"`
	Diffstat   string    `json:"diffstat,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	FailReason string    `json:"fail_reason,omitempty"`
	Setup      []Check   `json:"setup,omitempty"`
	Checks     []Check   `json:"checks,omitempty"`
	FixRounds  int       `json:"fix_rounds,omitempty"`
	Pushed     *bool     `json:"pushed,omitempty"`
	PRURL      string    `json:"pr_url,omitempty"`
	// Work: what the implementer says it verified, and what came of each.
	Work []runs.WorkEntry `json:"work,omitempty"`
	// AdoptFrom: the run took in commits added to the branch after this
	// one (Adopted: those commits); no agent implemented anything.
	AdoptFrom string               `json:"adopt_from,omitempty"`
	Adopted   []runs.AdoptedCommit `json:"adopted,omitempty"`
}

// Review is one finished review, QA run or second opinion.
type Review struct {
	RunID   string    `json:"run_id"`
	Stage   string    `json:"stage"`
	Runner  string    `json:"runner,omitempty"`
	Focus   string    `json:"focus,omitempty"`
	At      time.Time `json:"at"`
	HeadSHA string    `json:"head_sha,omitempty"`
	Status  string    `json:"status"`
	// Passed: it succeeded with no blocking finding.
	Passed bool `json:"passed"`
	// Advisory: a second opinion, which never decides.
	Advisory   bool         `json:"advisory,omitempty"`
	Blocking   int          `json:"blocking"`
	StillOpen  int          `json:"still_open,omitempty"`
	Nits       int          `json:"nits"`
	Summary    string       `json:"summary,omitempty"`
	FailReason string       `json:"fail_reason,omitempty"`
	Findings   []RunFinding `json:"findings,omitempty"`
	// Work: what the reviewer checked, or the scenarios QA ran.
	Work []runs.WorkEntry `json:"work,omitempty"`
}

// RunFinding is a finding as its run reported it.
type RunFinding struct {
	ID       string `json:"id,omitempty"`
	Severity string `json:"severity"`
	Text     string `json:"text"`
	File     string `json:"file,omitempty"`
}

// Finding is a finding and what became of it.
type Finding struct {
	N        int    `json:"n"`
	ID       string `json:"id"`
	Severity string `json:"severity"`
	By       string `json:"by"`
	Text     string `json:"text"`
	File     string `json:"file,omitempty"`
	HeadSHA  string `json:"head_sha,omitempty"`
	// State: open, fixed, addressed, still_open, waived, noted.
	State string `json:"state"`
	// Outcome says the state in words, with who waived it and why.
	Outcome string `json:"outcome"`
	// Earlier: from a run of before findings were recorded; what became
	// of it is not known.
	Earlier bool `json:"earlier,omitempty"`
}

// Section is a section of the item as it was written.
type Section struct {
	// Kind: review, qa, second_opinion, amendment.
	Kind     string `json:"kind"`
	Heading  string `json:"heading"`
	Markdown string `json:"markdown"`
}

// Run is one run of the item: who worked on it and where the code went.
type Run struct {
	ID       string `json:"id"`
	Stage    string `json:"stage"`
	Focus    string `json:"focus,omitempty"`
	Runner   string `json:"runner,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Executor: what contained the run.
	Executor string `json:"executor,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
	Status   string `json:"status"`
	// QueuedAt and UpdatedAt: a run records no start time, so the time
	// between them includes the wait for a worker.
	QueuedAt  time.Time `json:"queued_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CostUSD   *float64  `json:"cost_usd,omitempty"`
	// Usage: what the run's agents used, as the runner reported it.
	Usage *runs.Usage `json:"usage,omitempty"`
	// ModelsUsed: the models the runner said did the work, which may
	// differ from Model (the configured one, empty when the account names
	// none and the CLI picks its own).
	ModelsUsed []string `json:"models_used,omitempty"`
}

// Build reads the item's records and puts the report together.
func (s *Service) Build(ctx context.Context, itemID string) (*Report, error) {
	item, err := s.Items.ReadItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	brief, secs := itemtext.Split(item.Content)
	r := &Report{
		GeneratedAt: now().UTC(),
		Item: Item{
			ID: item.ID, WorkspaceID: item.WorkspaceID, Title: strings.TrimSpace(item.Title), Ref: item.Ref, ExternalURL: item.ExternalURL,
			Status: string(item.Status), Flow: item.Flow, Branch: item.Branch, Needs: item.Needs, Merge: item.Outcome,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		},
		Brief:           strings.TrimSpace(brief),
		Outcome:         []string{},
		Questions:       []QuestionSet{},
		Decisions:       []Decision{},
		Implementations: []Implementation{},
		Reviews:         []Review{},
		Findings:        []Finding{},
		Runs:            []Run{},
	}

	var approved []approvals.Entry
	if s.Approvals != nil {
		if approved, err = s.Approvals.List(ctx, item.ID); err != nil {
			return nil, err
		}
	}
	timeline, cut, err := s.timeline(ctx, item)
	if err != nil {
		return nil, err
	}
	r.Decisions, r.DecisionsCut = decisions(approved, timeline), cut
	if r.Plan, err = s.plan(ctx, item, r.Decisions); err != nil {
		return nil, err
	}
	if s.Questions != nil {
		rec, err := s.Questions.Record(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		r.Questions, r.FreeAnswers = questions(rec)
	}
	if s.Runs != nil {
		all, err := s.Runs.List(ctx, "", item.ID)
		if err != nil {
			return nil, err
		}
		// Oldest first: the order things happened. Run ids are UUIDv7,
		// so they break a tie in the creation time.
		sort.SliceStable(all, func(i, j int) bool {
			if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
				return all[i].CreatedAt.Before(all[j].CreatedAt)
			}
			return all[i].ID < all[j].ID
		})
		if d, ok := s.Runs.(RunDetail); ok {
			for i, run := range all {
				if run.Observed != nil || !finished(run.Status) || (run.Stage != runs.StageImplement && !reviewStage(run.Stage)) {
					continue
				}
				full, err := d.GetFull(ctx, run.ID)
				if errors.Is(err, runs.ErrNotFound) {
					continue // removed since the list was read: no list for it
				}
				if err != nil {
					return nil, err
				}
				all[i].Observed = full.Observed
			}
		}
		r.fromRuns(all)
	}
	if s.Findings != nil {
		fs, err := s.Findings.Item(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		r.Findings = findingRows(fs)
		// The audit notes name findings by id (a waiver, the sign-off):
		// the report names them by their number in its own table.
		for i := range r.Decisions {
			for _, f := range r.Findings {
				if f.ID != "" {
					r.Decisions[i].Notes = strings.ReplaceAll(r.Decisions[i].Notes, f.ID, fmt.Sprintf("finding %d", f.N))
				}
			}
		}
	}
	r.Sections = sections(secs)
	r.Outcome = outcome(r)
	return r, nil
}

// auditPage is how many audit entries one read returns at most (the audit
// list's own limit); maxAuditPages bounds what one report reads.
const (
	auditPage     = 200
	maxAuditPages = 10
)

// timeline reads the audit entries of a person's actions on the item,
// oldest first. cut: there were more than the report reads, and the oldest
// are missing.
func (s *Service) timeline(ctx context.Context, item *inbox.Item) (all []audit.Entry, cut bool, err error) {
	if s.Audit == nil {
		return nil, false, nil
	}
	actions := make([]string, 0, len(personActions))
	for a := range personActions {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	before := ""
	for n := 0; ; n++ {
		if n == maxAuditPages {
			cut = true
			break
		}
		page, err := s.Audit.List(ctx, audit.ListFilter{WorkspaceID: item.WorkspaceID, EntityType: "inbox_item", EntityID: item.ID,
			Actions: actions, Before: before, Limit: auditPage})
		if err != nil {
			return nil, false, err
		}
		all = append(all, page...)
		if len(page) < auditPage {
			break
		}
		before = page[len(page)-1].ID
	}
	// Ids are UUIDv7: their order is the order things happened.
	sort.SliceStable(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all, cut, nil
}

// personActions maps the audit actions a person takes to the report's.
// Edits, links and the pipeline's own transitions are not decisions, and
// the findings sent back with a change request are in that request's notes.
var personActions = map[string]string{
	"inbox.queue": ActionQueue, "inbox.review": ActionReview, "inbox.approve": ActionApprove,
	"inbox.request_changes": ActionRequestChange, "inbox.retry": ActionRetry, "inbox.reject": ActionReject,
	"inbox.mark_done": ActionMarkDone, "inbox.second_opinion": ActionSecondOpinion,
	"inbox.finding_waive": ActionWaive, "inbox.answer": ActionAnswer, "inbox.check_commits": ActionCheckCommits,
	"inbox.auto_approve_held": ActionAutoHeld, "inbox.nothing_to_do": ActionNothingToDo,
}

// subjectWindow is how far apart an action's audit entry and its approval
// record may be written: both come from one request.
const subjectWindow = time.Minute

// decisions are what a person decided, oldest first: the audit entries of
// a person's actions. An approval and a sign-off take the subject (the
// revision and base, the commit) from the approval record written with
// them — the one nearest in time, never by position: an approval is
// recorded before the status changes, so one whose status change failed
// has a record and no audit entry. Without the audit log the approvals
// alone are listed.
func decisions(approved []approvals.Entry, timeline []audit.Entry) []Decision {
	subjects := map[string][]approvals.Entry{}
	for _, e := range approved {
		if e.Decision != approvals.DecisionApproved {
			continue
		}
		switch e.Gate {
		case approvals.GatePlan:
			subjects[ActionApprove] = append(subjects[ActionApprove], e)
		case approvals.GateReview:
			subjects[ActionMarkDone] = append(subjects[ActionMarkDone], e)
		}
	}
	out := []Decision{}
	if len(timeline) == 0 {
		for _, action := range []string{ActionApprove, ActionMarkDone} {
			for _, e := range subjects[action] {
				out = append(out, Decision{At: e.CreatedAt, Action: action, Subject: e.Subject, By: e.Actor, Via: e.Via, Notes: strings.TrimSpace(e.Notes), Reason: e.Reason})
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
		return out
	}
	used := map[string][]bool{}
	for action, list := range subjects {
		used[action] = make([]bool, len(list))
	}
	for _, e := range timeline {
		action, ok := personActions[e.Action]
		if !ok {
			continue
		}
		d := Decision{At: e.CreatedAt, Action: action, By: e.Actor, Via: e.Via, Notes: strings.TrimSpace(e.Notes)}
		best, gap := -1, subjectWindow
		for i, a := range subjects[action] {
			if g := a.CreatedAt.Sub(e.CreatedAt).Abs(); !used[action][i] && g <= gap {
				best, gap = i, g
			}
		}
		if best >= 0 {
			d.Subject, d.Reason = subjects[action][best].Subject, subjects[action][best].Reason
			used[action][best] = true
			// The audit note repeats the reason after the notes: the
			// report shows it once, as the reason.
			if d.Reason != "" {
				d.Notes = strings.TrimSpace(strings.TrimSuffix(d.Notes, "Why this plan: "+d.Reason))
			}
		}
		out = append(out, d)
	}
	return out
}

// plan reads the item's planning rounds and picks the plan the newest plan
// approval names. Nil for an item with no plan and no plan approval.
func (s *Service) plan(ctx context.Context, item *inbox.Item, ds []Decision) (*Plan, error) {
	p := &Plan{}
	newest, err := inbox.PlansOf(item.ID, item.Title, item.Content, 0)
	switch {
	case err == nil:
		p.Rounds = newest.Rounds
	case !errors.Is(err, inbox.ErrNotFound):
		return nil, err
	}
	var docs []PlanDoc
	for n := 1; n <= p.Rounds; n++ {
		v := newest
		if n != p.Rounds {
			if v, err = inbox.PlansOf(item.ID, item.Title, item.Content, n); err != nil {
				return nil, err
			}
		}
		for _, d := range v.Plans {
			docs = append(docs, PlanDoc{Round: n, Title: d.Title, RevisionID: d.RevisionID, Markdown: strings.TrimSpace(d.Markdown)})
		}
		for _, note := range []*inbox.PlanNote{v.Comparison, v.Summary} {
			if note != nil {
				p.Notes = append(p.Notes, PlanNote{Round: n, Heading: note.Heading, Markdown: strings.TrimSpace(note.Markdown)})
			}
		}
	}

	// The approval in force is the newest. The approved plan is the one a
	// plan approval names, looking from the newest back: an approved
	// amendment names a revision no plan section carries, and the plan it
	// amends is the one approved before it.
	approved := -1
	for i := len(ds) - 1; i >= 0; i-- {
		if ds[i].Action != ActionApprove {
			continue
		}
		rev, base, _ := strings.Cut(ds[i].Subject, "+")
		if p.Approval == nil {
			d := ds[i]
			p.Approval, p.RevisionID, p.BaseSHA = &d, rev, base
		}
		for j := len(docs) - 1; j >= 0 && rev != ""; j-- {
			if docs[j].RevisionID == rev {
				approved = j
				break
			}
		}
		if approved >= 0 {
			break
		}
	}
	// No approval names a plan section (a plan that does not say its
	// revision): the newest round's plan, when that round has exactly one.
	if p.Approval != nil && approved < 0 {
		only := -1
		for j, doc := range docs {
			if doc.Round != p.Rounds {
				continue
			}
			if only >= 0 {
				only = -1
				break
			}
			only = j
		}
		approved = only
	}
	if approved >= 0 && docs[approved].RevisionID != "" && docs[approved].RevisionID != p.RevisionID {
		p.Amended = true
	}
	if p.Approval == nil && len(docs) == 0 {
		return nil, nil
	}
	for i := range docs {
		if i == approved {
			d := docs[i]
			p.Approved = &d
			continue
		}
		p.Others = append(p.Others, docs[i])
	}
	if p.RevisionID != "" && s.Plans != nil {
		rev, err := s.Plans.GetRevision(ctx, p.RevisionID)
		switch {
		case err == nil:
			p.RevisionNo = rev.RevisionNo
			for _, st := range rev.Steps {
				p.Steps = append(p.Steps, strings.TrimSpace(st.Title))
			}
		case errors.Is(err, plans.ErrNotFound), errors.Is(err, auth.ErrOutOfScope):
			// The plan is gone (a finished plan is cleaned up): the
			// plan's text on the item is still the record.
		default:
			return nil, err
		}
	}
	return p, nil
}

// questions turns the record into the report's sets, oldest first.
func questions(rec *qa.Record) ([]QuestionSet, []Answer) {
	out := []QuestionSet{}
	if rec == nil {
		return out, nil
	}
	sets := append([]qa.Set(nil), rec.Sets...)
	sort.SliceStable(sets, func(i, j int) bool { return sets[i].CreatedAt.Before(sets[j].CreatedAt) })
	for _, set := range sets {
		qs := QuestionSet{Asked: set.Asked(), Stage: set.Stage, At: set.CreatedAt, Blocking: set.Blocking, Questions: []Question{}}
		if qs.Asked == "" {
			qs.Asked = "the planner"
		}
		for _, q := range set.Questions {
			row := Question{N: q.N, Text: strings.TrimSpace(q.Text), Options: q.Options, State: q.State}
			for _, a := range q.Answers {
				if a.Kind != qa.KindAnswer {
					continue
				}
				if a.Replaced {
					row.Replaced = append(row.Replaced, answer(a))
					continue
				}
				ans := answer(a)
				row.Answer = &ans
			}
			qs.Questions = append(qs.Questions, row)
		}
		for _, a := range set.General {
			qs.General = append(qs.General, answer(a))
		}
		out = append(out, qs)
	}
	var free []Answer
	for _, a := range rec.Free {
		free = append(free, answer(a))
	}
	return out, free
}

// answer renders what was said: the option chosen, then the words.
func answer(a qa.Answer) Answer {
	text := strings.TrimSpace(a.Text)
	if opt := strings.TrimSpace(a.Option); opt != "" {
		if text == "" || text == opt {
			text = opt
		} else {
			text = opt + " — " + text
		}
	}
	return Answer{Text: text, By: a.Actor, Via: a.Via, At: a.CreatedAt}
}

// reviewStage: the stages whose runs give a verdict on a head.
func reviewStage(stage string) bool {
	switch stage {
	case runs.StageReview, runs.StageBranchReview, runs.StageQA, runs.StageSecondOpinion:
		return true
	}
	return false
}

func finished(s runs.Status) bool { return s == runs.StatusSucceeded || s == runs.StatusFailed }

// adopted is the commits an adopt run took in.
func adopted(run runs.Run) []runs.AdoptedCommit {
	if run.Observed == nil {
		return nil
	}
	return run.Observed.Adopted
}

// work is the list a run reported of what it did.
func work(run runs.Run) []runs.WorkEntry {
	if run.Observed == nil {
		return nil
	}
	return run.Observed.Work
}

// fromRuns fills the implementations, the reviews, the run table, the cost
// and the header's branch, head and pull request (all oldest first).
func (r *Report) fromRuns(all []runs.Run) {
	var cost float64
	costed, implemented := false, false
	var used []*runs.Usage
	for _, run := range all {
		row := Run{ID: run.ID, Stage: run.Stage, Focus: run.Focus, Runner: run.Runner, Executor: run.Executor,
			WorkerID: run.WorkerID, Status: string(run.Status), QueuedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, CostUSD: run.CostUSD,
			Usage: run.Usage}
		if run.Usage != nil {
			row.ModelsUsed = run.Usage.ModelNames()
			used = append(used, run.Usage)
		}
		// What the worker reported about the runner when it claimed the
		// run: the model, and the provider the code went to.
		var facts struct {
			Kind     string `json:"kind"`
			Model    string `json:"model"`
			Provider string `json:"provider"`
			Executor string `json:"executor"`
		}
		if len(run.Facts) > 0 && json.Unmarshal(run.Facts, &facts) == nil {
			row.Kind, row.Model, row.Provider = facts.Kind, facts.Model, facts.Provider
			if row.Executor == "" {
				row.Executor = facts.Executor
			}
		}
		r.Runs = append(r.Runs, row)
		if run.CostUSD != nil {
			cost += *run.CostUSD
			costed = true
		}

		switch {
		case run.Stage == runs.StageImplement && finished(run.Status):
			implemented = true
			r.Implementations = append(r.Implementations, Implementation{
				RunID: run.ID, Runner: run.Runner, Status: string(run.Status), At: run.UpdatedAt, HeadSHA: run.HeadSHA,
				Diffstat: strings.TrimSpace(run.Diffstat), Summary: strings.TrimSpace(run.ResultSummary), FailReason: strings.TrimSpace(run.FailReason),
				Setup: checks(run.Setup), Checks: checks(run.Checks), FixRounds: run.FixRounds, Pushed: run.Pushed, PRURL: run.PRURL,
				Work: work(run), AdoptFrom: run.AdoptFrom, Adopted: adopted(run),
			})
			if run.Branch != "" {
				r.Item.Branch = run.Branch
			}
			if run.HeadSHA != "" {
				r.Item.HeadSHA = run.HeadSHA
			}
			if run.PRURL != "" {
				r.Item.PullRequest = run.PRURL
			}
		case reviewStage(run.Stage) && finished(run.Status):
			rv := Review{
				RunID: run.ID, Stage: run.Stage, Runner: run.Runner, Focus: run.Focus, At: run.UpdatedAt, HeadSHA: run.HeadSHA,
				Status: string(run.Status), Passed: runs.ReviewPassed(run), Advisory: run.Stage == runs.StageSecondOpinion,
				Blocking: runs.BlockingFindings(run), StillOpen: run.StillOpen,
				Summary: strings.TrimSpace(run.ResultSummary), FailReason: strings.TrimSpace(run.FailReason),
				Work: work(run),
			}
			for _, f := range run.Findings {
				if f.Severity == runs.SeverityNit {
					rv.Nits++
				}
				rv.Findings = append(rv.Findings, RunFinding{ID: f.ID, Severity: f.Severity, Text: strings.TrimSpace(f.Text), File: f.File})
			}
			r.Reviews = append(r.Reviews, rv)
			// A review-only item has no implement run: its branch and
			// head are the newest reviewed ones.
			if !implemented && run.HeadSHA != "" && run.Stage == runs.StageBranchReview {
				r.Item.HeadSHA = run.HeadSHA
				if run.Branch != "" {
					r.Item.Branch = run.Branch
				}
			}
		}
	}
	if costed {
		r.CostUSD = &cost
	}
	r.Usage = usage.Sum(used...)
}

// maxCheckLines is how much of a failed command's output the report keeps.
const maxCheckLines = 40

func checks(in []runs.Check) []Check {
	out := make([]Check, 0, len(in))
	for _, c := range in {
		row := Check{Name: c.Name, Passed: c.Passed, Stopped: c.Stopped}
		if !c.Passed {
			row.Output = tail(c.Output, maxCheckLines)
		}
		out = append(out, row)
	}
	return out
}

// tail is the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// findingRows numbers the findings in the order they were raised and says
// what became of each.
func findingRows(fs []findings.Finding) []Finding {
	sorted := append([]findings.Finding(nil), fs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].N < sorted[j].N
	})
	out := make([]Finding, 0, len(sorted))
	for i, f := range sorted {
		out = append(out, Finding{N: i + 1, ID: f.ID, Severity: f.Severity, By: f.By(), Text: strings.TrimSpace(f.Text), File: f.File,
			HeadSHA: f.HeadSHA, State: f.State, Outcome: findingOutcome(f), Earlier: f.Earlier()})
	}
	return out
}

func findingOutcome(f findings.Finding) string {
	if f.Earlier() {
		return "from an earlier run; not followed"
	}
	switch f.State {
	case findings.StateWaived:
		for i := len(f.Updates) - 1; i >= 0; i-- {
			if u := f.Updates[i]; u.Kind == findings.KindWaived {
				out := "waived"
				if u.Actor != "" {
					out += " by " + u.Actor
				}
				if note := strings.TrimSpace(u.Note); note != "" {
					out += ": " + note
				}
				return out
			}
		}
		return "waived"
	case findings.StateAddressed:
		return "fixed, confirmed by a review"
	case findings.StateFixed:
		return "fixed according to the implementer; no review confirmed it"
	case findings.StateStillOpen:
		return "still open: a review found it again"
	case findings.StateOpen:
		return "to fix"
	case findings.StateNoted:
		return "noted; not followed up"
	}
	return f.State
}

// sections keeps what reviewers, QA, second opinions and the implementer's
// amendments wrote on the item, with the approver's notes that answered
// them. Plans are in the plan part and questions come from their record.
func sections(secs []itemtext.Section) []Section {
	var out []Section
	for _, round := range itemtext.Rounds(secs) {
		switch round.Kind {
		case "review", "qa", "second_opinion", "amendment":
		default:
			continue
		}
		for _, s := range round.Sections {
			if itemtext.HeadingIs(s, "Answers") {
				continue
			}
			out = append(out, Section{Kind: round.Kind, Heading: s.Heading, Markdown: strings.TrimSpace(s.Body)})
		}
	}
	return out
}

// outcome says where the item stands, from the records alone.
func outcome(r *Report) []string {
	var out []string
	signOff := r.signOff()
	switch {
	case r.Item.Status == string(inbox.StatusDone) && signOff != nil:
		out = append(out, fmt.Sprintf("Signed off by %s on %s%s.", signOff.By, day(signOff.At), through(signOff.Via)))
	case r.Item.Status == string(inbox.StatusDone) && r.closedNothingToDo() != nil:
		out = append(out, "Closed without changes: the planner found nothing to do ("+oneLineOf(r.closedNothingToDo().Notes)+").")
	case r.Item.Status == string(inbox.StatusDone):
		out = append(out, "Done.")
	case r.Item.Status == string(inbox.StatusDiscarded):
		out = append(out, "Discarded.")
	default:
		out = append(out, "Not finished: the item is "+strings.ReplaceAll(r.Item.Status, "_", " ")+".")
	}
	if r.Item.HeadSHA != "" {
		line := "The work is at commit " + short(r.Item.HeadSHA)
		if r.Item.Branch != "" {
			line += " on branch " + r.Item.Branch
		}
		out = append(out, line+".")
	}
	if line := mergeLine(r.Item.Merge); line != "" {
		out = append(out, line)
	}
	if line := findingCounts(r.Findings); line != "" {
		out = append(out, line)
	}
	asked, answered, skipped, open := 0, 0, 0, 0
	for _, set := range r.Questions {
		for _, q := range set.Questions {
			switch q.State {
			case qa.StateSuperseded:
			case qa.StateAnswered, qa.StateAnsweredFreeText:
				asked, answered = asked+1, answered+1
			case qa.StateSkipped:
				asked, skipped = asked+1, skipped+1
			default:
				asked, open = asked+1, open+1
			}
		}
	}
	if asked > 0 {
		parts := []string{fmt.Sprintf("%d answered", answered)}
		if skipped > 0 {
			parts = append(parts, fmt.Sprintf("%d passed over", skipped))
		}
		if open > 0 {
			parts = append(parts, fmt.Sprintf("%d open", open))
		}
		out = append(out, fmt.Sprintf("%s asked: %s.", plural(asked, "question"), strings.Join(parts, ", ")))
	}
	return out
}

// mergeLine says what became of the branch's pull request after sign-off.
func mergeLine(o *inbox.BranchOutcome) string {
	if o == nil {
		return ""
	}
	switch o.State {
	case inbox.OutcomeMerged:
		line := "The pull request was merged"
		if o.MergedAt != nil {
			line += " on " + day(*o.MergedAt)
		}
		if o.MergeSHA != "" {
			line += " (merge commit " + short(o.MergeSHA) + ")"
		}
		return line + "."
	case inbox.OutcomeClosed:
		line := "The pull request was closed without merging"
		if o.ClosedAt != nil {
			line += " on " + day(*o.ClosedAt)
		}
		return line + "."
	case inbox.OutcomeOpen:
		return "The pull request was still open 30 days after sign-off."
	}
	return ""
}

// closedNothingToDo is the server's closing of the item for nothing to do.
func (r *Report) closedNothingToDo() *Decision {
	for i := len(r.Decisions) - 1; i >= 0; i-- {
		if d := r.Decisions[i]; d.Action == ActionNothingToDo {
			return &d
		}
	}
	return nil
}

func oneLineOf(s string) string { return strings.Join(strings.Fields(s), " ") }

// signOff is the newest sign-off, nil when there is none.
func (r *Report) signOff() *Decision {
	for i := len(r.Decisions) - 1; i >= 0; i-- {
		if d := r.Decisions[i]; d.Action == ActionMarkDone {
			return &d
		}
	}
	return nil
}

// findingCounts: "5 findings: 3 fixed and confirmed, 1 waived, 1 still open."
func findingCounts(fs []Finding) string {
	counts := map[string]int{}
	total := 0
	for _, f := range fs {
		if f.Earlier {
			continue
		}
		counts[f.State]++
		total++
	}
	if total == 0 {
		return ""
	}
	var parts []string
	for _, st := range []struct{ state, word string }{
		{findings.StateAddressed, "fixed and confirmed"}, {findings.StateFixed, "fixed, not confirmed"},
		{findings.StateWaived, "waived"}, {findings.StateStillOpen, "still open"}, {findings.StateOpen, "to fix"},
		{findings.StateNoted, "noted, not followed up"},
	} {
		if n := counts[st.state]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st.word))
		}
	}
	return fmt.Sprintf("%s: %s.", plural(total, "finding"), strings.Join(parts, ", "))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func through(via string) string {
	if via == "" {
		return ""
	}
	return " (" + via + ")"
}
