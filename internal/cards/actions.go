package cards

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/itemstate"
	"github.com/martinsuchenak/skopos/internal/runs"
)

// The actions a card's buttons send: the A2UI event names Request.Action
// carries back. They are the item state's action names (itemstate.Actions),
// so a card offers exactly what the key may do on the item now.
const (
	ActionQueue          = "queue"
	ActionReview         = "review"
	ActionApprove        = "approve"
	ActionRequestChanges = "request-changes"
	ActionAnswer         = "answers"
	ActionRetry          = "retry"
	ActionMarkDone       = "mark-done"
	ActionCheckCommits   = "check-commits"
	ActionSecondOpinion  = "second-opinion"
	ActionReject         = "reject"
)

// buttonOrder is the order a card draws its buttons in, and their labels:
// the gate's main action first, the one that ends the item last.
var buttonOrder = []struct{ action, label string }{
	{ActionQueue, "Send to the agents"},
	{ActionReview, "Review the existing branch"},
	{ActionApprove, "Approve"},
	{ActionMarkDone, "Sign off"},
	{ActionRetry, "Retry"},
	{ActionRequestChanges, "Request changes"},
	{ActionAnswer, "Only send the answers"},
	{ActionCheckCommits, "Check the new commits"},
	{ActionSecondOpinion, "Ask for a second opinion"},
	{ActionReject, "Reject"},
}

// primary is the gate's main action: the one drawn as the primary button.
var primary = []string{ActionQueue, ActionApprove, ActionMarkDone, ActionRetry}

// answerable: the statuses at which a person answers the open questions
// (inbox.Answer takes answers then).
func answerable(status string) bool {
	switch status {
	case StatusPlanApproval, StatusBlocked, StatusFailed:
		return true
	}
	return false
}

// form is what actionForm adds to a card: its components and the data model
// they start from.
type form struct {
	body      []string
	dataModel map[string]any
}

// tickableFindings: open findings can be sent back to the implementer now,
// with an action the key may take (the state's send_back: request changes at
// sign-off, a retry that starts an implementation).
func tickableFindings(st *itemstate.State) bool {
	if len(openFindings(st)) == 0 || st.Findings.SendBack == "" {
		return false
	}
	return slices.Contains(st.Actions, st.Findings.SendBack)
}

// since is when the item entered its status, as a button's context carries
// it: with the card's key it tells a card drawn before a new stop or a new
// stage of the agents' work from the card now ("" = not known).
func since(st *itemstate.State) string {
	if st.Item.StatusSince == nil {
		return ""
	}
	return st.Item.StatusSince.UTC().Format(time.RFC3339Nano)
}

// actionForm is the card's inputs and buttons for the actions the state
// allows: the open questions (each its options and a text field, under
// /answers/<question id>), the plan to approve when there are several
// (/plan/choice), the open findings to send back (/findings/<id>), a notes
// field (/notes), then one button per action. Every button's context names
// the item, the card's key and when the item entered its status, so an
// action on a card that has changed since it was drawn is refused rather than
// applied to something else.
func actionForm(s *surface, st *itemstate.State, status, cardKey string) form {
	if len(st.Actions) == 0 {
		return form{}
	}
	var out []string
	model := map[string]any{}
	context := map[string]any{"item": st.Item.ID, "key": cardKey, "since": since(st)}

	if answerable(status) && len(st.Questions) > 0 {
		inputs, answers := questionInputs(s, st.Questions)
		out = append(out, inputs...)
		model["answers"] = answers
		context["answers"] = bind("/answers")
	}

	if tickableFindings(st) {
		parts := []string{s.caption("Tick the ones to send back to the implementer.")}
		ticks := map[string]any{}
		for _, f := range openFindings(st) {
			label := f.Severity + ": " + clip(f.Text, maxFinding)
			if f.File != "" {
				label += " (" + f.File + ")"
			}
			parts = append(parts, s.add("CheckBox", Component{"label": label, "value": bind("/findings/" + f.ID)}))
			ticks[f.ID] = false
		}
		out = append(out, s.section("Findings still open", parts...))
		model["findings"] = ticks
		context["findings"] = bind("/findings")
	}

	// The decision: the plan to approve when there are several, a note, the
	// buttons.
	var decision []string
	if revs := revisions(st); slices.Contains(st.Actions, ActionApprove) && len(revs) > 1 {
		opts := make([]map[string]any, 0, len(revs))
		for i, r := range revs {
			opts = append(opts, map[string]any{"label": planTitle(i, r), "value": r.RevisionID})
		}
		decision = append(decision, s.add("ChoicePicker", Component{
			"label": "The plan to approve", "variant": "mutuallyExclusive", "options": opts, "value": bind("/plan/choice"),
		}))
		model["plan"] = map[string]any{"choice": []any{}}
		context["plan"] = bind("/plan/choice")
	}
	if label := notesLabel(st, status); label != "" {
		decision = append(decision, s.add("TextField", Component{"label": label, "variant": "longText", "value": bind("/notes")}))
		model["notes"] = ""
		context["notes"] = bind("/notes")
	}

	var buttons []string
	hasQuestions := context["answers"] != nil
	for _, b := range buttonOrder {
		if !slices.Contains(st.Actions, b.action) {
			continue
		}
		if b.action == ActionAnswer && !hasQuestions {
			continue // nothing to answer
		}
		label := b.label
		if b.action == ActionRetry && hasQuestions {
			label = "Send answers and continue"
		}
		if b.action == ActionAnswer && !slices.ContainsFunc(st.Actions, func(a string) bool { return slices.Contains(primary, a) }) {
			label = "Send answers"
		}
		variant := "default"
		switch {
		case slices.Contains(primary, b.action):
			variant = "primary"
		case b.action == ActionReject:
			variant = "borderless"
		}
		buttons = append(buttons, s.button(label, variant, b.action, context))
	}
	if len(buttons) == 0 {
		return form{}
	}
	decision = append(decision, s.add("Row", Component{"children": buttons}))
	if hint := formHint(st, hasQuestions); hint != "" {
		decision = append(decision, s.caption(hint))
	}
	out = append(out, s.section("Your decision", decision...))
	return form{body: out, dataModel: model}
}

// questionInputs are the open questions as inputs, by set: per question its
// options to pick from and a text field.
func questionInputs(s *surface, qs []itemstate.QuestionView) ([]string, map[string]any) {
	var out []string
	answers := map[string]any{}
	for _, set := range questionSets(qs) {
		var parts []string
		if set[0].Blocking {
			parts = append(parts, s.caption("The work waits for the answers."))
		}
		for i, q := range set {
			parts = append(parts, s.text(fmt.Sprintf("**%d.** %s", number(q, i+1), clip(q.Text, maxQuestion))))
			path := "/answers/" + q.ID
			textLabel := "Your answer"
			if len(q.Options) > 0 {
				opts := make([]map[string]any, 0, len(q.Options))
				for j, o := range q.Options {
					letter := string(rune('a' + j))
					opts = append(opts, map[string]any{"label": "(" + letter + ") " + clip(o, maxQuestion), "value": letter})
				}
				parts = append(parts, s.add("ChoicePicker", Component{
					"label": "Answer", "variant": "mutuallyExclusive", "options": opts, "value": bind(path + "/option"),
				}))
				textLabel = "Or in your own words"
			}
			parts = append(parts, s.add("TextField", Component{"label": textLabel, "variant": "longText", "value": bind(path + "/text")}))
			answers[q.ID] = map[string]any{"option": []any{}, "text": ""}
		}
		out = append(out, s.section("Questions — "+asker(set[0]), parts...))
	}
	return out, answers
}

// notesLabel is the notes field's label, for the actions that take notes
// ("" = none of them is offered).
func notesLabel(st *itemstate.State, status string) string {
	takesNotes := false
	for _, a := range []string{ActionApprove, ActionRequestChanges, ActionRetry, ActionReject} {
		if slices.Contains(st.Actions, a) {
			takesNotes = true
		}
	}
	if !takesNotes {
		return ""
	}
	switch status {
	case StatusPlanApproval:
		return "Notes for the implementer, or what to change in the plan"
	case StatusSignOff:
		return "What to change (with Request changes), or why you reject"
	case StatusBlocked, StatusFailed:
		return "An answer to the stop, or why you reject"
	}
	return "Why you reject it"
}

// formHint is the caption under the buttons.
func formHint(st *itemstate.State, hasQuestions bool) string {
	var parts []string
	if hasQuestions {
		parts = append(parts, "A question left blank stays open.")
	}
	switch changes, reject := slices.Contains(st.Actions, ActionRequestChanges), slices.Contains(st.Actions, ActionReject); {
	case changes && reject:
		parts = append(parts, "Request changes and Reject need a note.")
	case changes:
		parts = append(parts, "Request changes needs a note.")
	case reject:
		parts = append(parts, "Reject needs a note.")
	}
	return strings.Join(parts, " ")
}

// revisions are the plans at the gate (none when the key may not read
// plans).
func revisions(st *itemstate.State) []itemstate.RevisionView {
	if st.Plan == nil {
		return nil
	}
	return st.Plan.Revisions
}

// stepper is where the item is in the workflow, as the dashboard's stage
// track shows it: the steps done (a tick), the current one (an arrow, or a
// warning when the work stopped there), the rest ("" = none: not started,
// discarded).
func stepper(s *surface, st *itemstate.State) string {
	steps := []string{"Plan", "Plan approval", "Implement", "Checks", "Review", "Sign-off"}
	var current int
	switch inbox.Status(st.Item.Status) {
	case inbox.StatusQueued, inbox.StatusPlanning:
		current = 0
	case inbox.StatusAwaitingApproval:
		current = 1
	case inbox.StatusApproved, inbox.StatusImplementing:
		current = 2
		if st.Run != nil {
			switch {
			case st.Run.Stage == runs.StageImplement && (st.Run.Phase == "checks" || st.Run.Phase == "fixing"):
				current = 3
			case st.Run.Stage == runs.StageReview || st.Run.Stage == runs.StageBranchReview || st.Run.Stage == runs.StageQA:
				current = 4
			}
		}
	case inbox.StatusInReview:
		current = 5
	case inbox.StatusBlocked, inbox.StatusFailed:
		current = stoppedStep(st)
	case inbox.StatusDone:
		current = len(steps)
	default:
		return ""
	}
	stopped := st.Gate == itemstate.GateBlocked || st.Gate == itemstate.GateFailed
	items := make([]string, 0, len(steps))
	for i, name := range steps {
		switch {
		case i < current:
			items = append(items, s.row(s.icon("check"), s.caption(name)))
		case i == current && stopped:
			items = append(items, s.row(s.icon("warning"), s.text("**"+name+" (stopped)**")))
		case i == current:
			items = append(items, s.row(s.icon("arrowForward"), s.text("**"+name+"**")))
		default:
			items = append(items, s.caption(name))
		}
	}
	return s.add("Row", Component{"children": items, "align": "center"})
}

// stoppedStep is the step a stopped item stopped at: the plan when no plan
// was approved (nothing implemented), else by what ran last.
func stoppedStep(st *itemstate.State) int {
	if st.Implementation == nil {
		if st.Plan != nil && slices.ContainsFunc(st.Plan.Revisions, func(r itemstate.RevisionView) bool { return r.Locked }) {
			return 2
		}
		return 0
	}
	if len(st.Reviews) > 0 && st.Implementation.Status == string(runs.StatusSucceeded) {
		return 4
	}
	if len(st.Implementation.Checks) > 0 && st.Implementation.Status != string(runs.StatusSucceeded) {
		return 3
	}
	return 2
}
