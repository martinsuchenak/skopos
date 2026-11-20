package itemreport

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/runs"
	"github.com/martinsuchenak/skopos/internal/usage"
)

// Markdown renders the report. The summary level fits a comment; the full
// level holds every plan, review and section verbatim. The same report
// gives the same text.
func Markdown(r *Report, level string) string {
	d := &doc{full: level != LevelSummary}
	d.header(r)
	d.outcome(r)
	d.brief(r)
	d.plan(r.Plan)
	d.questions(r)
	d.decisions(r)
	d.implementation(r)
	d.reviews(r)
	d.findings(r)
	d.asWritten(r)
	d.runs(r)
	return strings.TrimRight(d.b.String(), "\n") + "\n"
}

type doc struct {
	b    strings.Builder
	full bool
}

func (d *doc) w(format string, a ...any) { fmt.Fprintf(&d.b, format, a...) }

// h2 starts one of the report's own parts.
func (d *doc) h2(title string) { d.w("## %s\n\n", title) }

func (d *doc) header(r *Report) {
	title := r.Item.Title
	if title == "" {
		title = "Item " + r.Item.ID
	}
	d.w("# %s\n\n", oneLine(title))
	kind := "Item report"
	if !d.full {
		kind = "Item report, summary"
	}
	d.w("_%s · generated %s_\n\n", kind, stamp(r.GeneratedAt))

	d.w("- **Workspace:** %s\n", r.Item.WorkspaceID)
	switch {
	case r.Item.Ref != "" && r.Item.ExternalURL != "":
		d.w("- **Reference:** [%s](%s)\n", linkText(r.Item.Ref), r.Item.ExternalURL)
	case r.Item.Ref != "":
		d.w("- **Reference:** %s\n", oneLine(r.Item.Ref))
	case r.Item.ExternalURL != "":
		d.w("- **Reference:** <%s>\n", r.Item.ExternalURL)
	}
	d.w("- **Status:** %s\n", words(r.Item.Status))
	if r.Item.Branch != "" {
		line := code(r.Item.Branch)
		if r.Item.HeadSHA != "" {
			line += " at " + code(short(r.Item.HeadSHA))
		}
		d.w("- **Branch:** %s\n", line)
	}
	if r.Item.PullRequest != "" {
		d.w("- **Pull request:** <%s>\n", r.Item.PullRequest)
	}
	if len(r.Item.Needs) > 0 {
		d.w("- **Needs a worker with:** %s\n", strings.Join(r.Item.Needs, ", "))
	}
	d.w("- **Created:** %s\n", stamp(r.Item.CreatedAt))
	d.w("- **Last change:** %s\n", stamp(r.Item.UpdatedAt))
	d.w("- **Item:** %s\n\n", code(r.Item.ID))
}

func (d *doc) outcome(r *Report) {
	if len(r.Outcome) == 0 {
		return
	}
	d.h2("Outcome")
	for _, line := range r.Outcome {
		d.w("- %s\n", line)
	}
	d.w("\n")
}

func (d *doc) brief(r *Report) {
	if r.Brief == "" {
		return
	}
	d.h2("Brief")
	if !d.full {
		// The start of it: the work the summary is posted to has the rest.
		d.w("%s\n\n", clip(firstParagraph(r.Brief), 500))
		return
	}
	d.w("%s\n\n", nest(r.Brief, 2))
}

func (d *doc) plan(p *Plan) {
	if p == nil {
		return
	}
	d.h2("Plan")
	if p.Approval == nil {
		d.w("No plan is approved yet.\n\n")
	} else {
		line := fmt.Sprintf("Approved by %s on %s%s", p.Approval.By, stamp(p.Approval.At), through(p.Approval.Via))
		if p.RevisionNo > 0 {
			line += fmt.Sprintf(": revision %d", p.RevisionNo)
		} else if p.RevisionID != "" {
			line += ": revision " + code(p.RevisionID)
		}
		if p.BaseSHA != "" {
			line += ", on base commit " + code(short(p.BaseSHA))
		}
		d.w("%s.\n\n", line)
		if p.Approval.Reason != "" {
			d.w("**Why this plan:** %s\n\n", clipIf(!d.full, oneLine(p.Approval.Reason), 300))
		}
		d.notes(p.Approval.Notes)
		if p.Amended {
			d.w("That revision amends the plan approved before it; the steps are the amended ones.\n\n")
		}
	}

	if !d.full {
		switch {
		case len(p.Steps) > 0:
			for i, s := range p.Steps {
				d.w("%d. %s\n", i+1, oneLine(s))
			}
			d.w("\n")
		case p.Approved != nil:
			d.w("The plan's text is in the full report.\n\n")
		}
		if len(p.Others) > 0 {
			names := make([]string, 0, len(p.Others))
			for _, o := range p.Others {
				names = append(names, planName(o, p.Rounds))
			}
			word := "Not chosen"
			if p.Approval == nil {
				word = "Written so far"
			}
			d.w("%s: %s.\n\n", word, strings.Join(names, "; "))
		}
		return
	}

	if p.Approved != nil {
		d.w("### %s, approved\n\n%s\n\n", planName(*p.Approved, p.Rounds), nest(p.Approved.Markdown, 3))
	}
	// The steps, when the plan's own text is not the whole record: it is
	// not on the item, or a later revision amended it.
	if (p.Approved == nil || p.Amended) && len(p.Steps) > 0 {
		if p.Approved != nil {
			d.w("### The steps as amended\n\n")
		}
		for i, s := range p.Steps {
			d.w("%d. %s\n", i+1, oneLine(s))
		}
		d.w("\n")
	}
	for _, o := range p.Others {
		word := "not chosen"
		if p.Approval == nil {
			word = "not approved"
		}
		d.w("### %s, %s\n\n%s\n\n", planName(o, p.Rounds), word, nest(o.Markdown, 3))
	}
	for _, n := range p.Notes {
		heading := oneLine(n.Heading)
		if p.Rounds > 1 {
			heading += fmt.Sprintf(" (round %d)", n.Round)
		}
		d.w("### %s\n\n%s\n\n", heading, nest(n.Markdown, 3))
	}
}

// planName: "Candidate 2 · codex", with the round when there were several.
func planName(p PlanDoc, rounds int) string {
	name := oneLine(p.Title)
	if rounds > 1 {
		name += fmt.Sprintf(" (round %d)", p.Round)
	}
	return name
}

func (d *doc) questions(r *Report) {
	asked := len(r.FreeAnswers) > 0
	for _, set := range r.Questions {
		if len(set.Questions) > 0 || len(set.General) > 0 {
			asked = true
		}
	}
	if !asked {
		return
	}
	d.h2("Questions and answers")
	for _, set := range r.Questions {
		if d.full {
			d.w("### Asked by %s on %s\n\n", set.Asked, stamp(set.At))
		}
		for _, q := range set.Questions {
			if !d.full {
				if q.State == qa.StateSuperseded {
					continue
				}
				d.w("- %s (%s) → %s\n", oneLine(q.Text), set.Asked, d.answerLine(q))
				continue
			}
			d.w("%d. %s\n", q.N, oneLine(q.Text))
			if len(q.Options) > 0 {
				opts := make([]string, 0, len(q.Options))
				for i, o := range q.Options {
					opts = append(opts, fmt.Sprintf("(%c) %s", 'a'+rune(i%26), oneLine(o)))
				}
				d.w("   - Options: %s\n", strings.Join(opts, "; "))
			}
			d.w("   - %s\n", d.answerLine(q))
			for _, old := range q.Replaced {
				d.w("   - Corrected; before that: %s%s\n", oneLine(old.Text), who(old))
			}
		}
		for _, a := range set.General {
			d.w("- To the questions as a whole: %s%s\n", oneLine(a.Text), who(a))
		}
		if d.full {
			d.w("\n")
		}
	}
	if len(r.FreeAnswers) > 0 {
		if d.full {
			d.w("### Said without a question\n\n")
		}
		for _, a := range r.FreeAnswers {
			d.w("- %s%s\n", oneLine(a.Text), who(a))
		}
		if d.full {
			d.w("\n")
		}
	}
	if !d.full {
		d.w("\n")
	}
}

func (d *doc) answerLine(q Question) string {
	switch {
	case q.Answer != nil:
		if d.full {
			return "Answer: **" + oneLine(q.Answer.Text) + "**" + who(*q.Answer)
		}
		return "**" + oneLine(q.Answer.Text) + "**"
	case q.State == qa.StateSkipped:
		return "passed over at the gate"
	case q.State == qa.StateSuperseded:
		return "asked again later"
	case q.State == qa.StateAnswered, q.State == qa.StateAnsweredFreeText:
		return "answered in the notes"
	}
	return "not answered"
}

// who: " — root, 2026-11-15 10:00 UTC (slack)".
func who(a Answer) string {
	if a.By == "" {
		return ""
	}
	return " — " + a.By + ", " + stamp(a.At) + through(a.Via)
}

func (d *doc) decisions(r *Report) {
	if len(r.Decisions) == 0 {
		return
	}
	d.h2("Decisions")
	if r.DecisionsCut {
		d.w("The item has more decisions than a report reads; the oldest are not shown.\n\n")
	}
	for _, dec := range r.Decisions {
		line := stamp(dec.At) + " · " + decisionWords(dec) + decisionBy(dec)
		if s := subjectWords(dec); s != "" {
			line += ", " + s
		}
		if !d.full {
			if n := oneLine(dec.Notes); n != "" {
				line += ": " + clip(n, 200)
			}
			if dec.Reason != "" {
				line += "; why this plan: " + clip(oneLine(dec.Reason), 200)
			}
			d.w("- %s\n", line)
			continue
		}
		d.w("- %s\n", line)
		if dec.Reason != "" {
			d.w("\n  **Why this plan:** %s\n\n", oneLine(dec.Reason))
		}
		if dec.Notes != "" {
			d.w("\n%s\n\n", indent(quote(dec.Notes), "  "))
		}
	}
	d.w("\n")
}

// decisionBy names who decided: " by <actor> (<via>)"; for the server's
// automatic approval, who switched it on.
func decisionBy(d Decision) string {
	switch {
	case d.Action == ActionAutoHeld, d.Action == ActionNothingToDo:
		return ""
	case d.Via == audit.ViaAuto:
		if set := strings.TrimSuffix(strings.TrimPrefix(d.By, "auto (set by "), ")"); set != d.By {
			return ", switched on by " + set
		}
		return ""
	}
	return " by " + d.By + through(d.Via)
}

func decisionWords(d Decision) string {
	if d.Action == ActionApprove && d.Via == audit.ViaAuto {
		return "Plan approved automatically"
	}
	switch d.Action {
	case ActionQueue:
		return "Started"
	case ActionReview:
		return "Sent for review only"
	case ActionApprove:
		return "Plan approved"
	case ActionRequestChange:
		return "Changes requested"
	case ActionRetry:
		return "Started again"
	case ActionReject:
		return "Discarded"
	case ActionMarkDone:
		return "Signed off"
	case ActionSecondOpinion:
		return "Second opinion asked for"
	case ActionWaive:
		return "Finding waived"
	case ActionAnswer:
		return "Questions answered"
	case ActionCheckCommits:
		return "New commits sent to be checked"
	case ActionAutoHeld:
		return "Not approved automatically"
	case ActionNothingToDo:
		return "Closed: nothing to do"
	}
	return words(d.Action)
}

func subjectWords(d Decision) string {
	if d.Subject == "" {
		return ""
	}
	if d.Action == ActionApprove {
		rev, base, _ := strings.Cut(d.Subject, "+")
		out := "revision " + code(short(rev))
		if base != "" {
			out += " on base " + code(short(base))
		}
		return out
	}
	return "commit " + code(short(d.Subject))
}

func (d *doc) implementation(r *Report) {
	if len(r.Implementations) == 0 {
		return
	}
	d.h2("Implementation")
	list := r.Implementations
	if !d.full {
		if n := len(list) - 1; n > 0 {
			d.w("The newest of %d implementation runs:\n\n", len(list))
		}
		list = list[len(list)-1:]
	}
	for i, im := range list {
		if d.full {
			d.w("### Implementation %d · %s\n\n", i+1, stamp(im.At))
		}
		line := words(im.Status)
		if im.Runner != "" {
			line = im.Runner + ": " + line
		}
		if im.FailReason != "" {
			line += " (" + clip(oneLine(im.FailReason), 300) + ")"
		}
		if !d.full {
			line += ", " + stamp(im.At)
		}
		d.w("- %s\n", line)
		if im.AdoptFrom != "" {
			d.w("- Took in %s added to the branch after %s; no agent implemented anything\n", plural(len(im.Adopted), "commit"), code(short(im.AdoptFrom)))
			for _, c := range im.Adopted {
				line := code(short(c.SHA)) + " " + oneLine(c.Subject)
				if c.Author != "" {
					line += " (" + oneLine(c.Author) + ")"
				}
				d.w("  - %s\n", line)
			}
		}
		if im.HeadSHA != "" {
			line := "Commit " + code(short(im.HeadSHA))
			if im.Diffstat != "" {
				line += ": " + oneLine(im.Diffstat)
			}
			d.w("- %s\n", line)
		}
		if s := checkLine("Setup", im.Setup); s != "" {
			d.w("- %s\n", s)
		}
		if s := checkLine("Checks", im.Checks); s != "" {
			if im.FixRounds > 0 {
				s += fmt.Sprintf("; %s to get there", plural(im.FixRounds, "fix round"))
			}
			d.w("- %s\n", s)
		}
		if s := workOutcomes(im.Work); s != "" {
			d.w("- The implementer's verifications: %s\n", s)
		}
		switch {
		case im.PRURL != "":
			d.w("- Pull request: <%s>\n", im.PRURL)
		case im.Pushed != nil && *im.Pushed:
			d.w("- Pushed\n")
		case im.Pushed != nil:
			d.w("- Not pushed\n")
		}
		d.w("\n")
		if !d.full {
			continue
		}
		// An adopt run's summary only lists the commits named above.
		if im.Summary != "" && im.AdoptFrom == "" {
			d.w("%s\n\n", nest(im.Summary, 3))
		}
		d.workList("What the implementer verified", im.Work)
		for _, c := range append(append([]Check(nil), im.Setup...), im.Checks...) {
			if c.Passed || c.Output == "" {
				continue
			}
			d.w("The end of %s's output:\n\n%s\n\n", code(c.Name), fenced(c.Output))
		}
	}
}

// workList renders what a run says it did, one line each: what, the
// outcome, and what it saw.
func (d *doc) workList(title string, ws []runs.WorkEntry) {
	if len(ws) == 0 {
		return
	}
	d.w("**%s**\n\n", title)
	for _, e := range ws {
		line := oneLine(e.Name) + ": **" + workWord(e.Result) + "**"
		if e.Note != "" {
			line += " — " + oneLine(e.Note)
		}
		d.w("- %s\n", line)
	}
	d.w("\n")
}

// workCount: "13 scenarios: 12 passed, 1 skipped".
func workCount(ws []runs.WorkEntry, noun string) string {
	if len(ws) == 0 {
		return ""
	}
	return plural(len(ws), noun) + ": " + workOutcomes(ws)
}

// workOutcomes: "12 passed, 1 skipped".
func workOutcomes(ws []runs.WorkEntry) string {
	var order []string
	by := map[string]int{}
	for _, e := range ws {
		w := workWord(e.Result)
		if by[w] == 0 {
			order = append(order, w)
		}
		by[w]++
	}
	rank := func(w string) int {
		if i := slices.Index([]string{"passed", "failed", "skipped"}, w); i >= 0 {
			return i
		}
		return 3
	}
	slices.SortStableFunc(order, func(a, b string) int { return rank(a) - rank(b) })
	parts := make([]string, 0, len(order))
	for _, w := range order {
		parts = append(parts, fmt.Sprintf("%d %s", by[w], w))
	}
	return strings.Join(parts, ", ")
}

// workWord is a work entry's outcome as the report says it.
func workWord(result string) string {
	switch result {
	case "pass":
		return "passed"
	case "fail":
		return "failed"
	case "skipped":
		return "skipped"
	case "":
		return "not reported"
	}
	return oneLine(result)
}

// workNoun is what one entry of a review-like run's list is called.
func workNoun(stage string) string {
	if stage == runs.StageQA {
		return "scenario"
	}
	return "check"
}

// checkLine: "Checks: 3 passed, 1 failed (lint)".
func checkLine(label string, cs []Check) string {
	if len(cs) == 0 {
		return ""
	}
	passed := 0
	var failed []string
	for _, c := range cs {
		switch {
		case c.Passed:
			passed++
		case c.Stopped:
			failed = append(failed, oneLine(c.Name)+", stopped")
		default:
			failed = append(failed, oneLine(c.Name))
		}
	}
	out := fmt.Sprintf("%s: %d passed", label, passed)
	if len(failed) > 0 {
		out += fmt.Sprintf(", %d failed (%s)", len(failed), strings.Join(failed, "; "))
	}
	return out
}

func (d *doc) reviews(r *Report) {
	if len(r.Reviews) == 0 {
		return
	}
	d.h2("Reviews and QA")
	list := r.Reviews
	if !d.full {
		// The reviews of the newest reviewed commit.
		head := list[len(list)-1].HeadSHA
		var newest []Review
		for _, rv := range list {
			if rv.HeadSHA == head {
				newest = append(newest, rv)
			}
		}
		if len(newest) < len(list) && head != "" {
			d.w("Of commit %s, the newest reviewed (the full report has %s):\n\n", code(short(head)), plural(len(list)-len(newest), "earlier review"))
		}
		for _, rv := range newest {
			line := reviewName(rv) + ": " + verdict(rv)
			if s := workCount(rv.Work, workNoun(rv.Stage)); s != "" {
				line += " (" + s + ")"
			}
			d.w("- %s\n", line)
		}
		d.w("\n")
		return
	}
	for _, rv := range list {
		title := reviewName(rv)
		if rv.HeadSHA != "" {
			title += " · commit " + code(short(rv.HeadSHA))
		}
		d.w("### %s · %s\n\n", title, stamp(rv.At))
		d.w("Verdict: %s.\n\n", verdict(rv))
		if rv.Summary != "" {
			d.w("%s\n\n", nest(rv.Summary, 3))
		}
		listTitle := "What it checked"
		if rv.Stage == runs.StageQA {
			listTitle = "Scenarios"
		}
		d.workList(listTitle, rv.Work)
		for _, f := range rv.Findings {
			line := "**" + f.Severity + "** " + oneLine(f.Text)
			if f.File != "" {
				line += " (" + code(f.File) + ")"
			}
			d.w("- %s\n", line)
		}
		if len(rv.Findings) > 0 {
			d.w("\n")
		}
	}
}

// reviewName: "Review · security · claude", "Second opinion · codex".
func reviewName(rv Review) string {
	name := map[string]string{
		runs.StageReview: "Review", runs.StageBranchReview: "Branch review",
		runs.StageQA: "QA", runs.StageSecondOpinion: "Second opinion",
	}[rv.Stage]
	if name == "" {
		name = words(rv.Stage)
	}
	if rv.Focus != "" {
		name += " · " + rv.Focus
	}
	if rv.Runner != "" {
		name += " · " + rv.Runner
	}
	return name
}

func verdict(rv Review) string {
	var out string
	switch {
	case rv.Passed:
		out = "passed"
		if rv.Nits > 0 {
			out += ", " + plural(rv.Nits, "nit")
		}
	case rv.Blocking > 0:
		out = plural(rv.Blocking, "blocking finding")
		if rv.StillOpen > 0 {
			out += fmt.Sprintf(" (%d found again from an earlier review)", rv.StillOpen)
		}
		if rv.Nits > 0 {
			out += ", " + plural(rv.Nits, "nit")
		}
	default:
		out = "did not finish"
		if rv.FailReason != "" {
			out += ": " + clip(oneLine(rv.FailReason), 300)
		}
	}
	if rv.Advisory {
		out += " (advisory)"
	}
	return out
}

func (d *doc) findings(r *Report) {
	if len(r.Findings) == 0 {
		return
	}
	d.h2("Findings")
	if !d.full {
		if line := findingCounts(r.Findings); line != "" {
			d.w("%s\n\n", line)
		}
		listed := false
		for _, f := range r.Findings {
			if f.Earlier {
				continue
			}
			switch f.State {
			case findings.StateWaived, findings.StateStillOpen, findings.StateOpen, findings.StateFixed:
				d.w("- %s (%s, %s): %s\n", oneLine(f.Text), f.Severity, f.By, f.Outcome)
				listed = true
			}
		}
		if listed {
			d.w("\n")
		}
		return
	}
	d.w("| # | Severity | Raised by | Finding | What became of it |\n|---|---|---|---|---|\n")
	for _, f := range r.Findings {
		text := oneLine(f.Text)
		if f.File != "" {
			text += " (" + code(f.File) + ")"
		}
		d.w("| %d | %s | %s | %s | %s |\n", f.N, cell(f.Severity), cell(f.By), cell(text), cell(f.Outcome))
	}
	d.w("\n")
}

// asWritten: the reviewers', QA's and the implementer's sections of the
// item, verbatim (full level only).
func (d *doc) asWritten(r *Report) {
	if !d.full || len(r.Sections) == 0 {
		return
	}
	d.h2("Reviews, QA and amendments as written")
	for _, s := range r.Sections {
		d.w("### %s\n\n%s\n\n", oneLine(s.Heading), nest(s.Markdown, 3))
	}
}

func (d *doc) runs(r *Report) {
	if len(r.Runs) == 0 {
		return
	}
	d.h2("Who worked on it")
	if d.full {
		d.w("| Stage | Runner | Model | Provider | Contained by | Worker | Queued | Last change | Status | Tokens | Cost |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, run := range r.Runs {
			stage := words(run.Stage)
			if run.Focus != "" {
				stage += " · " + run.Focus
			}
			cost := ""
			if run.CostUSD != nil {
				cost = money(*run.CostUSD)
			}
			d.w("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", cell(stage), cell(run.Runner), cell(modelOf(run)), cell(run.Provider),
				cell(run.Executor), cell(run.WorkerID), stamp(run.QueuedAt), stamp(run.UpdatedAt), cell(words(run.Status)), tokenCell(run.Usage), cost)
		}
		d.w("\nA run records when it was queued and when it last changed, not when it started.\n\n")
	}
	if line := providers(r.Runs); line != "" {
		d.w("- Providers that received the code: %s\n", line)
	}
	// A run a worker took without saying which provider is behind its
	// runner: the list above is not the whole answer.
	unknown := 0
	for _, run := range r.Runs {
		if run.WorkerID != "" && run.Provider == "" {
			unknown++
		}
	}
	if unknown > 0 {
		d.w("- %s did not record a provider\n", plural(unknown, "run"))
	}
	d.w("- Runs: %d\n", len(r.Runs))
	if r.CostUSD != nil {
		d.w("- Cost as the runners reported it: %s\n", money(*r.CostUSD))
	}
	if line := tokenLine(r.Usage); line != "" {
		d.w("- Tokens as the runners reported them: %s\n", line)
		// The sum leaves out runs that recorded nothing: from a worker
		// older than the record, or ended before an agent started.
		missing := 0
		for _, run := range r.Runs {
			if run.WorkerID != "" && run.Usage.Empty() && finished(runs.Status(run.Status)) {
				missing++
			}
		}
		if missing > 0 {
			d.w("- %s recorded no usage: a worker older than the record, or no agent started\n", plural(missing, "run"))
		}
	}
	d.w("\n")
}

// modelOf is the model a run's runner said did the work, else the one its
// account names.
func modelOf(run Run) string {
	if len(run.ModelsUsed) > 0 {
		return strings.Join(run.ModelsUsed, ", ")
	}
	return run.Model
}

// tokenCell: "1.2M in · 56k out" (input with the cache's).
func tokenCell(u *runs.Usage) string {
	in, out := u.Tokens()
	if in == 0 && out == 0 {
		return ""
	}
	return usage.Compact(in) + " in · " + usage.Compact(out) + " out"
}

// tokenLine: "12M in (11M from the cache), 210k out (40k reasoning)".
func tokenLine(u *runs.Usage) string {
	in, out := u.Tokens()
	if in == 0 && out == 0 {
		return ""
	}
	line := usage.Compact(in) + " in"
	if c := u.Counts["cache_read_tokens"]; c > 0 {
		line += " (" + usage.Compact(c) + " from the cache)"
	}
	line += ", " + usage.Compact(out) + " out"
	if rt := u.Counts["reasoning_tokens"]; rt > 0 {
		line += " (" + usage.Compact(rt) + " reasoning)"
	}
	return line
}

// providers: "anthropic (claude: sonnet), openai (codex: gpt-5)".
func providers(rs []Run) string {
	seen := map[string]map[string]bool{}
	var order []string
	for _, run := range rs {
		if run.Provider == "" {
			continue
		}
		if seen[run.Provider] == nil {
			seen[run.Provider] = map[string]bool{}
			order = append(order, run.Provider)
		}
		via := run.Kind
		if m := modelOf(run); m != "" {
			if via != "" {
				via += ": "
			}
			via += m
		}
		if via != "" {
			seen[run.Provider][via] = true
		}
	}
	parts := make([]string, 0, len(order))
	for _, p := range order {
		var via []string
		for v := range seen[p] {
			via = append(via, v)
		}
		sort.Strings(via)
		if len(via) > 0 {
			p += " (" + strings.Join(via, ", ") + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// notes writes a person's notes as a quote.
func (d *doc) notes(text string) {
	if text == "" {
		return
	}
	if !d.full {
		d.w("> %s\n\n", clip(oneLine(text), 300))
		return
	}
	d.w("%s\n\n", quote(text))
}

func quote(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n")
}

func indent(text, by string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = by + l
	}
	return strings.Join(lines, "\n")
}

// nest moves the headings of text an agent or the author wrote below the
// report's own: the highest heading in it becomes level under+1. Headings
// already deeper stay, and nothing inside a code fence is touched. A
// heading written as underlined text ("Title" over "=====" or "-----")
// becomes a "#" heading first, so it moves with the others.
func nest(md string, under int) string {
	lines := atxHeadings(strings.Split(strings.TrimSpace(md), "\n"))
	levels := headingLevels(lines)
	lowest := 7
	for _, n := range levels {
		if n > 0 {
			lowest = min(lowest, n)
		}
	}
	shift := under + 1 - lowest
	if lowest == 7 || shift <= 0 {
		return strings.Join(lines, "\n")
	}
	for i, n := range levels {
		if n == 0 {
			continue
		}
		t := strings.TrimLeft(lines[i], " ")
		lines[i] = strings.Repeat("#", min(n+shift, 6)) + t[n:]
	}
	return strings.Join(lines, "\n")
}

// prose reports, per line, whether it is ordinary text: not inside a code
// fence, not a fence line and not indented as code.
func prose(lines []string) []bool {
	out := make([]bool, len(lines))
	var fence string
	for i, l := range lines {
		t := strings.TrimLeft(l, " ")
		if len(l)-len(t) > 3 {
			continue
		}
		if f := fenceOf(t); f != "" {
			switch {
			case fence == "":
				fence = f
			case f[0] == fence[0] && len(f) >= len(fence) && strings.TrimSpace(t[len(f):]) == "":
				fence = ""
			}
			continue
		}
		out[i] = fence == ""
	}
	return out
}

// headingLevels is, per line, the level of the "#" heading on it (0 = none).
func headingLevels(lines []string) []int {
	levels := make([]int, len(lines))
	for i, ok := range prose(lines) {
		if !ok {
			continue
		}
		t := strings.TrimLeft(lines[i], " ")
		n := 0
		for n < len(t) && t[n] == '#' {
			n++
		}
		if n >= 1 && n <= 6 && (len(t) == n || t[n] == ' ') {
			levels[i] = n
		}
	}
	return levels
}

// atxHeadings rewrites underlined headings as "#" headings: a line of text
// directly over a line of only "=" (level 1) or only "-" (level 2). Text
// that is a list item, a quote, a table row or a heading already is not a
// heading's text, so a rule under it stays a rule.
func atxHeadings(lines []string) []string {
	ok := prose(lines)
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if i+1 < len(lines) && ok[i] && ok[i+1] {
			text, under := strings.TrimSpace(lines[i]), strings.TrimSpace(lines[i+1])
			level := 0
			switch {
			case under != "" && strings.Trim(under, "=") == "":
				level = 1
			case under != "" && strings.Trim(under, "-") == "":
				level = 2
			}
			if level > 0 && text != "" && !strings.ContainsAny(text[:1], "#>-*+|") && !startsNumbered(text) &&
				strings.Trim(text, "=") != "" && strings.Trim(text, "-") != "" && (i == 0 || strings.TrimSpace(lines[i-1]) == "" || !ok[i-1] || headingLevelOf(lines[i-1]) > 0) {
				out = append(out, strings.Repeat("#", level)+" "+text)
				i++
				continue
			}
		}
		out = append(out, lines[i])
	}
	return out
}

// headingLevelOf is the "#" heading level of one line of ordinary text.
func headingLevelOf(line string) int { return headingLevels([]string{line})[0] }

// startsNumbered reports a numbered list item: "1. " or "1) ".
func startsNumbered(text string) bool {
	n := 0
	for n < len(text) && text[n] >= '0' && text[n] <= '9' {
		n++
	}
	return n > 0 && n+1 < len(text) && (text[n] == '.' || text[n] == ')') && text[n+1] == ' '
}

// firstParagraph is the first paragraph of the text that is not a heading,
// on one line; the text's first line without its "#" when it has no other.
func firstParagraph(md string) string {
	lines := atxHeadings(strings.Split(strings.TrimSpace(md), "\n"))
	levels := headingLevels(lines)
	var para []string
	for i, l := range lines {
		switch {
		case levels[i] > 0, strings.TrimSpace(l) == "":
			if len(para) > 0 {
				return oneLine(strings.Join(para, " "))
			}
		default:
			para = append(para, l)
		}
	}
	if len(para) > 0 {
		return oneLine(strings.Join(para, " "))
	}
	return oneLine(strings.TrimLeft(strings.TrimSpace(md), "# "))
}

// fenceOf is the run of backticks or tildes a fence line starts with.
func fenceOf(line string) string {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return ""
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return ""
	}
	return line[:n]
}

// fenced puts output in a code block whose fence the output cannot close.
func fenced(s string) string {
	fence := "```"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	return fence + "\n" + s + "\n" + fence
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// cell is text for a table cell.
func cell(s string) string { return strings.ReplaceAll(oneLine(s), "|", `\|`) }

// code is an inline code span the text cannot close.
func code(s string) string {
	s = oneLine(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "`") {
		return "`" + s + "`"
	}
	return "`` " + s + " ``"
}

// linkText is text for the inside of a link.
func linkText(s string) string {
	return strings.NewReplacer("[", `\[`, "]", `\]`).Replace(oneLine(s))
}

// words: "in_review" → "in review".
func words(s string) string { return strings.ReplaceAll(s, "_", " ") }

// clipIf clips s to n runes when cut is true.
func clipIf(cut bool, s string, n int) string {
	if cut {
		return clip(s, n)
	}
	return s
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04") + " UTC"
}

func money(v float64) string { return fmt.Sprintf("$%.2f", v) }
