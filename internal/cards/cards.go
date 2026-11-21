// Package cards shows an item to another system as a card: POST
// /api/inbox/card takes the other system's subject (its reference, title,
// link) and answers with an envelope — a status name the host maps to its
// own, whether a person must act, a one-line summary, a key that changes
// exactly when something new happened, and an A2UI 1.0 surface
// (a2ui.org, the basic catalog) the host draws with its own components.
// The host needs to know nothing about skopos's workflow; skopos needs to
// know nothing about the host. Built from the same state as GET
// /api/inbox/{id}/state (internal/itemstate), so the card and the state
// never disagree. For a key that may act for a person, the card also holds
// the inputs and buttons of the actions the item allows (actions.go), and
// the same endpoint carries them out.
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
	// Ref is the host's reference for its record, as the item's ref holds
	// it; the URL's ?ref= gives it too.
	Ref   string `json:"ref,omitempty"`
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
	// Actions: the key asking may act on the item (CanAct), so the card
	// draws inputs and a button per action the state allows (actionForm).
	Actions bool
	// Plans is the newest planning round as the item holds it (each plan in
	// full, the planners' questions, the comparison; inbox.PlansOf), read at
	// the plan gate. Nil = the steps of the plan revisions only.
	Plans *inbox.PlansView
}

// Limits of what a card shows.
const (
	maxReason      = 1500
	maxStep        = 1200
	maxQuestion    = 1000
	maxFinding     = 600
	maxSummaryLine = 160
	// A plan or the comparison in full, up to this: a card stays a card.
	maxPlanText = 24 << 10
)

// Build is the card: pure, the same input gives the same envelope.
func Build(in Input) *Envelope {
	st := in.State
	s := &surface{}
	if st == nil {
		env := &Envelope{Status: StatusNone, Summary: "Not sent to skopos", Key: "none"}
		head := s.headline("info", "Not sent to skopos", "No skopos item has the reference "+code(in.Subject.Ref)+".")
		env.A2UI = s.createSurface(surfaceID(in.Subject.Ref), []string{head}, nil)
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
		body = append(body, s.headline("info", "Not started", "The item is in skopos and waits to be sent to the agents."))
	case StatusAgent:
		env.Summary, body = agent(s, st)
	case StatusPlanApproval:
		env.Summary, body = planApproval(s, st, in.NothingToDo, in.Plans)
	case StatusBlocked, StatusFailed:
		env.Summary, body = stopped(s, st, env.Status, in.Actions && tickableFindings(st))
	case StatusSignOff:
		env.Summary, body = signOff(s, st, in.Actions && tickableFindings(st))
	case StatusDone:
		if inbox.Status(st.Item.Status) == inbox.StatusConverted {
			env.Summary, body = "Done: converted into a plan", []string{s.headline("check", "Done", "The item was converted into a plan.")}
		} else {
			env.Summary, body = done(s, st, in.Outcome)
		}
	case StatusDiscarded:
		env.Summary = "Discarded"
		body = append(body, s.headline("close", "Discarded", "Nothing more happens to this item."))
	case StatusNothingToDo:
		env.Summary = clip("Closed: nothing to do — "+in.NothingToDo, maxSummaryLine)
		body = append(body, s.headline("info", "Closed: nothing to do", "The planner found nothing to do."),
			s.section("Why", s.caption(clip(in.NothingToDo, maxReason))))
	}
	env.Key = key(st, env.Status, in.NothingToDo)
	if steps := stepper(s, st); steps != "" {
		body = append([]string{steps}, body...)
	}
	var f form
	if in.Actions {
		f = actionForm(s, st, env.Status, env.Key)
	}
	if len(f.body) == 0 || !answerable(env.Status) {
		// Without inputs for them, the open questions are text.
		body = append(body, questions(s, st.Questions)...)
	}
	body = append(body, f.body...)
	env.A2UI = s.createSurface(surfaceID(st.Item.ID), body, f.dataModel)
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

// prName names a pull request in a line of text: its number when the address
// ends in one. Text carries no links (A2UI's basic catalog), so the address is
// shown as text where a person needs it.
func prName(url string) string {
	i := strings.LastIndex(url, "/")
	if n := url[i+1:]; i >= 0 && n != "" && strings.Trim(n, "0123456789") == "" {
		return "pull request #" + n
	}
	return "pull request"
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
		return "Skopos is working on it", []string{s.headline("refresh", "Skopos is working on it", "")}
	}
	what := stageWord(st.Run.Stage)
	sub := ""
	switch {
	case st.Run.Status == string(runs.StatusQueued):
		sub = "Queued: " + what + ", waiting for a worker."
	case st.Run.Runner != "":
		sub = st.Run.Runner + " is on it."
	}
	return "Skopos is working on it: " + what, []string{s.headline("refresh", "Skopos is working on it: "+what, sub)}
}

func planApproval(s *surface, st *itemstate.State, nothingToDo string, round *inbox.PlansView) (string, []string) {
	if nothingToDo != "" {
		return clip("Waiting for you: the planner found nothing to do — "+nothingToDo, maxSummaryLine),
			[]string{s.headline("info", "The planner found nothing to do", "Discard the item, or send it back with what to do."),
				s.section("Why", s.caption(clip(nothingToDo, maxReason)))}
	}
	revs := revisions(st)
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
	var facts []string
	switch len(revs) {
	case 0:
	case 1:
		facts = append(facts, plural(len(revs[0].Steps), "step", "steps"))
	default:
		facts = append(facts, plural(len(revs), "plan", "plans")+" to choose from")
	}
	if n := len(st.Questions); n > 0 {
		facts = append(facts, plural(n, "open question", "open questions"))
	}
	if round != nil && round.Rounds > 1 {
		facts = append(facts, fmt.Sprintf("planning round %d", round.Round))
	}
	body := []string{s.headline("notifications", "Plan ready for approval", strings.Join(facts, " · "))}
	if len(revs) == 0 {
		return summary, append(body, s.section("Plan", s.caption("The plan is not readable with this key.")))
	}
	if round != nil && round.Comparison != nil && len(revs) > 1 {
		body = append(body, s.section("Comparison", s.text(fullText(round.Comparison.Markdown))))
	}
	if len(revs) == 1 {
		return summary, append(body, s.section("Plan", planBody(s, revs[0], docFor(round, revs[0].RevisionID))...))
	}
	ts := make([]tab, 0, len(revs))
	for i, r := range revs {
		ts = append(ts, tab{title: planTitle(i, r), child: s.column(planBody(s, r, docFor(round, r.RevisionID))...)})
	}
	return summary, append(body, s.section("Plans", s.tabs(ts)))
}

// planTitle is a plan's name on the card: its number at the gate (as the
// picker and the comparison number them) and who wrote it; the synthesis is
// named as the merge of the others.
func planTitle(i int, r itemstate.RevisionView) string {
	title := fmt.Sprintf("Plan %d", i+1)
	switch r.Label {
	case "":
	case "synthesis":
		title += " — merged from the others"
	default:
		title += " — " + r.Label
	}
	return title
}

// docFor is the round's plan document of a revision (nil = none: the item
// holds no section for it).
func docFor(round *inbox.PlansView, revisionID string) *inbox.PlanDoc {
	if round == nil {
		return nil
	}
	for i := range round.Plans {
		if round.Plans[i].RevisionID == revisionID && revisionID != "" {
			return &round.Plans[i]
		}
	}
	if len(round.Plans) == 1 {
		return &round.Plans[0]
	}
	return nil
}

// planBody is a plan on the card: the plan as its planner wrote it, with the
// questions it asked, else (no document) the revision's steps.
func planBody(s *surface, r itemstate.RevisionView, doc *inbox.PlanDoc) []string {
	if doc == nil || strings.TrimSpace(doc.Markdown) == "" {
		return []string{s.text(steps(r))}
	}
	parts := []string{s.text(fullText(withoutRevisionLine(doc.Markdown)))}
	if q := strings.TrimSpace(doc.QuestionsMarkdown); q != "" {
		parts = append(parts, s.text("##### Questions this planner asked"), s.text(fullText(q)))
	}
	return parts
}

// withoutRevisionLine drops the "**Revision:** <id>" line a plan section
// starts with: the item's bookkeeping, not part of the plan a person reads.
// Only the first such line outside code, so a plan's own text keeps its
// lines.
func withoutRevisionLine(md string) string {
	lines := strings.Split(md, "\n")
	var f fences
	for i, l := range lines {
		if !f.step(l) && strings.HasPrefix(strings.TrimSpace(l), "**Revision:**") {
			return strings.Join(append(lines[:i:i], lines[i+1:]...), "\n")
		}
	}
	return md
}

// fullText is a plan's or the comparison's text for a card: its headings
// moved below the card's own (a section heading is level 4) and the text
// cut at maxPlanText, saying so (a code block the cut falls in is closed
// first).
func fullText(md string) string {
	md = nestHeadings(strings.TrimSpace(md), 5)
	if len(md) > maxPlanText {
		md = clip(md, maxPlanText)
		var f fences
		for _, l := range strings.Split(md, "\n") {
			f.step(l)
		}
		if f.open != "" {
			md += "\n" + f.open
		}
		md += "\n\n_The rest is in skopos (Open)._"
	}
	return md
}

// fences follows Markdown's fenced code blocks line by line (``` or ~~~; a
// block ends only at the marker that opened it).
type fences struct{ open string }

// step takes the next line and reports whether it is code: inside a block,
// or a fence line.
func (f *fences) step(l string) bool {
	t := strings.TrimSpace(l)
	for _, m := range []string{"```", "~~~"} {
		if !strings.HasPrefix(t, m) {
			continue
		}
		switch f.open {
		case "":
			f.open = m
			return true
		case m:
			f.open = ""
			return true
		}
	}
	return f.open != ""
}

// nestHeadings moves Markdown's ATX headings down so the highest becomes
// level base (at most 6), leaving code blocks alone.
func nestHeadings(md string, base int) string {
	lines := strings.Split(md, "\n")
	level := func(l string) int {
		n := 0
		for n < len(l) && l[n] == '#' {
			n++
		}
		if n == 0 || n > 6 || (n < len(l) && l[n] != ' ') {
			return 0
		}
		return n
	}
	top := 0
	var f fences
	for _, l := range lines {
		if n := level(l); !f.step(l) && n > 0 && (top == 0 || n < top) {
			top = n
		}
	}
	if top == 0 || top >= base {
		return md
	}
	f = fences{}
	for i, l := range lines {
		if n := level(l); !f.step(l) && n > 0 {
			lines[i] = strings.Repeat("#", min(6, n+base-top)) + l[n:]
		}
	}
	return strings.Join(lines, "\n")
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

// stopped is a blocked or failed item; ticked says the open findings are
// drawn as tick boxes below.
func stopped(s *surface, st *itemstate.State, status string, ticked bool) (string, []string) {
	word, icon := "Blocked", "warning"
	if status == StatusFailed {
		word, icon = "Failed", "error"
	}
	reason := clip(st.Reason, maxReason)
	summary := word
	if l := firstLine(reason); l != "" {
		summary += ": " + l
	}
	body := []string{s.headline(icon, word, "The work stopped and waits for you.")}
	if reason != "" {
		body = append(body, s.section("Why it stopped", s.caption(reason)))
	}
	if len(openFindings(st)) > 0 && !ticked {
		body = append(body, findingsList(s, st))
	}
	return clip(summary, maxSummaryLine), body
}

// signOff is the sign-off gate; ticked says the open findings are drawn as
// tick boxes below, so they are not listed here as well.
func signOff(s *surface, st *itemstate.State, ticked bool) (string, []string) {
	var sub []string
	impl := st.Implementation
	if impl != nil {
		if impl.PRURL != "" {
			sub = append(sub, prName(impl.PRURL))
		}
		if impl.HeadSHA != "" {
			sub = append(sub, "commit "+code(short(impl.HeadSHA)))
		}
	}
	body := []string{s.headline("visibility", "Ready for sign-off", strings.Join(sub, " · "))}
	var facts []string
	if impl != nil && impl.PRURL != "" {
		facts = append(facts, "- Pull request: "+impl.PRURL)
	}
	if impl != nil && len(impl.Checks) > 0 {
		facts = append(facts, "- "+checksLine(impl.Checks))
	}
	for _, r := range st.Reviews {
		facts = append(facts, "- "+reviewLine(r))
	}
	if len(facts) > 0 {
		body = append(body, s.section("Checks and reviews", s.text(strings.Join(facts, "\n"))))
	}
	open := openFindings(st)
	if len(open) > 0 && !ticked {
		body = append(body, findingsList(s, st))
	}
	summary := "Branch ready for sign-off"
	if n := blocking(open); n > 0 {
		summary += " — " + plural(n, "blocking finding", "blocking findings") + " open"
	}
	return summary, body
}

// findingsList is the open findings as a section of text.
func findingsList(s *surface, st *itemstate.State) string {
	var b strings.Builder
	for _, f := range openFindings(st) {
		fmt.Fprintf(&b, "- **%s** %s", f.Severity, clip(f.Text, maxFinding))
		if f.File != "" {
			fmt.Fprintf(&b, " (%s)", code(f.File))
		}
		b.WriteString("\n")
	}
	return s.section("Findings still open", s.text(strings.TrimSpace(b.String())))
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
	sub := []string{"Signed off"}
	summary := "Done"
	if out != nil {
		switch out.State {
		case inbox.OutcomeMerged:
			sub = append(sub, "the pull request was merged")
			summary += ", merged"
		case inbox.OutcomeClosed:
			sub = append(sub, "the pull request was closed without merging")
			summary += ", pull request closed"
		}
	}
	if impl := st.Implementation; impl != nil && impl.PRURL != "" {
		sub = append(sub, prName(impl.PRURL)+": "+impl.PRURL)
	}
	return summary, []string{s.headline("check", "Done", strings.Join(sub, " · "))}
}

// questions are the open questions as text, a section per set: who asks,
// then the questions with their options.
func questions(s *surface, qs []itemstate.QuestionView) []string {
	var out []string
	for _, set := range questionSets(qs) {
		var b strings.Builder
		for i, q := range set {
			fmt.Fprintf(&b, "%d. %s\n", number(q, i+1), indent(clip(q.Text, maxQuestion), "   "))
			for j, o := range q.Options {
				fmt.Fprintf(&b, "   - (%c) %s\n", 'a'+j, indent(clip(o, maxQuestion), "     "))
			}
		}
		parts := []string{}
		if set[0].Blocking {
			parts = append(parts, s.caption("The work waits for the answers."))
		}
		parts = append(parts, s.text(strings.TrimSpace(b.String())))
		out = append(out, s.section("Questions — "+asker(set[0]), parts...))
	}
	return out
}

// questionSets are the questions by set, in order.
func questionSets(qs []itemstate.QuestionView) [][]itemstate.QuestionView {
	var sets [][]itemstate.QuestionView
	for _, q := range qs {
		if n := len(sets); n > 0 && sets[n-1][0].SetID == q.SetID {
			sets[n-1] = append(sets[n-1], q)
			continue
		}
		sets = append(sets, []itemstate.QuestionView{q})
	}
	return sets
}

// number is the question's number as asked, so the questions still open
// keep the numbers the asker gave them (count = its place on the card, for a
// state without numbers).
func number(q itemstate.QuestionView, count int) int {
	if q.N > 0 {
		return q.N
	}
	return count
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
