// Package findings is the record of what reviews found and what became of
// each finding: the implementer's word on it, the next review's check, the
// approver's decision. A finding is never changed; what happens to it is
// appended as an update, and its state is derived from those.
//
// It knows nothing of runs or the inbox: the run queue records a review's
// findings and a run's answers when the run reports, the inbox records the
// approver's actions, and prompts read through ForRun.
package findings

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidInput = errors.New("invalid finding input")
	ErrNotFound     = errors.New("finding not found")
)

const (
	SeverityBlocking = "blocking"
	SeverityNit      = "nit"

	// SourceRun: recorded when its run reported. SourceImported: taken from
	// a run that reported before findings were records.
	SourceRun      = "run"
	SourceImported = "imported"
)

// What can happen to a finding.
const (
	// KindSentBack: the approver asked for it to be fixed (request changes
	// or retry, by id).
	KindSentBack = "sent_back"
	// KindFixed / KindNotFixed: the implementer's word on it.
	KindFixed    = "fixed"
	KindNotFixed = "not_fixed"
	// KindAddressed / KindStillOpen: a later review checked it.
	KindAddressed = "addressed"
	KindStillOpen = "still_open"
	// KindWaived: the approver accepts it as it is.
	KindWaived = "waived"
)

// A finding's state, derived from its updates.
const (
	// StateOpen: to be fixed; nobody has said it is.
	StateOpen = "open"
	// StateFixed: the implementer says it is fixed; no review confirmed it.
	StateFixed = "fixed"
	// StateAddressed: a review confirmed it is fixed.
	StateAddressed = "addressed"
	// StateStillOpen: a review checked it and it is not fixed.
	StateStillOpen = "still_open"
	// StateWaived: the approver accepted it.
	StateWaived = "waived"
	// StateNoted: recorded and not followed up — a nit, an advisory second
	// opinion's finding, or one from before findings were records — until
	// the approver sends it back.
	StateNoted = "noted"
)

// Stage names, as the run queue has them (kept here so the package stands
// alone).
const (
	stageImplement     = "implement"
	stageReview        = "review"
	stageQA            = "qa"
	stageBranchReview  = "branch_review"
	stageSecondOpinion = "second_opinion"
)

// Limits on what an agent or a person may write into the record.
const (
	MaxText = 2000
	MaxNote = 1000
	MaxFile = 300
	// MaxPerRun bounds one report's findings and answers.
	MaxPerRun = 100
)

// Finding is one thing a review found.
type Finding struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	ItemID      string    `json:"item_id"`
	RunID       string    `json:"run_id"`
	Stage       string    `json:"stage"`
	Runner      string    `json:"runner,omitempty"`
	Focus       string    `json:"focus,omitempty"`
	HeadSHA     string    `json:"head_sha,omitempty"`
	N           int       `json:"n"`
	Severity    string    `json:"severity"`
	Text        string    `json:"text"`
	File        string    `json:"file,omitempty"`
	Source      string    `json:"source"`
	CreatedAt   time.Time `json:"created_at"`

	// Derived from the updates (never stored).
	State string `json:"state"`
	// Tracked: it is followed up — the implementer is told to fix it and a
	// later review to check it. True for a blocking finding of a review
	// that gates the item, and for anything the approver sent back.
	Tracked  bool     `json:"tracked"`
	SentBack bool     `json:"sent_back"`
	Updates  []Update `json:"updates"`
}

// Update is one thing that happened to a finding.
type Update struct {
	ID          string    `json:"id"`
	FindingID   string    `json:"finding_id"`
	WorkspaceID string    `json:"workspace_id"`
	ItemID      string    `json:"item_id"`
	Kind        string    `json:"kind"`
	Note        string    `json:"note,omitempty"`
	RunID       string    `json:"run_id,omitempty"`
	HeadSHA     string    `json:"head_sha,omitempty"`
	Actor       string    `json:"actor,omitempty"`
	ActorKeyID  string    `json:"-"`
	Via         string    `json:"via,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Gating reports whether a stage's verdict moves the item (a review, QA or
// a review-only run); a second opinion is advisory.
func Gating(stage string) bool {
	return stage == stageReview || stage == stageQA || stage == stageBranchReview
}

// Reviews reports whether a stage produces findings.
func Reviews(stage string) bool { return Gating(stage) || stage == stageSecondOpinion }

// family groups the stages whose reviewers check each other's findings: a
// review-only run's findings are checked by the review stage that follows a
// handover, QA checks QA's, and what the approver sent back from a second
// opinion goes to the review stage.
func family(stage string) string {
	if stage == stageQA {
		return stageQA
	}
	return stageReview
}

// By names who raised a finding: "review", "review · security",
// "second opinion · codex".
func (f Finding) By() string {
	name := strings.ReplaceAll(f.Stage, "_", " ")
	switch {
	case f.Stage == stageSecondOpinion && f.Runner != "":
		return name + " · " + f.Runner
	case f.Focus != "":
		return name + " · " + f.Focus
	}
	return name
}

// pending: something is still owed on it — a fix, or a review's check.
func pending(state string) bool {
	return state == StateOpen || state == StateFixed || state == StateStillOpen
}

// derive fills State, Tracked and SentBack from the updates (oldest first).
//
// The approver's word is final against an agent's: a waived finding stays
// waived whatever a later run says, until the approver sends it back. An
// implementer's claim never reopens or closes a finding a review settled.
func derive(f *Finding) {
	tracked := f.Source == SourceRun && Gating(f.Stage) && f.Severity == SeverityBlocking
	state := StateNoted
	if tracked {
		state = StateOpen
	}
	f.SentBack = false
	// stillOpenAt: the commits a review found it not fixed on. Another
	// reviewer's "addressed" for the same commit does not undo that: two
	// checks of one commit that disagree leave it open.
	stillOpenAt := map[string]string{}
	for _, u := range f.Updates {
		switch u.Kind {
		case KindSentBack:
			f.SentBack, tracked, state = true, true, StateOpen
		case KindWaived:
			state = StateWaived
		case KindFixed:
			if state == StateOpen || state == StateStillOpen {
				state = StateFixed
			}
		case KindNotFixed:
			if state == StateFixed || state == StateStillOpen {
				state = StateOpen
			}
		case KindAddressed:
			if by, ok := stillOpenAt[u.HeadSHA]; ok && u.HeadSHA != "" && by != u.RunID {
				continue
			}
			if state != StateWaived {
				state = StateAddressed
			}
		case KindStillOpen:
			stillOpenAt[u.HeadSHA] = u.RunID
			if state != StateWaived {
				state = StateStillOpen
			}
		}
	}
	f.State, f.Tracked = state, tracked
}

// standing is what a prompt says about a finding besides its text: only
// what still holds. The implementer's claim stands until a review answers
// it or the approver sends the finding back; a review's "not fixed" note
// stands until a review finds it fixed or it is sent back.
func (f Finding) standing() (claim, claimNote, reviewNote, waiveNote string) {
	for _, u := range f.Updates {
		switch u.Kind {
		case KindSentBack:
			claim, claimNote, reviewNote, waiveNote = "", "", "", ""
		case KindFixed, KindNotFixed:
			claim, claimNote = u.Kind, u.Note
		case KindStillOpen:
			claim, claimNote, reviewNote = "", "", u.Note
		case KindAddressed:
			claim, claimNote, reviewNote = "", "", ""
		case KindWaived:
			waiveNote = u.Note
		}
	}
	return claim, claimNote, reviewNote, waiveNote
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
