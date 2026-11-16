package cmd

import (
	"context"
	"errors"
	"fmt"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/qa"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/approvals"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/integrations"
	"github.com/martinsuchenak/skopos/internal/itemtext"
	"github.com/martinsuchenak/skopos/internal/labels"
	"github.com/martinsuchenak/skopos/internal/notify"
	"github.com/martinsuchenak/skopos/internal/runs"
)

// slackRelay is the Slack command relay. It adapts the approver's replies
// onto the approver-gated inbox workflow actions and formats the outbound
// gate notifications, one thread per item.
//
// Security model (design doc §2): the relay acts as the approver after the Slack
// provider has checked their user id — the gate rests on their Slack account.
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
	// item. In memory: after a restart the relay cannot know what the approver
	// read, so an approve then re-posts the current plan (fail closed).
	shown map[string][]shownRev
	// shownFindings is the numbering of the findings list the latest
	// message showed per item (finding ids, in order). In memory too: after
	// a restart a reply by number is asked to list them again.
	shownFindings map[string][]string
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
var reCmd = regexp.MustCompile(`(?is)^\s*([a-z]+)(?:\s+([a-z0-9@._,-]+))?(?:\s*[.!]?\s*|\s*:\s*(.*))$`)

// reWord is the leading word of a reply that did not parse as a command —
// used only to hint at the syntax when it names one.
var reWord = regexp.MustCompile(`(?i)^\s*([a-z]+)`)

// `new: <what>` or `new <what>`; "news: ..." / "newer ..." are not captures.
var reNew = regexp.MustCompile(`(?is)^\s*new(?:\s*:\s*|\s+)(.+)$`)
var reSelector = regexp.MustCompile(`(?s)^([A-Za-z0-9._/~+-]+):\s+(.+)$`)

// reHashtag: `#name` words in a capture become item tags — the skill tag
// hints the planner unions into its selection (capability profiles §3).
var reHashtag = regexp.MustCompile(`(?:^|\s)#([a-z0-9][a-z0-9._-]{0,63})\b`)

// reNeeds: a `needs: <label>[, <label>…]` line of its own in a capture names
// the labels the item needs of a worker.
var reNeeds = regexp.MustCompile(`(?im)^[ \t]*needs[ \t]*:[ \t]*(.*?)[ \t]*$`)

// captureNeeds takes the `needs:` lines out of a capture, below its first
// line (the title). It returns the text without them and the labels named;
// found says a line was there, so an empty or invalid list is refused and
// never read as "no requirement": an item that lost its needs would run
// on a worker it was meant to avoid.
func captureNeeds(text string) (rest string, needs []string, found bool, err error) {
	first, below, _ := strings.Cut(text, "\n")
	var named []string
	below = reNeeds.ReplaceAllStringFunc(below, func(line string) string {
		found = true
		named = append(named, labels.Split(reNeeds.FindStringSubmatch(line)[1])...)
		return ""
	})
	if !found {
		return text, nil, false, nil
	}
	if needs, err = inbox.NormalizeNeeds(named); err != nil {
		return "", nil, true, err
	}
	if len(needs) == 0 {
		return "", nil, true, fmt.Errorf("the `needs:` line names no label")
	}
	return strings.TrimSpace(first + "\n" + below), needs, true, nil
}

// needsWords lists labels for a message: `stock`, `plant-2`.
func needsWords(list []string) string {
	out := make([]string, 0, len(list))
	for _, l := range list {
		out = append(out, "`"+l+"`")
	}
	return strings.Join(out, ", ")
}

// needsWarning says when no worker that serves the workspace has every
// label the item needs: a label nobody holds (a typo is a valid label)
// would leave the item waiting without a word. "" = some worker has them,
// or the workers could not be read.
func (r *slackRelay) needsWarning(ws string, needs []string) string {
	if len(needs) == 0 || r.runs == nil {
		return ""
	}
	workers, err := r.runs.ListWorkers(relayCtx())
	if err != nil {
		return ""
	}
	for _, w := range workers {
		if slices.Contains(w.Workspaces, ws) && labels.Covers(w.Labels, needs) {
			return ""
		}
	}
	return "⚠️ No worker serving `" + ws + "` has " + needsWords(needs) + " now: the stages that need it wait until one does (`skopos worker label`)."
}

// captureTags is slack-new plus the capture's hashtags (lowercased, unique).
// #panel and #solo are the item's agents switch (captureAgents), not tags.
func captureTags(text string) []string {
	tags := []string{"slack-new"}
	seen := map[string]bool{"slack-new": true, inbox.AgentsPanel: true, inbox.AgentsSolo: true, autoTag: true}
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

// autoTag: `#auto` in a capture approves the item's plan automatically when
// the planner asks no question.
const autoTag = "auto"

// captureAuto reports `#auto` in a capture.
func captureAuto(text string) bool {
	for _, m := range reHashtag.FindAllStringSubmatch(strings.ToLower(text), -1) {
		if m[1] == autoTag {
			return true
		}
	}
	return false
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
	"• `answer: 1) a, 2) …` — answer the open questions without approving (while the item waits for you)\n" +
	"• `reject` — discard the item\n" +
	"• `second` / `second <runner>` — an advisory second opinion on the finished branch (`force` re-runs a runner on the same head)\n" +
	"• `findings` — the review findings, numbered; `changes 2,3: <notes>` / `retry 2: <answer>` send findings back to be fixed, `waive 2: <why>` accepts one\n" +
	"• `panel` / `solo` / `default` — the item's plan, review and QA stages use the repository's agent panel, one agent, or the repository default (from the next stage)\n" +
	"• `needs: <label>, <label>` — only a worker with these labels works on the item; `needs: -` clears it, `needs` shows it\n" +
	"• `auto: on` / `auto: off` — approve the plan automatically when the planner asks no question (the branch still comes to you); `auto` shows it\n" +
	"Top-level: `new: <what to do>` creates and queues an item; add `#panel` or `#solo` to choose its agents, `#auto` to approve its plan automatically, and a line `needs: <label>` below it for a worker with that label."

// TopLevel handles threadless DMs: capture and help. ts is the approver's own
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
	what, needs, _, err := captureNeeds(what)
	if err != nil {
		r.post(ctx, "", "⚠️ Nothing created: "+firstLine(err.Error())+". A `needs:` line names labels, comma-separated: `needs: stock, plant-2`.")
		return true
	}
	item, err := r.inbox.CreateItem(relayCtx(), inbox.CreateInput{
		// The title is the capture's first line: with a `needs:` line the
		// capture has several.
		WorkspaceID: ws, Title: truncate(firstLine(what), 80),
		Content:         what + "\n\n_Created from Slack (`new:`), " + time.Now().UTC().Format("2006-01-02 15:04") + " UTC._",
		Tags:            captureTags(what),
		AuthorAgentID:   "slack",
		Agents:          captureAgents(what),
		Needs:           needs,
		AutoApprovePlan: captureAuto(what),
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
	if len(item.Needs) > 0 {
		agentsNote = " · needs " + needsWords(item.Needs) + agentsNote
	}
	if item.AutoApprovePlan {
		agentsNote += " · plan approved automatically when it has no questions"
	}
	msg := "📥 Queued in `" + ws + "`" + agentsNote + " · planning starts within a minute. The plan will come back in this thread."
	if warn := r.needsWarning(ws, item.Needs); warn != "" {
		msg += "\n" + warn
	}
	r.post(ctx, ts, msg)
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
		// `findings`, `waive` and `needs` are ordinary words at the start
		// of a sentence ("findings look fine", "needs a rebase first"): no
		// hint for those.
		if w := reWord.FindStringSubmatch(text); w != nil && isCommand(strings.ToLower(w[1])) && !isFindingCommand(strings.ToLower(w[1])) && !isSettingWord(strings.ToLower(w[1])) {
			if item, err := r.inbox.GetItem(relayCtx(), itemID); err == nil {
				word := strings.ToLower(w[1])
				r.post(ctx, item.SlackThreadTS, "🤔 Not applied — a command is the bare word (`"+word+"`) or `"+word+": <notes>` on its own.")
			}
		}
		return false
	}
	cmd, sel, arg := strings.ToLower(m[1]), strings.ToLower(strings.TrimSpace(m[2])), strings.TrimSpace(m[3])
	byNumber := (cmd == "changes" || cmd == "retry" || cmd == "waive") && reFindingNumbers.MatchString(sel)
	if sel != "" && !byNumber && cmd != "approve" && cmd != "approved" && cmd != "lgtm" && cmd != "revise" && cmd != "second" {
		// Only approve/revise (a candidate), second (a runner) and
		// changes/retry/waive (findings by number) take a selector;
		// anything else with a word after it is prose.
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
	// So are the labels the item needs of a worker, and the automatic plan
	// approval.
	if cmd == "needs" {
		return r.setNeeds(ctx, item, arg)
	}
	if cmd == autoTag {
		return r.setAuto(ctx, item, arg)
	}
	// Listing findings and waiving one are no workflow action either: they
	// apply at once, whatever the item is doing.
	if isFindingCommand(cmd) {
		return r.applyFindingCommand(ctx, item, cmd, sel, arg)
	}
	if !inbox.IsWorkflowStatus(item.Status) {
		r.post(ctx, item.SlackThreadTS, "⚠️ The item is "+statusWord(item.Status)+" — no workflow actions apply.")
		return true
	}
	// Mid-run guard: a reply that would pull the worktree out from under a
	// running agent waits for the run to finish.
	if item.Status == inbox.StatusPlanning || item.Status == inbox.StatusImplementing ||
		item.Status == inbox.StatusQueued || item.Status == inbox.StatusApproved {
		// Approvals are never buffered: a buffered approve/done would apply
		// the instant the gate is reached — approving a plan or a head
		// commit the approver has not seen (review H2). They wait for the gate
		// message instead; the non-advancing replies still buffer.
		// Answers name the questions open now: buffered, they would land
		// on whatever the next round asks.
		// A reply by number names findings in the list shown now: buffered,
		// it would pick from whatever list the next message shows.
		if isApproval(cmd) || cmd == "answer" || byNumber {
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
		notes, reason := splitWhy(arg)
		approved, err := r.inbox.Approve(relayCtx, item.ID, inbox.ApproveInput{Notes: notes, Reason: reason, Via: audit.ViaSlack, RevisionID: revID})
		if err != nil {
			if errors.Is(err, inbox.ErrStaleRevision) || errors.Is(err, plans.ErrStaleRevision) {
				r.post(ctx, thread, "⚠️ The plan changed since the message you replied to. The current plan follows — approve that one.")
				r.postPlanReady(ctx, item.ID)
				return true
			}
			return fail(err)
		}
		r.postMatched(ctx, thread, approved)
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
		revised, err := r.inbox.RequestChangesOn(relayCtx, item.ID, arg, audit.ViaSlack, reviseID)
		if err != nil {
			return fail(err)
		}
		r.postMatched(ctx, thread, revised)
		return true
	case "changes":
		if item.Status != inbox.StatusInReview {
			r.post(ctx, thread, "⚠️ changes applies to a finished branch — the item is "+statusWord(item.Status)+".")
			return true
		}
		// `changes 2,3: notes` sends those findings back with the notes.
		in := inbox.ChangesInput{Notes: arg, Via: audit.ViaSlack}
		if sel != "" {
			ids, msg := r.pickFindings(item.ID, sel)
			if msg != "" {
				r.post(ctx, thread, "⚠️ "+msg)
				return true
			}
			in.Findings = ids
		}
		if _, err := r.inbox.RequestChangesWith(relayCtx, item.ID, in); err != nil {
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
		// `retry 2: answer` has those findings fixed in the run it starts.
		in := inbox.RetryInput{Answers: qa.AnswersInput{Text: arg}, Via: audit.ViaSlack}
		if sel != "" {
			ids, msg := r.pickFindings(item.ID, sel)
			if msg != "" {
				r.post(ctx, thread, "⚠️ "+msg)
				return true
			}
			in.Findings = ids
		}
		retried, err := r.inbox.RetryOn(relayCtx, item.ID, in)
		if err != nil {
			return fail(err)
		}
		r.postMatched(ctx, thread, retried)
		return true
	case "answer":
		// Answers (or corrections) without moving the item, while it waits.
		answered, err := r.inbox.Answer(relayCtx, item.ID, qa.AnswersInput{Text: arg}, audit.ViaSlack)
		if err != nil {
			return fail(err)
		}
		if len(answered.AnswersMatched) == 0 {
			r.post(ctx, thread, "📝 Recorded as written: no open question matched.")
			return true
		}
		r.postMatched(ctx, thread, answered)
		return true
	case "reject":
		if _, err := r.inbox.Reject(relayCtx, item.ID, arg, audit.ViaSlack); err != nil {
			return fail(err)
		}
		return true
	case "second":
		// `second`, `second codex`, `second: codex`; "force" re-runs a
		// runner that already reviewed this head.
		in := inbox.SecondOpinionInput{Via: audit.ViaSlack}
		for _, w := range append([]string{sel}, strings.Fields(arg)...) {
			switch w = strings.TrimSpace(w); {
			case w == "":
			case strings.EqualFold(w, "force"):
				in.Force = true
			case in.Runner == "":
				in.Runner = w
			}
		}
		req, err := r.inbox.RequestSecondOpinion(relayCtx, item.ID, in)
		if err != nil {
			return fail(err)
		}
		r.post(ctx, thread, fmt.Sprintf("🔍 Second opinion queued: `%s` reviews %s. I'll post it here when it's done (advisory: the item stays %s).",
			req.Runner, shortHead(req.HeadSHA), statusWord(item.Status)))
		return true
	}
	return false
}

// PostSecondOpinion posts a finished second opinion to the item's thread.
func (r *slackRelay) PostSecondOpinion(ctx context.Context, res runs.SecondOpinionResult) {
	item, err := r.inbox.GetItem(relayCtx(), res.ItemID)
	if err != nil || item.SlackThreadTS == "" {
		return
	}
	icon := "🔍"
	switch res.Verdict {
	case "pass":
		icon = "✅"
	case "fail":
		icon = "⚠️"
	}
	r.post(ctx, item.SlackThreadTS, icon+" *"+res.Heading+"*\n"+truncate(strings.TrimSpace(res.Body), 2800))
}

// PostWorkerHealth posts, once per change, that a worker stopped taking a
// workspace's work because its preflight fails, or that it is back. Top
// level: it is about a worker, not an item (the items move on elsewhere).
func (r *slackRelay) PostWorkerHealth(ctx context.Context, h runs.WorkerHealth) {
	if h.Healthy {
		r.post(ctx, "", "✅ Worker `"+h.Worker+"` is taking `"+h.Workspace+"` work again (its preflight passes).")
		return
	}
	r.post(ctx, "", "⚠️ Worker `"+h.Worker+"` stopped taking `"+h.Workspace+"` work: its preflight fails. Only other workers serving it pick up its items until it passes.\n"+truncate(h.Reason, 600))
}

// PostWebhookDisabled says a webhook subscription stopped delivering.
func (r *slackRelay) PostWebhookDisabled(ctx context.Context, w integrations.Webhook, reason string) {
	r.post(ctx, "", "⚠️ Webhook `"+w.Name+"` ("+endpointOnly(w.URL)+") is disabled: "+truncate(reason, 400)+
		"\nIts events wait; enable it again with `skopos webhook enable "+w.ID+"` or from the dashboard's Webhooks view.")
}

// endpointOnly is a URL without its query and fragment: receivers often
// authenticate with a token there, and a Slack channel is wider than the
// people who manage webhooks.
func endpointOnly(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the endpoint"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}

func shortHead(sha string) string {
	if len(sha) > 7 {
		return "`" + sha[:7] + "`"
	}
	return "`" + sha + "`"
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

// setNeeds applies a `needs: <labels>` reply: the labels the item needs of
// a worker. `needs: -` clears them; the bare word shows them.
func (r *slackRelay) setNeeds(ctx context.Context, item *inbox.Item, arg string) bool {
	thread := item.SlackThreadTS
	named, set := labels.Arg(arg)
	if !set {
		if len(item.Needs) == 0 {
			r.post(ctx, thread, "🏷️ This item needs no label: any worker serving `"+item.WorkspaceID+"` may work on it. Set one with `needs: <label>`.")
		} else {
			r.post(ctx, thread, "🏷️ This item needs a worker with "+needsWords(item.Needs)+". `needs: -` clears it.")
		}
		return true
	}
	needs, err := inbox.NormalizeNeeds(named)
	if err == nil {
		if needs == nil {
			needs = []string{}
		}
		err = r.inbox.UpdateItem(relayCtx(), item.ID, inbox.UpdateInput{Needs: &needs, Via: audit.ViaSlack})
	}
	if err != nil {
		r.post(ctx, thread, "⚠️ Not applied: "+truncate(err.Error(), 300))
		return true
	}
	if len(needs) == 0 {
		r.post(ctx, thread, "🏷️ This item needs no label now: any worker serving `"+item.WorkspaceID+"` may work on it.")
		return true
	}
	msg := "🏷️ This item now needs a worker with " + needsWords(needs) + ". Runs that still wait follow it; a run in progress keeps its worker."
	if warn := r.needsWarning(item.WorkspaceID, needs); warn != "" {
		msg += "\n" + warn
	}
	r.post(ctx, thread, msg)
	return true
}

// reWhy: a `why: <reason>` line in an approval says why that plan was
// chosen; it is kept with the approval, not given to the implementer.
var reWhy = regexp.MustCompile(`(?im)^[ \t]*why[ \t]*:[ \t]*(.*?)[ \t]*$`)

// splitWhy takes the `why:` lines out of an approval's notes.
func splitWhy(arg string) (notes, reason string) {
	var why []string
	notes = reWhy.ReplaceAllStringFunc(arg, func(line string) string {
		if r := strings.TrimSpace(reWhy.FindStringSubmatch(line)[1]); r != "" {
			why = append(why, r)
		}
		return ""
	})
	return strings.TrimSpace(notes), strings.Join(why, " ")
}

// isSettingWord: item settings whose word also starts ordinary sentences
// ("needs a rebase", "auto deploy is down"): no syntax hint for those.
func isSettingWord(w string) bool { return w == "needs" || w == autoTag }

// setAuto applies an `auto: on|off` reply: the item's automatic plan
// approval. The bare word shows it.
func (r *slackRelay) setAuto(ctx context.Context, item *inbox.Item, arg string) bool {
	thread := item.SlackThreadTS
	if strings.TrimSpace(arg) == "" {
		if item.AutoApprovePlan {
			r.post(ctx, thread, "🤖 This item's plan is approved automatically when the planner asks no question (set by "+item.AutoApproveBy+"). `auto: off` stops it.")
		} else {
			r.post(ctx, thread, "🤖 This item's plan waits for you. `auto: on` approves it automatically when the planner asks no question.")
		}
		return true
	}
	on, err := onOff(arg)
	if err != nil {
		r.post(ctx, thread, "⚠️ Not applied: `auto: on` or `auto: off`.")
		return true
	}
	if err := r.inbox.UpdateItem(relayCtx(), item.ID, inbox.UpdateInput{AutoApprovePlan: &on, Via: audit.ViaSlack}); err != nil {
		r.post(ctx, thread, "⚠️ Not applied: "+truncate(err.Error(), 300))
		return true
	}
	switch {
	case !on:
		r.post(ctx, thread, "🤖 This item's plan waits for you now.")
	case item.Status == inbox.StatusAwaitingApproval:
		r.post(ctx, thread, "🤖 Set. The plan waiting now is still yours to approve; the next one is approved automatically when it has no questions.")
	default:
		r.post(ctx, thread, "🤖 This item's plan is approved automatically when the planner asks no question. The branch still comes to you.")
	}
	return true
}

// postAutoApproved says that the server approved the item's plan: the plan
// it approved, and that the implementation starts. False when the newest
// plan approval is not an automatic one.
func (r *slackRelay) postAutoApproved(ctx context.Context, itemID string) bool {
	entries, err := r.inbox.Approvals(relayCtx(), itemID) // newest first
	if err != nil {
		return false
	}
	auto := false
	for _, e := range entries {
		if e.Gate == approvals.GatePlan {
			auto = e.Via == audit.ViaAuto
			break
		}
	}
	if !auto {
		return false
	}
	msg, _ := r.planReadyMessage(itemID)
	if msg == "" {
		return false
	}
	// The plan as the gate would have shown it, under its own heading and
	// without the gate's replies.
	msg = strings.Replace(msg, "🧭 *Plan ready*", "✅ *Plan approved automatically*", 1)
	if i := strings.LastIndex(msg, "\n`approve`"); i >= 0 {
		msg = msg[:i]
	}
	r.postInThread(ctx, itemID, msg+"\n\nThe planner asked no question, so the implementation starts now. The branch comes to you at sign-off. `reject` stops it.")
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
	if to == inbox.StatusApproved && r.postAutoApproved(ctx, itemID) {
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
	// The item asked for its plan to be approved automatically, and
	// something kept it for the person: say what.
	if held := inbox.AutoApproveHeld(ctx); held != "" {
		msg += "\n_Not approved automatically: " + held + "._"
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
	if q := questionsText(item.Content, "*"); q != "" {
		b.WriteString("\n*Questions for you* (answer in the notes: `approve: 1) a, 2) …`):\n" + q)
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
	if q := questionsText(item.Content, "*"); q != "" {
		b.WriteString("\n*Questions for you* (answer in the notes: `approve <n>: 1) a, 2) …`):\n" + q)
	}
	b.WriteString("\n`approve <n>` · `approve <n>: <notes>` (a line `why: …` records why you chose it) · `revise <n>: <notes>` (one planner reworks it) · `revise: <notes>` (re-plan all) · `reject`")
	return b.String(), shown
}

// formatGate builds the per-status message with command hints
// (essentials only, capped by the notifier).
func (r *slackRelay) formatGate(itemID string, from, to inbox.Status) string {
	item, err := r.inbox.GetItem(context.Background(), itemID)
	if err != nil {
		return ""
	}
	// The title and the branch line are cut to a share of the message's
	// budget (the notifier cuts at 2800 bytes): with the group summary (900)
	// and the findings list (maxFindingsBytes) the message always fits. A
	// message cut by the notifier would lose findings the relay had already
	// numbered — a reply by number would name one nobody was shown.
	title := truncate(item.Title, gateTitleBytes)
	// A gate message replaces what the thread showed before: a findings
	// list it does not repeat is no longer the one a number refers to.
	r.forgetFindings(itemID)
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
				// The failure's own reason first ("checks failed: …"),
				// else what the run reported.
				if run.Status == "failed" && (run.FailReason != "" || run.Summary != "") {
					reason = run.FailReason
					if reason == "" {
						reason = run.Summary
					}
					break
				}
			}
		}
		// The planner needs answers: its questions, one per line, instead
		// of the run's one-line reason.
		if strings.HasPrefix(reason, "needs clarification") {
			if q := questionsText(item.Content, "*"); q != "" {
				return "❓ *Questions for you* · " + title + "\n\n" + q +
					"\n`retry: 1) a, 2) …` · `reject`"
			}
		}
		if reason == "" {
			reason = "_Reason in skopos._"
		}
		// Blocked with questions (the implementer's): the reason, then the
		// questions to answer with the retry.
		if q := questionsText(item.Content, "*"); q != "" {
			return "⚠️ *Blocked* · " + title + "\n\n" + truncate(reason, 500) +
				"\n\n*Questions for you:*\n" + q + "\n`retry: 1) a, 2) …` · `reject`"
		}
		if block := r.findingsBlock(itemID); block != "" {
			return "⚠️ *Blocked* · " + title + "\n\n" + truncate(reason, 500) + "\n\n" + block +
				"\n`retry: <answer>` · " + findingHints(r.inbox.SendBackWith(relayCtx(), item)) + " · `reject`"
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
		detail = truncate(detail, gateDetailBytes)
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
		if block := r.findingsBlock(itemID); block != "" {
			return "✅ *Branch ready* · " + title + "\n\n" + detail + "\n\n" + block +
				"\n`done` (approves head " + short(head) + ") · `changes: <notes>` · " + findingHints(r.inbox.SendBackWith(relayCtx(), item)) + " · `reject`"
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
	case "approve", "approved", "lgtm", "revise", "changes", "done", "retry", "reject", "second", "answer", "waive", "findings", "needs", autoTag:
		return true
	}
	return isAgentsCommand(w)
}

// postMatched replies with what an action's answers were recorded as: the
// questions answered (and the option), and whether the rest became an
// answer to the round's questions.
func (r *slackRelay) postMatched(ctx context.Context, thread string, item *inbox.Item) {
	if item == nil || len(item.AnswersMatched) == 0 {
		return
	}
	var linked []string
	general := false
	for _, m := range item.AnswersMatched {
		switch {
		case m.QuestionID != "":
			s := strconv.Itoa(m.N)
			if m.Option != "" {
				s += ") " + m.Option
			}
			linked = append(linked, s)
		case m.General:
			general = true
		}
	}
	var parts []string
	if len(linked) > 0 {
		parts = append(parts, "answers to "+strings.Join(linked, ", "))
	}
	if general {
		parts = append(parts, "the rest as an answer to the round's questions")
	}
	if len(parts) > 0 {
		r.post(ctx, thread, "📝 Recorded "+strings.Join(parts, "; ")+".")
	}
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

// questionsText lists the questions the newest round asked
// (itemtext.CurrentQuestions): one numbered question per line, its options
// lettered on their own lines, a plan group member's under its label. bold
// wraps the numbers and labels ("*" for Slack, "" for the CLI). "" when the
// round asked none.
func questionsText(content, bold string) string {
	var b strings.Builder
	for _, a := range itemtext.CurrentQuestions(content) {
		if a.Label != "" {
			fmt.Fprintf(&b, "%s%s asks:%s\n", bold, a.Label, bold)
		}
		for i, q := range a.Questions {
			fmt.Fprintf(&b, "%s%d.%s %s\n", bold, i+1, bold, q.Text)
			for j, o := range q.Options {
				fmt.Fprintf(&b, "      %s) %s\n", itemtext.OptionLetter(j), o)
			}
		}
	}
	return b.String()
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

// gateTitleBytes and gateDetailBytes bound an item's title and the branch
// line in a gate message.
const (
	gateTitleBytes  = 200
	gateDetailBytes = 300
)

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
