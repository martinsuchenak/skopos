package cmd

import (
	"context"
	"fmt"
	"regexp"
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
	log        notify.Logger           // nil = silent (tests)

	mu      sync.Mutex
	pending map[string][]pendingReply // itemID -> replies waiting for an admissible status
}

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
var reCmd = regexp.MustCompile(`(?is)^\s*([a-z]+)(?:\s*[.!]?\s*|\s*:\s*(.*))$`)

// reWord is the leading word of a reply that did not parse as a command —
// used only to hint at the syntax when it names one.
var reWord = regexp.MustCompile(`(?i)^\s*([a-z]+)`)

// `new: <what>` or `new <what>`; "news: ..." / "newer ..." are not captures.
var reNew = regexp.MustCompile(`(?is)^\s*new(?:\s*:\s*|\s+)(.+)$`)
var reSelector = regexp.MustCompile(`(?s)^([A-Za-z0-9._/~+-]+):\s+(.+)$`)

const slackHelp = "*skopos commands* — reply in an item's thread:\n" +
	"• `new <workspace>: <what>` — capture into a specific workspace (suffix match, e.g. `new skopos: ...`)\n" +
	"• `approve` / `lgtm` [`: <notes>`] — approve the plan at the gate\n" +
	"• `revise: <notes>` — request a re-plan\n" +
	"• `changes: <notes>` — request changes on a finished branch\n" +
	"• `done` — approve the review, mark the item done\n" +
	"• `retry: <answer>` — answer a blocker and requeue\n" +
	"• `reject` — discard the item\n" +
	"Top-level: `new: <what to do>` creates and queues an item."

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
		Tags:          []string{"slack-new"},
		AuthorAgentID: "slack",
	})
	if err != nil {
		r.post(ctx, "", "⚠️ Could not create the item: "+firstLine(err.Error()))
		return true
	}
	if _, qErr := r.inbox.Queue(relayCtx(), item.ID, audit.ViaSlack); qErr != nil {
		r.post(ctx, "", "⚠️ Created but not queued: "+firstLine(qErr.Error()))
	}
	r.bindThread(ctx, item.ID, ts)
	r.post(ctx, ts, "📥 Queued in `"+ws+"` · planning starts within a minute. The plan will come back in this thread.")
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
	cmd, arg := strings.ToLower(m[1]), strings.TrimSpace(m[2])
	item, err := r.inbox.GetItem(relayCtx(), itemID)
	if err != nil {
		return false
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
	return r.applyCommand(ctx, item, cmd, arg)
}

// applyCommand runs one parsed command against the current item state.
func (r *slackRelay) applyCommand(ctx context.Context, item *inbox.Item, cmd, arg string) bool {
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
		if arg != "" {
			if err := r.appendSection(item.ID, "## Approval notes", arg); err != nil {
				return fail(err)
			}
		}
		if _, err := r.inbox.Approve(relayCtx, item.ID, inbox.ApproveInput{Notes: arg, Via: audit.ViaSlack}); err != nil {
			if strings.Contains(err.Error(), "stale") {
				r.post(ctx, thread, "⚠️ The plan changed since the plan-ready message — a new plan-ready follows; approve that one.")
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
		if err := r.appendSection(item.ID, "## Review notes", arg); err != nil {
			return fail(err)
		}
		if _, err := r.inbox.RequestChanges(relayCtx, item.ID, arg, audit.ViaSlack); err != nil {
			return fail(err)
		}
		return true
	case "changes":
		if item.Status != inbox.StatusInReview {
			r.post(ctx, thread, "⚠️ changes applies to a finished branch — the item is "+statusWord(item.Status)+".")
			return true
		}
		if err := r.appendSection(item.ID, "## Change requests", arg); err != nil {
			return fail(err)
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
	msg := r.formatGate(itemID, from, to)
	if msg == "" {
		return
	}
	item, err := r.inbox.GetItem(context.Background(), itemID)
	if err != nil {
		return
	}
	if item.SlackThreadTS == "" {
		if ts := r.notifier.Notify(ctx, "", msg); ts != "" {
			r.bindThread(ctx, itemID, ts)
		}
	} else {
		r.notifier.Notify(ctx, item.SlackThreadTS, msg)
	}
	r.drain(ctx, itemID)
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
		body := sectionOf(item.Content, "## Enrichment")
		if q := sectionOf(item.Content, "## Questions"); q != "" {
			body += "\n\n*Questions for you:*\n" + q
		}
		if body == "" {
			body = "_Plan details in skopos._"
		}
		return "🧭 *Plan ready* · " + title + "\n\n" + body +
			"\n\n`approve` · `approve: <notes>` · `revise: <notes>` · `reject`"
	case inbox.StatusBlocked, inbox.StatusFailed:
		reason := ""
		if runs, err := r.runs.ListRuns(context.Background(), itemID); err == nil {
			for _, run := range runs {
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
	return false
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

// appendSection read-modify-writes a markdown section onto the item —
// content edits are legal in every workflow phase (0.9.3; approver-only past
// the plan gate, which the relay's principal holds), and the stage prompts
// read exactly these sections.
func (r *slackRelay) appendSection(itemID, heading, body string) error {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	item, err := r.inbox.GetItem(relayCtx(), itemID)
	if err != nil {
		return err
	}
	content := item.Content + "\n\n" + heading + "\n\n" + strings.TrimSpace(body) + "\n"
	return r.inbox.UpdateItem(relayCtx(), itemID, inbox.UpdateInput{Content: content, Via: audit.ViaSlack})
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
