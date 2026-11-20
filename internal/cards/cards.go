// Package cards shows an item to another system as a card: POST
// /api/inbox/card takes the other system's subject (its reference, title,
// link) and answers with an envelope — a status name the host maps to its
// own, whether a person must act, a one-line summary, a key that changes
// exactly when something new happened, and an A2UI 1.0 surface
// (a2ui.org, the basic catalog) the host draws with its own components.
// The host needs to know nothing about skopos's workflow; skopos needs to
// know nothing about the host. Built from the same state as GET
// /api/inbox/{id}/state (internal/itemstate), so the card and the state
// never disagree. Render only: a card carries no buttons yet.
package cards

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/itemstate"
	"github.com/martinsuchenak/skopos/internal/runs"
)

// Status names a card can carry. A host maps them to its own states.
const (
	StatusNone         = "none"        // no item has the subject's reference
	StatusNotStarted   = "not_started" // an item, not in the pipeline yet
	StatusAgent        = "agent"       // the agents are working
	StatusPlanApproval = itemstate.GatePlanApproval
	StatusBlocked      = itemstate.GateBlocked
	StatusFailed       = itemstate.GateFailed
	StatusSignOff      = itemstate.GateSignOff
	StatusDone         = "done"
	StatusDiscarded    = "discarded"
	StatusNothingToDo  = "nothing_to_do" // closed: the planner found nothing to do
)

// Statuses lists every status name, for documentation and tests.
var Statuses = []string{StatusNone, StatusNotStarted, StatusAgent, StatusPlanApproval, StatusBlocked,
	StatusFailed, StatusSignOff, StatusDone, StatusDiscarded, StatusNothingToDo}

// Subject is what the host shows the card on: its own record.
type Subject struct {
	Ref   string `json:"ref"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
	Text  string `json:"text,omitempty"`
}

// Envelope is the answer: what the host needs that is not UI, and the UI.
type Envelope struct {
	Status    string `json:"status"`
	Attention bool   `json:"attention"`
	Summary   string `json:"summary"`
	Key       string `json:"key"`
	URL       string `json:"url,omitempty"`
	A2UI      []any  `json:"a2ui"`
}

// Input is what a card is built from: the subject, and the item's state
// (nil when no item has the reference).
type Input struct {
	Subject Subject
	State   *itemstate.State
	// NothingToDo is why the newest plan round found nothing to do ("" =
	// it did not; inbox.NewestRoundNothingToDo).
	NothingToDo string
	// Outcome is the item's branch after sign-off, when reported.
	Outcome *inbox.BranchOutcome
	// PublicURL is the server's public address ("" = no link).
	PublicURL string
}

// Limits of what a card shows.
const (
	maxReason      = 1500
	maxStep        = 1200
	maxQuestion    = 1000
	maxFinding     = 600
	maxSummaryLine = 160
)

// Build is the card: pure, the same input gives the same envelope.
func Build(in Input) *Envelope {
	st := in.State
	s := &surface{}
	if st == nil {
		env := &Envelope{Status: StatusNone, Summary: "Not sent to skopos", Key: "none"}
		title := "**Not sent to skopos.** No skopos item has the reference " + code(in.Subject.Ref) + "."
		env.A2UI = s.createSurface(surfaceID(in.Subject.Ref), []string{s.text(title)})
		return env
	}
	env := &Envelope{Status: status(st, in.NothingToDo), Attention: st.Turn == itemstate.TurnPerson}
	if in.PublicURL != "" {
		env.URL = in.PublicURL + "/?item=" + st.Item.ID
	}
	var body []string
	switch env.Status {
	case StatusNotStarted:
		env.Summary = "Not started"
		body = append(body, s.text("**Not started.** The item is in skopos and waits for someone to queue it."))
	case StatusAgent:
		env.Summary, body = agent(s, st)
	case StatusPlanApproval:
		env.Summary, body = planApproval(s, st, in.NothingToDo)
	case StatusBlocked, StatusFailed:
		env.Summary, body = stopped(s, st, env.Status)
	case StatusSignOff:
		env.Summary, body = signOff(s, st)
	case StatusDone:
		if inbox.Status(st.Item.Status) == inbox.StatusConverted {
			env.Summary, body = "Done: converted into a plan", []string{s.text("**Done.** The item was converted into a plan.")}
		} else {
			env.Summary, body = done(s, st, in.Outcome)
		}
	case StatusDiscarded:
		env.Summary = "Discarded"
		body = append(body, s.text("**Discarded.** Nothing more happens to this item."))
	case StatusNothingToDo:
		env.Summary = clip("Closed: nothing to do — "+in.NothingToDo, maxSummaryLine)
		body = append(body, s.text("**Closed: the planner found nothing to do.**"), s.card(s.caption(clip(in.NothingToDo, maxReason))))
	}
	if q := questions(s, st.Questions); len(q) > 0 {
		body = append(body, s.divider())
		body = append(body, q...)
	}
	env.Key = key(st, env.Status, in.NothingToDo)
	env.A2UI = s.createSurface(surfaceID(st.Item.ID), body)
	return env
}

// status is the card's status name for the state.
func status(st *itemstate.State, nothingToDo string) string {
	if st.Gate != "" {
		return st.Gate
	}
	switch inbox.Status(st.Item.Status) {
	case inbox.StatusDone:
		if nothingToDo != "" && st.Implementation == nil {
			return StatusNothingToDo
		}
		return StatusDone
	case inbox.StatusConverted:
		return StatusDone
	case inbox.StatusDiscarded:
		return StatusDiscarded
	case inbox.StatusOpen, inbox.StatusInProgress:
		return StatusNotStarted
	}
	return StatusAgent
}

// key changes exactly when something new happened: a new item, another
// status, a new plan round or question set, a new reviewed head, another
// reason to stop.
func key(st *itemstate.State, status, nothingToDo string) string {
	parts := []string{st.Item.ID, status}
	switch status {
	case StatusPlanApproval:
		if st.Plan != nil {
			for _, r := range st.Plan.Revisions {
				parts = append(parts, r.RevisionID)
			}
		}
		if nothingToDo != "" {
			parts = append(parts, "nothing:"+shortHash(nothingToDo))
		}
	case StatusBlocked, StatusFailed:
		parts = append(parts, shortHash(st.Reason))
	case StatusSignOff:
		if st.Implementation != nil {
			parts = append(parts, st.Implementation.HeadSHA)
		}
		for _, r := range st.Reviews {
			parts = append(parts, r.RunID)
		}
	}
	var sets []string
	for _, q := range st.Questions {
		if !slices.Contains(sets, q.SetID) {
			sets = append(sets, q.SetID)
		}
	}
	slices.Sort(sets)
	parts = append(parts, sets...)
	return strings.Join(parts, ":")
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// surfaceID names the surface after the item (or the reference, for none).
func surfaceID(id string) string { return "skopos-" + id }

func code(s string) string {
	if s == "" {
		return "(none)"
	}
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func stageWord(stage string) string {
	switch stage {
	case runs.StagePlan:
		return "planning"
	case runs.StageImplement:
		return "implementing"
	case runs.StageReview, runs.StageBranchReview:
		return "reviewing"
	case runs.StageQA:
		return "testing (QA)"
	case runs.StageSecondOpinion:
		return "a second opinion"
	}
	return strings.ReplaceAll(stage, "_", " ")
}

func agent(s *surface, st *itemstate.State) (string, []string) {
	if st.Run == nil {
		return "Skopos is working on it", []string{s.text("**Skopos is working on it.**")}
	}
	what := stageWord(st.Run.Stage)
	line := "**Skopos is working on it:** " + what
	if st.Run.Status == string(runs.StatusQueued) {
		line = "**Queued:** " + what + ", waiting for a worker"
	} else if st.Run.Runner != "" {
		line += " (" + st.Run.Runner + ")"
	}
	return "Skopos is working on it: " + what, []string{s.text(line + ".")}
}

func planApproval(s *surface, st *itemstate.State, nothingToDo string) (string, []string) {
	if nothingToDo != "" {
		return clip("Waiting for you: the planner found nothing to do — "+nothingToDo, maxSummaryLine),
			[]string{s.text("**The planner found nothing to do.** Discard the item, or send it back with what to do."), s.card(s.caption(clip(nothingToDo, maxReason)))}
	}
	var revs []itemstate.RevisionView
	if st.Plan != nil {
		revs = st.Plan.Revisions
	}
	summary := "Plan ready for approval"
	if len(revs) > 1 {
		summary += " — " + plural(len(revs), "plan", "plans")
	}
	if n := len(st.Questions); n > 0 {
		summary += ", " + plural(n, "question", "questions")
		if len(revs) <= 1 {
			summary = "Plan ready for approval — " + plural(n, "question", "questions")
		}
	}
	body := []string{s.text("**Plan ready for approval.**")}
	switch len(revs) {
	case 0:
		body = append(body, s.caption("The plan is not readable with this key."))
	case 1:
		body = append(body, s.text(steps(revs[0])))
	default:
		ts := make([]tab, 0, len(revs))
		for i, r := range revs {
			title := fmt.Sprintf("Plan %d", i+1)
			if r.Label != "" {
				title += " — " + r.Label
			}
			ts = append(ts, tab{title: title, child: s.text(steps(r))})
		}
		body = append(body, s.tabs(ts))
	}
	return summary, body
}

// steps is a plan revision's steps as a numbered Markdown list.
func steps(r itemstate.RevisionView) string {
	if len(r.Steps) == 0 {
		return "_No steps._"
	}
	var b strings.Builder
	for i, st := range r.Steps {
		// One line: a break in the title would end the list item.
		fmt.Fprintf(&b, "%d. **%s**", i+1, firstLine(st.Title))
		if d := clip(st.Description, maxStep); d != "" {
			// A hard break: an indented line alone would join the title's
			// paragraph.
			b.WriteString("\\\n   ")
			b.WriteString(indent(d, "   "))
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func stopped(s *surface, st *itemstate.State, status string) (string, []string) {
	word := "Blocked"
	if status == StatusFailed {
		word = "Failed"
	}
	reason := clip(st.Reason, maxReason)
	summary := word
	if l := firstLine(reason); l != "" {
		summary += ": " + l
	}
	body := []string{s.text("**" + word + ".** The work stopped and waits for you.")}
	if reason != "" {
		body = append(body, s.card(s.caption(reason)))
	}
	return clip(summary, maxSummaryLine), body
}

func signOff(s *surface, st *itemstate.State) (string, []string) {
	head := "**Branch ready for sign-off.**"
	impl := st.Implementation
	if impl != nil {
		if impl.PRURL != "" {
			head += " [Pull request](" + impl.PRURL + ")"
		}
		if impl.HeadSHA != "" {
			head += " at " + code(short(impl.HeadSHA))
		}
		head += "."
	}
	body := []string{s.text(head)}
	var facts []string
	if impl != nil && len(impl.Checks) > 0 {
		facts = append(facts, "- "+checksLine(impl.Checks))
	}
	for _, r := range st.Reviews {
		facts = append(facts, "- "+reviewLine(r))
	}
	if len(facts) > 0 {
		body = append(body, s.card(s.text(strings.Join(facts, "\n"))))
	}
	open := openFindings(st)
	if len(open) > 0 {
		var b strings.Builder
		for _, f := range open {
			fmt.Fprintf(&b, "- **%s** %s", f.Severity, clip(f.Text, maxFinding))
			if f.File != "" {
				fmt.Fprintf(&b, " (%s)", code(f.File))
			}
			b.WriteString("\n")
		}
		body = append(body, s.text("**Findings still open**"), s.text(strings.TrimSpace(b.String())))
	}
	summary := "Branch ready for sign-off"
	if n := blocking(open); n > 0 {
		summary += " — " + plural(n, "blocking finding", "blocking findings") + " open"
	}
	return summary, body
}

func openFindings(st *itemstate.State) []itemstate.FindingView {
	if st.Findings == nil {
		return nil
	}
	return st.Findings.Open
}

func blocking(fs []itemstate.FindingView) int {
	n := 0
	for _, f := range fs {
		if f.Severity == "blocking" {
			n++
		}
	}
	return n
}

func checksLine(cs []runs.Check) string {
	var failed, names []string
	for _, c := range cs {
		names = append(names, c.Name)
		if !c.Passed {
			n := c.Name
			if c.Stopped {
				n += " (stopped)"
			}
			failed = append(failed, n)
		}
	}
	if len(failed) > 0 {
		return "Checks: **failed** — " + strings.Join(failed, ", ")
	}
	return "Checks: passed — " + strings.Join(names, ", ")
}

func reviewLine(r itemstate.ReviewView) string {
	who := stageWord(r.Stage)
	switch r.Stage {
	case runs.StageReview, runs.StageBranchReview:
		who = "Review"
	case runs.StageSecondOpinion:
		who = "Second opinion"
	}
	if r.Runner != "" {
		who += " (" + r.Runner + ")"
	}
	if r.Passed {
		if r.Nits > 0 {
			return who + ": passed, " + plural(r.Nits, "nit", "nits")
		}
		return who + ": passed"
	}
	if r.Status != string(runs.StatusSucceeded) {
		return who + ": did not finish"
	}
	return fmt.Sprintf("%s: **%s**, %s", who, plural(r.Blocking, "blocking", "blocking"), plural(r.Nits, "nit", "nits"))
}

func done(s *surface, st *itemstate.State, out *inbox.BranchOutcome) (string, []string) {
	line := "**Done.** Signed off."
	summary := "Done"
	if out != nil {
		switch out.State {
		case inbox.OutcomeMerged:
			line += " The pull request was merged."
			summary += ", merged"
		case inbox.OutcomeClosed:
			line += " The pull request was closed without merging."
			summary += ", pull request closed"
		}
	}
	if impl := st.Implementation; impl != nil && impl.PRURL != "" {
		line += " [Pull request](" + impl.PRURL + ")"
	}
	return summary, []string{s.text(line)}
}

// questions are the open questions, by set: a heading naming who asks,
// then the questions with their options.
func questions(s *surface, qs []itemstate.QuestionView) []string {
	if len(qs) == 0 {
		return nil
	}
	var out []string
	var b strings.Builder
	set, n, started := "", 0, false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, s.text(strings.TrimSpace(b.String())))
			b.Reset()
		}
	}
	for _, q := range qs {
		if !started || q.SetID != set {
			started = true
			flush()
			set, n = q.SetID, 0
			heading := "**Questions — " + asker(q) + "**"
			if q.Blocking {
				heading += " (the work waits for the answers)"
			}
			out = append(out, s.text(heading))
		}
		n++
		fmt.Fprintf(&b, "%d. %s\n", n, indent(clip(q.Text, maxQuestion), "   "))
		for i, o := range q.Options {
			fmt.Fprintf(&b, "   - (%c) %s\n", 'a'+i, indent(clip(o, maxQuestion), "     "))
		}
	}
	flush()
	return out
}

func asker(q itemstate.QuestionView) string {
	who := q.Asker
	switch q.Asker {
	case "worker":
		who = "QA"
	case "":
		who = "skopos"
	}
	if q.Label != "" {
		who += " " + q.Label
	}
	return who
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
