package findings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/ids"
)

// Service is the findings record.
type Service struct {
	st    *Storage
	now   func() time.Time
	newID func() string
}

func NewService(st *Storage) *Service {
	return &Service{st: st, now: time.Now, newID: ids.New}
}

// Run is what the record needs to know about a run.
type Run struct {
	ID          string
	WorkspaceID string
	ItemID      string
	Stage       string
	Runner      string
	Focus       string
	HeadSHA     string
	// Listed: how many earlier findings the run's prompt listed for it to
	// answer, as its worker reports it. nil = the worker says nothing (a
	// release from before it listed them by name).
	Listed *int
}

// Member is one reviewer of a round: its run, and what it reviews as.
type Member struct {
	RunID string
	Stage string
	Focus string
}

// Round is the reviewers that run alongside a review run, in the order
// they were queued: a panel's members, or (no members) the run alone.
type Round struct {
	Members []Member
}

// members is the round's members, the run alone when it names none.
func (r Round) members(run Run) []Member {
	if len(r.Members) == 0 {
		return []Member{{RunID: run.ID, Stage: run.Stage, Focus: run.Focus}}
	}
	return r.Members
}

// inRound reports whether a run is one of the round's members: what a
// sibling found in this same round is not an earlier finding.
func inRound(members []Member, runID string) bool {
	for _, m := range members {
		if m.RunID == runID {
			return true
		}
	}
	return false
}

// checker is the one member of a round that checks a finding again, "" when
// none does. A finding is checked by one reviewer, so two members never
// answer for it: the first member that reviews as its reviewer did (the
// same kind of stage and the same focus), else the first member of that
// kind of stage — which takes a second opinion's finding the approver sent
// back, and one whose focus is no longer in the panel.
func checker(members []Member, f Finding) string {
	if Gating(f.Stage) {
		for _, m := range members {
			if family(m.Stage) == family(f.Stage) && m.Focus == f.Focus {
				return m.RunID
			}
		}
	}
	for _, m := range members {
		if Gating(m.Stage) && family(m.Stage) == family(f.Stage) {
			return m.RunID
		}
	}
	return ""
}

// ownSecondOpinion: the finding is an earlier one of the same runner's
// second opinions, which that runner's next second opinion answers for.
func ownSecondOpinion(run Run, f Finding) bool {
	return run.Stage == stageSecondOpinion && f.Stage == stageSecondOpinion && f.Runner == run.Runner &&
		f.Severity == SeverityBlocking && f.State != StateWaived && f.State != StateAddressed
}

// NewFinding is a finding as a run reports it. ID is set by the caller so
// the run's own copy of its findings carries the same ids.
type NewFinding struct {
	ID       string
	Severity string
	Text     string
	File     string
}

// Answer is what a run says about an earlier finding: an implement run's
// "fixed" or "not_fixed", a review's "addressed" or "still_open".
type Answer struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// RecordRun writes what a run reported, inside the report's transaction: a
// review's new findings and a run's answers to earlier ones. It returns how
// many blocking findings the run found still open.
//
// An answer the record cannot use is left out, never an error: an id the
// item does not have, a status the stage may not give, a finding that is
// settled or another reviewer's to check. What an agent writes must not
// fail its run's report.
//
// passed: the run is a review that found nothing blocking. A review that
// gates the item and passes settles what was its to check and it did not
// answer: it read the change and did not raise it again. Without that, a
// finding fixed rounds ago would go back to an implementer because a
// worker older than the record never said "addressed".
func (s *Service) RecordRun(ctx context.Context, tx *sql.Tx, run Run, round Round, fs []NewFinding, answers []Answer, passed bool, now time.Time) (stillOpen int, err error) {
	if !Reviews(run.Stage) && run.Stage != stageImplement {
		return 0, nil
	}
	settles := passed && Gating(run.Stage)
	if len(fs) == 0 && len(answers) == 0 && !settles {
		return 0, nil
	}
	members := round.members(run)
	existing, err := itemFindings(ctx, tx, run.ItemID)
	if err != nil {
		return 0, err
	}
	byID := map[string]Finding{}
	for _, f := range existing {
		byID[f.ID] = f
	}
	if Reviews(run.Stage) {
		n := 0
		for _, in := range fs {
			text := clip(in.Text, MaxText)
			if text == "" || in.ID == "" || n >= MaxPerRun {
				continue
			}
			n++
			sev := SeverityBlocking
			if in.Severity == SeverityNit {
				sev = SeverityNit
			}
			if err := insertFinding(ctx, tx, Finding{
				ID: in.ID, WorkspaceID: run.WorkspaceID, ItemID: run.ItemID, RunID: run.ID, Stage: run.Stage,
				Runner: run.Runner, Focus: run.Focus, HeadSHA: run.HeadSHA, N: n, Severity: sev, Text: text,
				File: clip(in.File, MaxFile), Source: SourceRun, CreatedAt: now,
			}); err != nil {
				return 0, err
			}
		}
	}
	seen := map[string]bool{}
	for i, a := range answers {
		if i >= MaxPerRun {
			break
		}
		id := strings.TrimSpace(a.ID)
		f, ok := byID[id]
		if !ok || seen[id] || f.RunID == run.ID {
			continue
		}
		kind := answerKind(run, members, f, strings.TrimSpace(a.Status))
		if kind == "" {
			continue
		}
		seen[id] = true
		if err := insertUpdate(ctx, tx, Update{
			ID: s.newID(), FindingID: f.ID, WorkspaceID: f.WorkspaceID, ItemID: f.ItemID, Kind: kind,
			Note: clip(a.Note, MaxNote), RunID: run.ID, HeadSHA: run.HeadSHA, CreatedAt: now,
		}); err != nil {
			return 0, err
		}
		if kind == KindStillOpen && f.Severity == SeverityBlocking {
			stillOpen++
		}
	}
	if !settles || stillOpen > 0 {
		return stillOpen, nil
	}
	// A passing review settles what was its to check and it did not answer
	// only when its worker cannot answer by name at all (a release from
	// before that). One that lists the findings and got no answer for one —
	// the read of the record failed, or the reviewer left it out — has not
	// checked it: it stays as it is, for the approver to see.
	if run.Listed != nil {
		return stillOpen, nil
	}
	for _, f := range existing {
		if seen[f.ID] || !f.Tracked || !pending(f.State) || inRound(members, f.RunID) || checker(members, f) != run.ID {
			continue
		}
		// Nor one the implementer said it did not fix: nothing says it is.
		if claim, _, _, _ := f.standing(); claim == KindNotFixed {
			continue
		}
		if err := insertUpdate(ctx, tx, Update{
			ID: s.newID(), FindingID: f.ID, WorkspaceID: f.WorkspaceID, ItemID: f.ItemID, Kind: KindAddressed,
			Note: NotRaisedAgain, RunID: run.ID, HeadSHA: run.HeadSHA, CreatedAt: now,
		}); err != nil {
			return 0, err
		}
	}
	return stillOpen, nil
}

// NotRaisedAgain is the note on a finding a passing review settled without
// answering for it by name.
const NotRaisedAgain = "not raised again: the review passed"

// answerKind is the update a run's answer becomes, "" when the record does
// not take it: only an implement run says fixed / not fixed, only a review
// says addressed / still open; a review that gates the item only about the
// findings that are its to check (checker), a second opinion only about
// its own runner's earlier ones.
func answerKind(run Run, members []Member, f Finding, status string) string {
	switch {
	case run.Stage == stageImplement:
		if !f.Tracked || !pending(f.State) {
			return ""
		}
		if status == KindFixed || status == KindNotFixed {
			return status
		}
	case run.Stage == stageSecondOpinion:
		if !ownSecondOpinion(run, f) {
			return ""
		}
		if status == KindAddressed || status == KindStillOpen {
			return status
		}
	case Gating(run.Stage):
		if !f.Tracked || !pending(f.State) || inRound(members, f.RunID) || checker(members, f) != run.ID {
			return ""
		}
		if status == KindAddressed || status == KindStillOpen {
			return status
		}
	}
	return ""
}

// Entry is a finding as a run's prompt reads it.
type Entry struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Text     string `json:"text"`
	File     string `json:"file,omitempty"`
	By       string `json:"by"`
	HeadSHA  string `json:"head_sha,omitempty"`
	State    string `json:"state"`
	// Claim: the implementer's newest word on it ("fixed" or "not_fixed")
	// with its note; ReviewNote: why the newest review found it still open;
	// WaiveNote: the approver's note when they accepted it.
	Claim      string `json:"claim,omitempty"`
	ClaimNote  string `json:"claim_note,omitempty"`
	ReviewNote string `json:"review_note,omitempty"`
	WaiveNote  string `json:"waive_note,omitempty"`
}

// View is the part of an item's record a run acts on.
type View struct {
	// Fix: what an implement run is to fix, and answer for.
	Fix []Entry `json:"fix"`
	// Check: what a review is to check again, and answer for.
	Check []Entry `json:"check"`
	// Others: followed-up findings another reviewer checks (context: not to
	// be reported again).
	Others []Entry `json:"others"`
	// Waived: what the approver accepted (not a finding any more).
	Waived []Entry `json:"waived"`
}

func entry(f Finding) Entry {
	e := Entry{ID: f.ID, Severity: f.Severity, Text: f.Text, File: f.File, By: f.By(), HeadSHA: f.HeadSHA, State: f.State}
	e.Claim, e.ClaimNote, e.ReviewNote, e.WaiveNote = f.standing()
	if f.State != StateWaived {
		e.WaiveNote = ""
	}
	return e
}

// ForRun is what a run acts on.
//
//   - An implement run fixes what is followed up and not yet claimed fixed.
//   - A review checks the earlier findings that are its to check (checker):
//     its own (same kind of stage, same focus), and for the round's first
//     reviewer those no member answers for — a second opinion's that the
//     approver sent back, a focus that is no longer in the panel. What a
//     sibling found in this same round is not an earlier finding: it is in
//     none of the lists.
//   - A second opinion checks its own runner's earlier blocking findings,
//     sent back or not, until one is addressed or waived.
func (s *Service) ForRun(ctx context.Context, run Run, round Round) (*View, error) {
	all, err := s.st.ItemFindings(ctx, run.ItemID)
	if err != nil {
		return nil, err
	}
	v := &View{Fix: []Entry{}, Check: []Entry{}, Others: []Entry{}, Waived: []Entry{}}
	members := round.members(run)
	for _, f := range all {
		if f.RunID == run.ID || (Gating(run.Stage) && inRound(members, f.RunID)) {
			continue
		}
		if f.State == StateWaived {
			v.Waived = append(v.Waived, entry(f))
			continue
		}
		owed := f.Tracked && pending(f.State)
		switch {
		case run.Stage == stageImplement:
			if f.Tracked && (f.State == StateOpen || f.State == StateStillOpen) {
				v.Fix = append(v.Fix, entry(f))
			}
		case run.Stage == stageSecondOpinion:
			switch {
			case ownSecondOpinion(run, f):
				v.Check = append(v.Check, entry(f))
			case owed:
				v.Others = append(v.Others, entry(f))
			}
		case Gating(run.Stage):
			if !owed {
				continue
			}
			if checker(members, f) == run.ID {
				v.Check = append(v.Check, entry(f))
			} else {
				v.Others = append(v.Others, entry(f))
			}
		}
	}
	return v, nil
}

// RunAnswer is what one run said about an earlier finding.
type RunAnswer struct {
	Finding Finding
	Kind    string
	Note    string
}

// RunAnswers is what a run said about earlier findings, in the order it was
// recorded — for the summary a review's result is shown with.
func (s *Service) RunAnswers(ctx context.Context, itemID, runID string) ([]RunAnswer, error) {
	all, err := s.st.ItemFindings(ctx, itemID)
	if err != nil {
		return nil, err
	}
	var out []RunAnswer
	for _, f := range all {
		for _, u := range f.Updates {
			if u.RunID == runID && runID != "" {
				out = append(out, RunAnswer{Finding: f, Kind: u.Kind, Note: u.Note})
			}
		}
	}
	return out, nil
}

// Item is an item's whole record, oldest finding first.
func (s *Service) Item(ctx context.Context, itemID string) ([]Finding, error) {
	return s.st.ItemFindings(ctx, itemID)
}

// Who is the person (or key) acting on a finding.
type Who struct {
	Actor string
	KeyID string
	Via   string
}

// resolve returns the item's findings named by ids, in the order given,
// refusing an id the item does not have.
func (s *Service) resolve(ctx context.Context, itemID string, ids []string) ([]Finding, error) {
	all, err := s.st.ItemFindings(ctx, itemID)
	if err != nil {
		return nil, err
	}
	byID := map[string]Finding{}
	for _, f := range all {
		byID[f.ID] = f
	}
	var out []Finding
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		f, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: this item has no finding %s", ErrNotFound, id)
		}
		out = append(out, f)
	}
	return out, nil
}

// Check reports whether every id names a finding of the item — asked
// before an action that would send them back, so a wrong id stops it.
func (s *Service) Check(ctx context.Context, itemID string, ids []string) error {
	_, err := s.resolve(ctx, itemID, ids)
	return err
}

// SendBack records that the approver wants the findings fixed: each is
// followed up from here on, whoever raised it and whatever its state was.
func (s *Service) SendBack(ctx context.Context, itemID string, ids []string, who Who) ([]Finding, error) {
	fs, err := s.resolve(ctx, itemID, ids)
	if err != nil || len(fs) == 0 {
		return nil, err
	}
	now := s.now().UTC()
	us := make([]Update, 0, len(fs))
	for _, f := range fs {
		us = append(us, Update{ID: s.newID(), FindingID: f.ID, WorkspaceID: f.WorkspaceID, ItemID: f.ItemID,
			Kind: KindSentBack, Actor: who.Actor, ActorKeyID: who.KeyID, Via: who.Via, CreatedAt: now})
	}
	if err := s.st.InsertUpdates(ctx, us); err != nil {
		return nil, err
	}
	return s.resolve(ctx, itemID, ids)
}

// Waive records that the approver accepts a finding as it is. A finding
// already waived is left alone. Accepting a blocking finding needs a
// reason: it is an exception to what a review asked for, and whoever reads
// the record later has to see why it was made.
func (s *Service) Waive(ctx context.Context, itemID, id, note string, who Who) (*Finding, error) {
	fs, err := s.resolve(ctx, itemID, []string{id})
	if err != nil {
		return nil, err
	}
	if len(fs) == 0 {
		return nil, fmt.Errorf("%w: a finding id is required", ErrInvalidInput)
	}
	f := fs[0]
	if f.State == StateWaived {
		return &f, nil
	}
	if f.Severity == SeverityBlocking && strings.TrimSpace(note) == "" {
		return nil, fmt.Errorf("%w: a reason is required to waive a blocking finding", ErrInvalidInput)
	}
	u := Update{ID: s.newID(), FindingID: f.ID, WorkspaceID: f.WorkspaceID, ItemID: f.ItemID, Kind: KindWaived,
		Note: clip(note, MaxNote), Actor: who.Actor, ActorKeyID: who.KeyID, Via: who.Via, CreatedAt: s.now().UTC()}
	if err := s.st.InsertUpdates(ctx, []Update{u}); err != nil {
		return nil, err
	}
	fs, err = s.resolve(ctx, itemID, []string{id})
	if err != nil {
		return nil, err
	}
	return &fs[0], nil
}

// Import records the findings of runs that reported before findings were
// records, so an item's history is in one place. They are never followed
// up by themselves: nothing says whether a later round fixed them. It is
// safe to run at every start: a run with rows is skipped.
func (s *Service) Import(ctx context.Context) (runs, findings int, err error) {
	rs, err := s.st.unrecordedRuns(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, r := range rs {
		var raw []struct {
			Severity string `json:"severity"`
			Text     string `json:"text"`
			File     string `json:"file"`
		}
		if json.Unmarshal([]byte(r.Findings), &raw) != nil {
			continue
		}
		at, perr := time.Parse(time.RFC3339Nano, r.UpdatedAt)
		if perr != nil {
			at = s.now().UTC()
		}
		var fs []Finding
		for _, in := range raw {
			text := clip(in.Text, MaxText)
			if text == "" || len(fs) >= MaxPerRun {
				continue
			}
			sev := SeverityBlocking
			if in.Severity == SeverityNit {
				sev = SeverityNit
			}
			fs = append(fs, Finding{ID: s.newID(), WorkspaceID: r.WorkspaceID, ItemID: r.ItemID, RunID: r.ID, Stage: r.Stage,
				Runner: r.Runner, Focus: r.Focus, HeadSHA: r.HeadSHA, N: len(fs) + 1, Severity: sev, Text: text,
				File: clip(in.File, MaxFile), Source: SourceImported, CreatedAt: at})
		}
		if len(fs) == 0 {
			continue
		}
		if err := s.st.importRun(ctx, fs); err != nil {
			return runs, findings, err
		}
		runs++
		findings += len(fs)
	}
	return runs, findings, nil
}
