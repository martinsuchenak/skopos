package cmd

import (
	"context"
	"errors"
	"fmt"
	"github.com/martinsuchenak/skopos/internal/plans"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/notify"
	"github.com/martinsuchenak/skopos/internal/runs"
)

// slackRelay is the Slack command relay, ported from the live-proven
// agent-trial executor (agent.go applyCommand). It adapts Martin's replies
// onto the approver-gated inbox workflow actions and formats the outbound
// gate notifications, one thread per item.
//
// Security model (design doc §2): the relay acts as Martin after the Slack
// provider has checked his user id — the gate rests on his Slack account.
// Every action is audit-logged via=slack under the slack-relay principal.
type slackRelay struct {
	inbox     *inbox.Service
	runs      *runs.Service
	defaultWS string
	// resolveWS maps a workspace selector from a `new <ws>: ...` message to
	// a registered workspace id (nil = only the default workspace).
	resolveWS  func(selector string) (string, bool)
	notifier   *notify.Notifier
	rebindHook func(ts, itemID string) // tests; nil in production
	// revisions reads plan revisions: a plan-ready message shows one, and
	// an `approve` reply approves exactly that one (M2).
	revisions planRevisions
	log       notify.Logger // nil = silent (tests)

	mu      sync.Mutex
	pending map[string][]pendingReply // itemID -> replies waiting for an admissible status
	// shown is the plan revision the latest plan-ready message showed per
	// item. In memory: after a restart the relay cannot know what Martin
	// read, so an approve then re-posts the current plan (fail closed).
	shown map[string][]shownRev
}

// planRevisions is the slice of the plans service the relay needs.
type planRevisions interface {
	LatestRevision(ctx context.Context, planID string) (*plans.Revision, error)
	OpenCandidates(ctx context.Context, planID string) ([]plans.Revision, error)
}

// shownRev is one revision a plan-ready message showed: the plan revision,
// or candidate N of a multi-agent round (Label names its planner).
type shownRev struct{ ID, Label string }

type pendingReply struct{ text string }

// SetNotifier wires the outbound side (setter breaks the construction
// cycle: the notifier is built with this relay, then handed back).
func (r *slackRelay) SetNotifier(n *notify.Notifier) { r.notifier = n }

// relayCtx is the in-process principal the relay acts as. Root passes the
// workspace scope checks (the relay serves every workspace); the KeyID
// keeps the audit trail honest about the actor.
func relayCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{
		Root: true, KeyID: "slack-relay", Name: "slack", Approver: true,
	})
}

// A command is the bare word (optionally ending in . or !) or `word: args`.
// Anything else — "Done with lunch", "approve? not sure", "reject, wait" —
// is conversation, not a command (review M1: the optional separator turned
// casual replies into irreversible actions).
// Groups: 1 the command, 2 an optional selector (a plan candidate: its
// number or label — `approve 2`, `revise glm: notes`), 3 the notes.
var reCmd = regexp.MustCompile(`(?is)^\s*([a-z]+)(?:\s+([a-z0-9@._-]+))?(?:\s*[.!]?\s*|\s*:\s*(.*))$`)

// reWord is the leading word of a reply that did not parse as a command —
// used only to hint at the syntax when it names one.
var reWord = regexp.MustCompile(`(?i)^\s*([a-z]+)`)

// `new: <what>` or `new <what>`; "news: ..." / "newer ..." are not captures.
var reNew = regexp.MustCompile(`(?is)^\s*new(?:\s*:\s*|\s+)(.+)$`)
var reSelector = regexp.MustCompile(`(?s)^([A-Za-z0-9._/~+-]+):\s+(.+)$`)

// reHashtag: `#name` words in a capture become item tags — the skill tag
// hints the planner unions into its selection (capability profiles §3).
var reHashtag = regexp.MustCompile(`(?:^|\s)#([a-z0-9][a-z0-9._-]{0,63})\b`)

// captureTags is slack-new plus the capture's hashtags (lowercased, unique).
// #panel and #solo are the item's agents switch (captureAgents), not tags.
func captureTags(text string) []string {
	tags := []string{"slack-new"}
	seen := map[string]bool{"slack-new": true, inbox.AgentsPanel: true, inbox.AgentsSolo: true}
	for _, m := range reHashtag.FindAllStringSubmatch(strings.ToLower(text), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			tags = append(tags, m[1])
		}
	}
	return tags
}

// captureAgents: `#panel` / `#solo` in a capture set the item's
// multi-agent switch (the last one wins); "" = the repository default.
func captureAgents(text string) string {
	agents := ""
	for _, m := range reHashtag.FindAllStringSubmatch(strings.ToLower(text), -1) {
		if m[1] == inbox.AgentsPanel || m[1] == inbox.AgentsSolo {
			agents = m[1]
		}
	}
	return agents
}

// agentsWord describes an agents switch for messages.
func agentsWord(agents string) string {
	switch agents {
	case inbox.AgentsPanel:
		return "the agent panel"
	case inbox.AgentsSolo:
		return "one agent"
	}
	return "the repository default"
}

const slackHelp = "*skopos commands* — reply in an item's thread:\n" +
	"• `new <workspace>: <what>` — capture into a specific workspace (suffix match, e.g. `new skopos: ...`)\n" +
	"• `approve` / `lgtm` [`: <notes>`] — approve the plan at the gate\n" +
	"• `revise: <notes>` — request a re-plan\n" +
	"• `changes: <notes>` — request changes on a finished branch\n" +
	"• `done` — approve the review, mark the item done\n" +
	"• `retry: <answer>` — answer a blocker and requeue\n" +
	"• `reject` — discard the item\n" +
	"• `panel` / `solo` / `default` — the item's plan, review and QA stages use the repository's agent panel, one agent, or the repository default (from the next stage)\n" +
	"Top-level: `new: <what to do>` creates and queues an item; add `#panel` or `#solo` to choose its agents."

// TopLevel handles threadless DMs: capture and help. ts is Martin's own
// message — a created item's thread binds to it, so his next reply in that
// thread is already in the right place.
func (r *slackRelay) TopLevel(ctx context.Context, text, ts string) bool {
	if strings.EqualFold(strings.TrimSpace(text), "help") {
		r.post(ctx, "", slackHelp)
		return true
	}
	m := reNew.FindStringSubmatch(text)
	if m == nil {
		r.post(ctx, "", "Say `new: <what to do>` to create an item, or `help` for commands.")
		return true
	}
	what := strings.TrimSpace(m[1])
	ws := r.defaultWS
	// `new <workspace>: <what>` — the selector must resolve (exact id or a
	// unique "/suffix" of one); unknown prefixes stay part of the title so
	// `new fix: the login` keeps working.
	if sel := reSelector.FindStringSubmatch(what); sel != nil && r.resolveWS != nil {
		if id, ok := r.resolveWS(sel[1]); ok {
			ws = id
			what = strings.TrimSpace(sel[2])
		}
	}
	if ws == "" {
		r.post(ctx, "", "⚠️ Capture is disabled: SKOPOS_SLACK_DEFAULT_WORKSPACE is not set on the server.")
		return true
	}
	item, err := r.inbox.CreateItem(relayCtx(), inbox.CreateInput{
		WorkspaceID: ws, Title: truncate(what, 80),
		Content:       what + "\n\n_Created from Slack (`new:`), " + time.Now().UTC().Format("2006-01-02 15:04") + " UTC._",
		Tags:          captureTags(what),
		AuthorAgentID: "slack",
		Agents:        captureAgents(what),
	})
	if err != nil {
		r.post(ctx, "", "⚠️ Could not create the item: "+firstLine(err.Error()))
		return true
	}
	if _, qErr := r.inbox.Queue(relayCtx(), item.ID, audit.ViaSlack); qErr != nil {
		r.post(ctx, "", "⚠️ Created but not queued: "+firstLine(qErr.Error()))
	}
	r.bindThread(ctx, item.ID, ts)
	agentsNote := ""
	if item.Agents != "" {
		agentsNote = " · " + agentsWord(item.Agents)
	}
	r.post(ctx, ts, "📥 Queued in `"+ws+"`"+agentsNote+" · planning starts within a minute. The plan will come back in this thread.")
	return true
}

// Apply maps a thread reply onto the item's actions. Returns true when the
// command was recognized — including when it is buffered until the current
// run finishes (Socket Mode delivers once; buffering is how the mid-run
// guard survives). The outcome is posted in-thread either way.
func (r *slackRelay) Apply(ctx context.Context, itemID, text string) bool {
	m := reCmd.FindStringSubmatch(text)
	if m == nil || !isCommand(strings.ToLower(m[1])) {
		// Not a command; leave un-acked. When it LEADS with a command word
		// ("done with lunch", "approve? ..."), say how to issue the real
		// one rather than silently ignoring what may have been meant.
		if w := reWord.FindStringSubmatch(text); w != nil && isCommand(strings.ToLower(w[1])) {
			if item, err := r.inbox.GetItem(relayCtx(), itemID); err == nil {
				word := strings.ToLower(w[1])
				r.post(ctx, item.SlackThreadTS, "🤔 Not applied — a command is the bare word (`"+word+"`) or `"+word+": <notes>` on its own.")
			}
		}
		return false
	}
	cmd, sel, arg := strings.ToLower(m[1]), strings.ToLower(strings.TrimSpace(m[2])), strings.TrimSpace(m[3])
	if sel != "" && cmd != "approve" && cmd != "approved" && cmd != "lgtm" && cmd != "revise" {
		// Only approve/revise take a selector; anything else with a word
		// after it is prose, not a command.
		return false
	}
	item, err := r.inbox.GetItem(relayCtx(), itemID)
	if err != nil {
		return false
	}
	// The agents switch is a setting, not a workflow action: it applies
	// at once (never buffered) and takes effect at the next stage.
	if isAgentsCommand(cmd) {
		return r.setAgents(ctx, item, cmd)
	}
	if !inbox.IsWorkflowStatus(item.Status) {
		r.post(ctx, item.SlackThreadTS, "⚠️ The item is "+statusWord(item.Status)+" — no workflow actions apply.")
		return true
	}
	// Mid-run guard: a reply that would pull the worktree out from under a
	// running agent waits for the run to finish (agent-trial semantics).
	if item.Status == inbox.StatusPlanning || item.Status == inbox.StatusImplementing ||
		item.Status == inbox.StatusQueued || item.Status == inbox.StatusApproved {
		// Approvals are never buffered: a buffered approve/done would apply
		// the instant the gate is reached — approving a plan or a head
		// commit Martin has not seen (review H2). They wait for the gate
		// message instead; the non-advancing replies still buffer.
		if isApproval(cmd) {
			r.post(ctx, item.SlackThreadTS, "⚠️ `"+cmd+"` applies at the gate — the item is "+statusWord(item.Status)+
				". Reply to the gate message once it arrives.")
			return true
		}
		r.buffer(itemID, text)
		r.post(ctx, item.SlackThreadTS, "⏳ Got it — the item is mid-run; I'll apply it when the run finishes.")
		return true
	}
	return r.applyCommand(ctx, item, cmd, sel, arg)
}

// applyCommand runs one parsed command against the current item state.
func (r *slackRelay) applyCommand(ctx context.Context, item *inbox.Item, cmd, sel, arg string) bool {
	relayCtx := relayCtx()
	thread := item.SlackThreadTS
	fail := func(err error) bool {
		r.post(ctx, thread, "⚠️ Command failed: "+truncate(err.Error(), 300))
		return true // consumed: retrying a service refusal verbatim helps nobody
	}
	switch cmd {
	case "approve", "approved", "lgtm":
		if item.Status != inbox.StatusAwaitingApproval {
			r.post(ctx, thread, "⚠️ Nothing to approve — the item is "+statusWord(item.Status)+".")
			return true
		}
		// Approve exactly the revision the plan-ready message showed (M2);
		// with several candidates, the one the reply names.
		shown := r.shownRevisions(item.ID)
		if len(shown) == 0 {
			r.post(ctx, thread, "⚠️ I can't tell which plan revision you read (the relay restarted since the plan-ready message). The current plan follows — reply `approve` to it.")
			r.postPlanReady(ctx, item.ID)
			return true
		}
		revID, msg := pickShown(shown, sel)
		if revID == "" {
			r.post(ctx, thread, "⚠️ "+msg)
			return true
		}
		if _, err := r.inbox.Approve(relayCtx, item.ID, inbox.ApproveInput{Notes: arg, Via: audit.ViaSlack, RevisionID: revID}); err != nil {
			if errors.Is(err, inbox.ErrStaleRevision) || errors.Is(err, plans.ErrStaleRevision) {
				r.post(ctx, thread, "⚠️ The plan changed since the message you replied to. The current plan follows — approve that one.")
				r.postPlanReady(ctx, item.ID)
				return true
			}
			return fail(err)
		}
		return true
	case "revise":
		if item.Status != inbox.StatusAwaitingApproval {
			r.post(ctx, thread, "⚠️ revise applies at the gate — the item is "+statusWord(item.Status)+".")
			return true
		}
		// `revise 2: notes` reworks candidate 2 with one planner; a bare
		// `revise: notes` re-plans everything.
		reviseID := ""
		if sel != "" {
			shown := r.shownRevisions(item.ID)
			if len(shown) < 2 {
				r.post(ctx, thread, "⚠️ This plan has one revision — reply `revise: <notes>`.")
				return true
			}
			id, msg := pickShown(shown, sel)
			if id == "" {
				r.post(ctx, thread, "⚠️ "+msg)
				return true
			}
			reviseID = id
		}
		if _, err := r.inbox.RequestChangesOn(relayCtx, item.ID, arg, audit.ViaSlack, reviseID); err != nil {
			return fail(err)
		}
		return true
	case "changes":
		if item.Status != inbox.StatusInReview {
			r.post(ctx, thread, "⚠️ changes applies to a finished branch — the item is "+statusWord(item.Status)+".")
			return true
		}
		if _, err := r.inbox.RequestChanges(relayCtx, item.ID, arg, audit.ViaSlack); err != nil {
			return fail(err)
		}
		return true
	case "done":
		if item.Status != inbox.StatusInReview {
			r.post(ctx, thread, "⚠️ done applies to a finished branch — the item is "+statusWord(item.Status)+".")
			return true
		}
		head := r.reviewHead(item.ID)
		if head == "" {
			r.post(ctx, thread, "⚠️ No head commit recorded for the review — mark done from the dashboard.")
			return true
		}
		if _, err := r.inbox.MarkDone(relayCtx, item.ID, head, audit.ViaSlack); err != nil {
			return fail(err)
		}
		return true
	case "retry":
		if item.Status != inbox.StatusBlocked && item.Status != inbox.StatusFailed {
			r.post(ctx, thread, "⚠️ Nothing to retry — the item is "+statusWord(item.Status)+".")
			return true
		}
		if _, err := r.inbox.Retry(relayCtx, item.ID, arg, audit.ViaSlack); err != nil {
			return fail(err)
		}
		return true
	case "reject":
		if _, err := r.inbox.Reject(relayCtx, item.ID, arg, audit.ViaSlack); err != nil {
			return fail(err)
		}
		return true
	}
	return false
}

// setAgents applies a panel / solo / default reply to the item's switch.
func (r *slackRelay) setAgents(ctx context.Context, item *inbox.Item, cmd string) bool {
	value := cmd
	if cmd == "default" {
		value = ""
	}
	if err := r.inbox.UpdateItem(relayCtx(), item.ID, inbox.UpdateInput{Agents: &value, Via: audit.ViaSlack}); err != nil {
		r.post(ctx, item.SlackThreadTS, "⚠️ Command failed: "+truncate(err.Error(), 300))
		return true
	}
	msg := "👥 Plan, review and QA stages of this item now use " + agentsWord(value) + "."
	if item.Status == inbox.StatusPlanning || item.Status == inbox.StatusImplementing {
		msg += " A stage already running keeps its agents."
	}
	r.post(ctx, item.SlackThreadTS, msg)
	return true
}

// ItemForThread re-resolves a thread after a restart (the provider's
// in-memory map is empty then; the binding persists on the item).
func (r *slackRelay) ItemForThread(parent string) string {
	item, err := r.inbox.ItemForSlackThread(context.Background(), parent)
	if err != nil {
		return ""
	}
	return item.ID
}

// --- outbound: the gate notifications ---

// NotifyItem formats and posts the gate message for a status change, then
// drains the item's buffered replies (a finished run may admit them).
func (r *slackRelay) NotifyItem(ctx context.Context, itemID string, from, to inbox.Status) {
	if to == inbox.StatusAwaitingApproval {
		r.postPlanReady(ctx, itemID)
		r.drain(ctx, itemID)
		return
	}
	msg := r.formatGate(itemID, from, to)
	if msg == "" {
		return
	}
	r.postInThread(ctx, itemID, msg)
	r.drain(ctx, itemID)
}

// postInThread posts msg in the item's thread (starting it if needed) and
// reports whether Slack accepted it.
func (r *slackRelay) postInThread(ctx context.Context, itemID, msg string) bool {
	item, err := r.inbox.GetItem(context.Background(), itemID)
	if err != nil || r.notifier == nil {
		return false
	}
	if item.SlackThreadTS == "" {
		ts := r.notifier.Notify(ctx, "", msg)
		if ts != "" {
			r.bindThread(ctx, itemID, ts)
		}
		return ts != ""
	}
	return r.notifier.Notify(ctx, item.SlackThreadTS, msg) != ""
}

// postPlanReady posts the plan at the gate — the revision's own steps, not
// prose scraped from the item — and records which revision was shown, so
// the `approve` reply binds to exactly it.
func (r *slackRelay) postPlanReady(ctx context.Context, itemID string) {
	msg, shown := r.planReadyMessage(itemID)
	if msg == "" {
		return
	}
	if r.postInThread(ctx, itemID, msg) && len(shown) > 0 {
		r.mu.Lock()
		if r.shown == nil {
			r.shown = map[string][]shownRev{}
		}
		r.shown[itemID] = shown
		r.mu.Unlock()
	}
}

func (r *slackRelay) shownRevisions(itemID string) []shownRev {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shown[itemID]
}

// pickShown resolves a reply's selector against what the plan-ready message
// showed: no selector is fine for a single revision; with candidates it
// must name one by number (1-based, as listed) or label.
func pickShown(shown []shownRev, sel string) (string, string) {
	if sel == "" {
		if len(shown) == 1 {
			return shown[0].ID, ""
		}
		return "", fmt.Sprintf("There are %d candidates — reply `approve <number>` (e.g. `approve 1`) or `approve <name>`.", len(shown))
	}
	if n, err := strconv.Atoi(sel); err == nil {
		if n >= 1 && n <= len(shown) && (len(shown) > 1 || n == 1) {
			return shown[n-1].ID, ""
		}
		return "", fmt.Sprintf("No candidate %d — there are %d.", n, len(shown))
	}
	for _, s := range shown {
		if s.Label != "" && strings.EqualFold(s.Label, sel) {
			return s.ID, ""
		}
	}
	return "", "No candidate named `" + sel + "`."
}

// planReadyMessage formats the gate message for the item's latest plan
// revision: the summary from the newest Enrichment section, the revision's
// steps and skills, and the planner's questions.
func (r *slackRelay) planReadyMessage(itemID string) (string, []shownRev) {
	item, err := r.inbox.GetItem(context.Background(), itemID)
	if err != nil {
		return "", nil
	}
	// A multi-agent plan round: list the candidates, numbered.
	if item.PlanID != "" && r.revisions != nil {
		if cands, cerr := r.revisions.OpenCandidates(relayCtx(), item.PlanID); cerr == nil && len(cands) > 1 {
			return candidatesMessage(item, cands)
		}
	}
	var rev *plans.Revision
	if item.PlanID != "" && r.revisions != nil {
		rev, _ = r.revisions.LatestRevision(relayCtx(), item.PlanID)
	}
	var b strings.Builder
	b.WriteString("🧭 *Plan ready* · " + item.Title)
	if rev != nil {
		fmt.Fprintf(&b, " · revision #%d", rev.RevisionNo)
	}
	b.WriteString("\n\n")
	enrich := lastSectionOf(item.Content, "## Enrichment")
	if i := strings.Index(enrich, "### Steps"); i >= 0 {
		enrich = strings.TrimSpace(enrich[:i])
	}
	if enrich != "" {
		b.WriteString(enrich + "\n\n")
	}
	if rev != nil {
		b.WriteString("*Steps:*\n")
		n := 0
		for _, st := range rev.Steps {
			if st.Phase == "review" {
				continue
			}
			n++
			fmt.Fprintf(&b, "%d. %s\n", n, st.Title)
		}
		if len(rev.Skills) > 0 {
			b.WriteString("*Skills:* " + strings.Join(rev.Skills, ", ") + "\n")
		}
	} else if enrich == "" {
		b.WriteString("_Plan details in skopos._\n")
	}
	if q := lastSectionOf(item.Content, "## Questions"); q != "" {
		b.WriteString("\n*Questions for you:*\n" + q + "\n")
	}
	b.WriteString("\n`approve` · `approve: <notes>` · `revise: <notes>` · `reject`")
	var shown []shownRev
	if rev != nil {
		shown = []shownRev{{ID: rev.ID}}
	}
	return b.String(), shown
}

// candidatesMessage is the gate message for a multi-agent plan round: each
// candidate with its planner, step titles and skills, numbered in the order
// approve/revise refer to.
func candidatesMessage(item *inbox.Item, cands []plans.Revision) (string, []shownRev) {
	var b strings.Builder
	fmt.Fprintf(&b, "🧭 *%d plan candidates* · %s\n", len(cands), item.Title)
	shown := make([]shownRev, 0, len(cands))
	for i, c := range cands {
		label := c.Label
		if label == "" {
			label = "planner"
		}
		fmt.Fprintf(&b, "\n*%d · %s*\n", i+1, label)
		n := 0
		for _, st := range c.Steps {
			if st.Phase == "review" {
				continue
			}
			n++
			if n <= 6 {
				fmt.Fprintf(&b, "  %d. %s\n", n, st.Title)
			}
		}
		if n > 6 {
			fmt.Fprintf(&b, "  _…%d more steps_\n", n-6)
		}
		if len(c.Skills) > 0 {
			b.WriteString("  _skills: " + strings.Join(c.Skills, ", ") + "_\n")
		}
		shown = append(shown, shownRev{ID: c.ID, Label: c.Label})
	}
	// The judge's recommendation, when a compare stage ran (advisory: it
	// never pre-selects anything).
	if cmp := lastSectionOf(item.Content, "## Comparison"); cmp != "" {
		for _, line := range strings.Split(cmp, "\n") {
			if rec, ok := strings.CutPrefix(strings.TrimSpace(line), "**Recommendation:**"); ok {
				b.WriteString("\n*Judge recommends:* " + strings.TrimSpace(rec) + " _(details in skopos)_\n")
				break
			}
		}
	}
	if q := lastSectionOf(item.Content, "## Questions"); q != "" {
		b.WriteString("\n*Questions for you:*\n" + q + "\n")
	}
	b.WriteString("\n`approve <n>` · `approve <n>: <notes>` · `revise <n>: <notes>` (one planner reworks it) · `revise: <notes>` (re-plan all) · `reject`")
	return b.String(), shown
}

// formatGate builds the per-status message with command hints (the
// agent-trial formats; essentials only, capped by the notifier).
func (r *slackRelay) formatGate(itemID string, from, to inbox.Status) string {
	item, err := r.inbox.GetItem(context.Background(), itemID)
	if err != nil {
		return ""
	}
	title := item.Title
	switch to {
	case inbox.StatusAwaitingApproval:
		msg, _ := r.planReadyMessage(itemID)
		return msg
	case inbox.StatusBlocked, inbox.StatusFailed:
		reason := ""
		if runs, err := r.runs.ListRuns(context.Background(), itemID); err == nil {
			for _, run := range runs {
				if run.Status == "failed" && run.GroupID != "" {
					// A multi-agent stage failed: its summary is the reason
					// (decision, who found what), not one member's text.
					reason = groupSummarySection(item.Content, run.Stage)
					break
				}
				if run.Status == "failed" && run.Summary != "" {
					reason = run.Summary
					break
				}
			}
		}
		if reason == "" {
			reason = "_Reason in skopos._"
		}
		return "⚠️ *Blocked* · " + title + "\n\n" + truncate(reason, 500) +
			"\n\n`retry: <answer>` · `reject`"
	case inbox.StatusInReview:
		branch, head, diff := "", "", ""
		if runs, err := r.runs.ListRuns(context.Background(), itemID); err == nil {
			for _, run := range runs {
				if run.Stage == "implement" && run.Status == "succeeded" {
					branch, head, diff = run.Branch, run.HeadSHA, run.Diffstat
					break
				}
			}
		}
		detail := branch
		if diff != "" {
			detail = branch + " · " + strings.SplitN(diff, "\n", 2)[0]
		}
		// After a multi-agent review/QA, its summary (members, nits) rides
		// along; the newest grouped run decides which stage's summary.
		if runs, err := r.runs.ListRuns(context.Background(), itemID); err == nil {
			for _, run := range runs {
				if run.Stage == "implement" {
					break // nothing newer than the implementation was grouped
				}
				if run.GroupID != "" && (run.Stage == "review" || run.Stage == "qa") {
					if sum := groupSummarySection(item.Content, run.Stage); sum != "" {
						detail += "\n\n" + truncate(sum, 900)
					}
					break
				}
			}
		}
		return "✅ *Branch ready* · " + title + "\n\n" + detail +
			"\n\n`done` (approves head " + short(head) + ") · `changes: <notes>` · `reject`"
	case inbox.StatusDone:
		return "🏁 *Done* · " + title
	case inbox.StatusDiscarded:
		return "🗑 Rejected · " + title
	}
	return ""
}

// Run drains buffered replies on a ticker (the status hook drains them the
// moment an item transitions; this is the safety net for missed events).
func (r *slackRelay) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.drainAll(ctx)
		}
	}
}

func (r *slackRelay) buffer(itemID, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = map[string][]pendingReply{}
	}
	r.pending[itemID] = append(r.pending[itemID], pendingReply{text: text})
}

// drain applies an item's buffered replies while its status admits them.
// A panic while applying is logged and swallowed: the relay runs on
// background goroutines, and one bad reply must not take the server down.
func (r *slackRelay) drain(ctx context.Context, itemID string) {
	defer func() {
		if p := recover(); p != nil {
			if r.log != nil {
				r.log.Warn("slack relay: drain panicked", "item", itemID, "panic", fmt.Sprint(p))
			}
		}
	}()
	item, err := r.inbox.GetItem(context.Background(), itemID)
	if err != nil || !bufferAdmissible(item.Status) {
		return
	}
	r.mu.Lock()
	replies := r.pending[itemID]
	delete(r.pending, itemID)
	r.mu.Unlock()
	for _, reply := range replies {
		r.Apply(ctx, itemID, reply.text)
	}
}

func (r *slackRelay) drainAll(ctx context.Context) {
	r.mu.Lock()
	ids := make([]string, 0, len(r.pending))
	for id := range r.pending {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.drain(ctx, id)
	}
}

// isCommand: the thread-reply vocabulary.
func isCommand(w string) bool {
	switch w {
	case "approve", "approved", "lgtm", "revise", "changes", "done", "retry", "reject":
		return true
	}
	return isAgentsCommand(w)
}

// isAgentsCommand: the per-item agents switch replies.
func isAgentsCommand(w string) bool {
	return w == inbox.AgentsPanel || w == inbox.AgentsSolo || w == "default"
}

// isApproval: commands that approve a subject (a plan revision, a review
// head) — they must answer a gate message, never a future state.
func isApproval(cmd string) bool {
	switch cmd {
	case "approve", "approved", "lgtm", "done":
		return true
	}
	return false
}

// bufferAdmissible: statuses where a buffered reply can be applied — the
// gates and the blocked/failed states (mid-run statuses keep waiting).
func bufferAdmissible(s inbox.Status) bool {
	return s == inbox.StatusAwaitingApproval || s == inbox.StatusInReview ||
		s == inbox.StatusBlocked || s == inbox.StatusFailed
}

// --- helpers ---

// reviewHead is the head the review approval names: the pushed head of the
// item's latest successful implement run.
func (r *slackRelay) reviewHead(itemID string) string {
	runs, err := r.runs.ListRuns(context.Background(), itemID)
	if err != nil {
		return ""
	}
	for _, run := range runs {
		if run.Stage == "implement" && run.Status == "succeeded" && run.HeadSHA != "" {
			return run.HeadSHA
		}
	}
	return ""
}

func (r *slackRelay) bindThread(ctx context.Context, itemID, ts string) {
	_ = r.inbox.SetSlackThread(ctx, itemID, ts)
	if r.notifier != nil {
		r.notifier.BindThread(ts, itemID)
	}
	if r.rebindHook != nil {
		r.rebindHook(ts, itemID)
	}
}

func (r *slackRelay) post(ctx context.Context, parent, text string) {
	if r.notifier != nil {
		r.notifier.Notify(ctx, parent, text)
	}
}

// groupSummarySection is the newest group summary for a stage ("Review
// summary — round N" / "QA summary — round N"), without its heading line.
func groupSummarySection(content, stage string) string {
	heading := "## Review summary"
	if stage == "qa" {
		heading = "## QA summary"
	}
	sec := lastSectionOf(content, heading)
	if sec == "" {
		return ""
	}
	first, rest, _ := strings.Cut(sec, "\n")
	return "*" + strings.TrimSpace(strings.TrimPrefix(first, "## ")) + "*\n" + strings.TrimSpace(rest)
}

// lastSectionOf is sectionOf for the LAST occurrence of heading — after a
// revise the item carries several Enrichment sections, and the newest one
// describes the plan at the gate.
func lastSectionOf(content, heading string) string {
	i := strings.LastIndex(content, heading)
	if i < 0 {
		return ""
	}
	return sectionOf(content[i:], heading)
}

// sectionOf extracts a markdown section (heading included) from content.
func sectionOf(content, heading string) string {
	i := strings.Index(content, heading)
	if i < 0 {
		return ""
	}
	rest := content[i:]
	if j := strings.Index(rest[len(heading):], "\n## "); j >= 0 {
		rest = rest[:len(heading)+j]
	}
	return strings.TrimSpace(rest)
}

func statusWord(s inbox.Status) string {
	return strings.ReplaceAll(string(s), "_", " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
