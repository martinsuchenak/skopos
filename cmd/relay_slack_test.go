package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/martinsuchenak/skopos/internal/approvals"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/notify"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/runs"
	_ "modernc.org/sqlite"
)

// fakeProvider captures everything the relay would send to Slack.
type fakeProvider struct {
	mu   sync.Mutex
	n    int
	sent []fakeMessage
}

type fakeMessage struct {
	parent, text string
}

func (f *fakeProvider) Name() string                                    { return "fake" }
func (f *fakeProvider) Start(ctx context.Context, r notify.Relay) error { return nil }

func (f *fakeProvider) Send(ctx context.Context, parent, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.sent = append(f.sent, fakeMessage{parent: parent, text: text})
	return fmt.Sprintf("ts%d", f.n), nil
}

func (f *fakeProvider) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, m := range f.sent {
		out[i] = m.text
	}
	return out
}

func (f *fakeProvider) last() fakeMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

// relayStack wires inbox + plans + runs + the relay exactly like serve.go,
// with the fake provider as the Slack transport.
func relayStack(t *testing.T) (*inbox.Service, *runs.Service, *plans.Service, *fakeProvider, *slackRelay) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	auditSvc := audit.NewService(audit.NewStorage(sqlDB))
	approvalsSvc := approvals.NewService(approvals.NewStorage(sqlDB))
	plansSvc := plans.NewService(plans.NewStorage(sqlDB))
	inboxSvc := inbox.NewService(inbox.NewStorage(sqlDB))
	inboxSvc.SetAuditRecorder(auditSvc)
	inboxSvc.SetApprovalLog(approvalsSvc)
	inboxSvc.SetPlanRevisions(plansSvc)
	plansSvc.SetAmendmentGuard(inboxSvc.RequireAmendable)
	plansSvc.SetPlannableGuard(inboxSvc.RequirePlannable)
	plansSvc.SetDeletionGuard(inboxSvc.PlanHasWorkflowItem)

	runsSvc := runs.NewService(runs.NewStorage(sqlDB))
	runsSvc.SetItemTransition(func(ctx context.Context, itemID, to, reason, agentID string) error {
		_, err := inboxSvc.SystemTransition(ctx, itemID, inbox.Status(to), reason, agentID, audit.ViaWorker)
		return err
	})
	inboxSvc.SetRunQueuer(runsSvc)
	inboxSvc.SetRunLister(runsListerAdapter{runs: runsSvc})

	fake := &fakeProvider{}
	relay := &slackRelay{inbox: inboxSvc, runs: runsSvc, defaultWS: "ws-a"}
	notifySvc := notify.New(fake, relay)
	relay.SetNotifier(notifySvc)
	inboxSvc.SetItemNotifier(relay)
	return inboxSvc, runsSvc, plansSvc, fake, relay
}

func approverCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Root: true, Approver: true})
}

func mustRelayItem(t *testing.T, svc *inbox.Service, title string) *inbox.Item {
	t.Helper()
	item, err := svc.CreateItem(context.Background(), inbox.CreateInput{WorkspaceID: "ws-a", Title: title, AuthorAgentID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// Queue -> planning -> gate, with a linked plan and a snapshot revision
// (the Approve preconditions).
func mustReachGate(t *testing.T, svc *inbox.Service, plansSvc *plans.Service, title string) *inbox.Item {
	t.Helper()
	root := context.Background()
	item := mustRelayItem(t, svc, title)
	for _, fn := range []func() error{
		func() error { _, err := svc.Queue(approverCtx(), item.ID, ""); return err },
		func() error {
			_, err := svc.SystemTransition(root, item.ID, inbox.StatusPlanning, "", "", "")
			return err
		},
	} {
		if err := fn(); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := plansSvc.CreatePlan(root, plans.CreatePlanInput{WorkspaceID: "ws-a", Name: "p", AuthorAgentID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plansSvc.AddItem(root, plan.ID, plans.CreateItemInput{Title: "step 1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LinkPlan(root, item.ID, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := plansSvc.CreateRevision(root, plan.ID, "base1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, inbox.StatusAwaitingApproval, "", "", ""); err != nil {
		t.Fatal(err)
	}
	return item
}

// approve at the gate: with notes (appended as an Approval notes section the
// implementer prompt reads), action audited via=slack, implement run queued.
func TestRelayApprove(t *testing.T) {
	svc, runsSvc, plansSvc, fake, relay := relayStack(t)
	item := mustReachGate(t, svc, plansSvc, "relay approve")
	if !relay.Apply(context.Background(), item.ID, "approve: focus on the flaky tests") {
		t.Fatal("approve must be handled")
	}
	got, _ := svc.GetItem(context.Background(), item.ID)
	if got.Status != inbox.StatusApproved {
		t.Fatalf("status after approve: %s", got.Status)
	}
	if !strings.Contains(got.Content, "## Approval notes") || !strings.Contains(got.Content, "focus on the flaky tests") {
		t.Fatalf("approval notes must land on the item: %q", got.Content)
	}
	runsList, _ := runsSvc.List(context.Background(), "", item.ID)
	hasImplement := false
	for _, r := range runsList {
		if r.Stage == runs.StageImplement {
			hasImplement = true
		}
	}
	if !hasImplement {
		t.Fatal("approve must queue the implement run")
	}
	// The gate notification started the thread before the command could
	// reply (NotifyItem fired on reaching awaiting_approval).
	if !strings.Contains(strings.Join(fake.texts(), "\n"), "Plan ready") {
		t.Fatalf("gate notification missing: %v", fake.texts())
	}
}

// A reply mid-run is buffered, then applied when the item reaches a gate —
// the agent-trial mid-run guard, socket-safe.
func TestRelayMidRunBuffer(t *testing.T) {
	svc, _, plansSvc, fake, relay := relayStack(t)
	root := context.Background()
	item := mustReachGate(t, svc, plansSvc, "buffered reject")
	if _, err := svc.Approve(approverCtx(), item.ID, inbox.ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, inbox.StatusImplementing, "", "", ""); err != nil {
		t.Fatal(err)
	}
	fake.sent = nil
	if !relay.Apply(root, item.ID, "reject") {
		t.Fatal("mid-run reject must be accepted (buffered)")
	}
	if !strings.Contains(fake.last().text, "mid-run") {
		t.Fatalf("buffering must tell Martin: %q", fake.last().text)
	}
	got, _ := svc.GetItem(root, item.ID)
	if got.Status != inbox.StatusImplementing {
		t.Fatalf("mid-run command must not apply immediately: %s", got.Status)
	}
	// The run finishes -> in_review -> the buffered reject applies via drain.
	if _, err := svc.SystemTransition(root, item.ID, inbox.StatusInReview, "", "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetItem(root, item.ID)
	if got.Status != inbox.StatusDiscarded {
		t.Fatalf("buffered reject must apply at the gate: %s", got.Status)
	}
	if !strings.Contains(strings.Join(fake.texts(), "\n"), "Rejected") {
		t.Fatalf("outcome must be posted: %v", fake.texts())
	}
}

// done pulls the review head from the item's latest successful implement
// run — the approval names exactly that commit.
func TestRelayDoneUsesRunHead(t *testing.T) {
	svc, runsSvc, plansSvc, _, relay := relayStack(t)
	root := context.Background()
	item := mustReachGate(t, svc, plansSvc, "done head")
	if _, err := svc.Approve(approverCtx(), item.ID, inbox.ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	// Claim + report the queued runs the way the worker does: the claim
	// moves the item to implementing, the implement report's chain moves
	// it to in_review with the head recorded.
	if err := recordRunWithHead(t, runsSvc, item.ID, "abc123def456"); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetItem(root, item.ID)
	if got.Status != inbox.StatusInReview {
		t.Fatalf("chain must walk the item to in_review: %s", got.Status)
	}
	if !relay.Apply(root, item.ID, "done") {
		t.Fatal("done must be handled")
	}
	after, _ := svc.GetItem(root, item.ID)
	if after.Status != inbox.StatusDone {
		t.Fatalf("status after done: %s", got.Status)
	}
}

// changes past the plan gate: the relay's content append is a post-approval
// edit (approver-only since review M7) — the relay principal holds the
// permission, so the section lands, is audited via=slack, and the item goes
// back to implementing.
func TestRelayChangesAfterApproval(t *testing.T) {
	svc, runsSvc, plansSvc, _, relay := relayStack(t)
	root := context.Background()
	item := mustReachGate(t, svc, plansSvc, "changes after approval")
	if _, err := svc.Approve(approverCtx(), item.ID, inbox.ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	if err := recordRunWithHead(t, runsSvc, item.ID, "abc123def456"); err != nil {
		t.Fatal(err)
	}
	if !relay.Apply(root, item.ID, "changes: handle the empty-list case") {
		t.Fatal("changes must be handled")
	}
	got, _ := svc.GetItem(root, item.ID)
	if got.Status != inbox.StatusImplementing {
		t.Fatalf("status after changes: %s", got.Status)
	}
	if !strings.Contains(got.Content, "## Change requests") || !strings.Contains(got.Content, "handle the empty-list case") {
		t.Fatalf("change request must land on the item: %q", got.Content)
	}
	entries, err := svc.Timeline(root, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	audited := false
	for _, e := range entries {
		if e.Action == "inbox.edit" && e.Via == audit.ViaSlack {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("the relay's content edit must be audited via=slack: %+v", entries)
	}
}

// new: capture — creates + queues the item and binds the thread to Martin's
// own message; help and non-command text behave.
func TestRelayCapture(t *testing.T) {
	svc, _, _, fake, relay := relayStack(t)
	if !relay.TopLevel(context.Background(), "new: fix the flaky scheduler test", "777.1") {
		t.Fatal("new: must be handled")
	}
	items, err := svc.ListItems(context.Background(), "ws-a", "", "slack-new", "")
	if err != nil || len(items) != 1 {
		t.Fatalf("captured item: %v err %v", items, err)
	}
	if items[0].Status != inbox.StatusQueued {
		t.Fatalf("captured item must be queued: %s", items[0].Status)
	}
	if items[0].SlackThreadTS != "777.1" {
		t.Fatalf("thread must bind to Martin's message: %q", items[0].SlackThreadTS)
	}
	if !strings.Contains(fake.last().text, "Queued") {
		t.Fatalf("capture confirmation: %v", fake.texts())
	}
	// Restart recovery: the provider's in-memory map is empty, but the
	// binding persists on the item.
	if got := relay.ItemForThread("777.1"); got != items[0].ID {
		t.Fatalf("ItemForThread: %q", got)
	}
	// help answers; a non-command reply is left un-acked.
	if !relay.TopLevel(context.Background(), "help", "778") {
		t.Fatal("help must be handled")
	}
	if relay.Apply(context.Background(), items[0].ID, "what is the status?") {
		t.Fatal("non-command text must not be acked")
	}
}

// recordRunWithHead simulates the worker without a live one: claim the
// item's already-queued runs (the plan run Approve's flow left, then the
// implement run) and report them, stamping the head on the implement one.
func recordRunWithHead(t *testing.T, runsSvc *runs.Service, itemID, head string) error {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		claimed, err := runsSvc.ClaimNext(ctx, runs.ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
		if err != nil {
			return err
		}
		if claimed == nil {
			return fmt.Errorf("no implement run to claim")
		}
		if claimed.ItemID != itemID {
			continue
		}
		in := runs.ReportInput{WorkerID: "w1", Status: runs.StatusSucceeded, Branch: "agent/x"}
		if claimed.Stage == runs.StageImplement {
			in.HeadSHA = head
		}
		if _, err := runsSvc.Report(ctx, claimed.ID, in, claimed.LeaseToken); err != nil {
			return err
		}
		if claimed.Stage == runs.StageImplement {
			return nil
		}
	}
	return fmt.Errorf("implement run never claimed")
}

// `new <workspace>: <what>` files into the selected workspace (unique
// suffix match); an unknown prefix stays part of the title.
func TestRelayCaptureWorkspaceSelector(t *testing.T) {
	svc, _, _, fake, relay := relayStack(t)
	relay.resolveWS = func(sel string) (string, bool) {
		if sel == "mail-processor" {
			return "github.com/fortix/mail-processor", true
		}
		return "", false
	}
	if !relay.TopLevel(context.Background(), "new mail-processor: fix the attachment parser", "800.1") {
		t.Fatal("selector capture must be handled")
	}
	items, err := svc.ListItems(context.Background(), "github.com/fortix/mail-processor", "", "slack-new", "")
	if err != nil || len(items) != 1 {
		t.Fatalf("item in the selected workspace: %v err %v", items, err)
	}
	if items[0].Title != "fix the attachment parser" {
		t.Fatalf("selector must not leak into the title: %q", items[0].Title)
	}
	if items[0].SlackThreadTS != "800.1" {
		t.Fatalf("thread binding: %q", items[0].SlackThreadTS)
	}
	// Unknown prefix falls through as content (default workspace).
	if !relay.TopLevel(context.Background(), "new fix: the login bug", "801.1") {
		t.Fatal("plain capture must still work")
	}
	def, err := svc.ListItems(context.Background(), "ws-a", "", "slack-new", "")
	if err != nil || len(def) != 1 {
		t.Fatalf("fallback capture: %v err %v", def, err)
	}
	if def[0].Title != "fix: the login bug" {
		t.Fatalf("unknown prefix must stay in the title: %q", def[0].Title)
	}
	_ = fake
}

// An approval sent mid-run is refused, never buffered: buffered, it would
// apply the instant the gate is reached — approving a plan Martin has not
// seen (review H2). Non-advancing replies (reject) still buffer.
func TestRelayMidRunApprovalNotBuffered(t *testing.T) {
	svc, _, _, fake, relay := relayStack(t)
	root := context.Background()
	item := mustRelayItem(t, svc, "early approve")
	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, inbox.StatusPlanning, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"approve", "lgtm: ship it", "done"} {
		if !relay.Apply(root, item.ID, cmd) {
			t.Fatalf("%q must be answered", cmd)
		}
		if !strings.Contains(fake.last().text, "applies at the gate") {
			t.Fatalf("%q mid-run must be refused with a hint: %q", cmd, fake.last().text)
		}
	}
	relay.mu.Lock()
	n := len(relay.pending[item.ID])
	relay.mu.Unlock()
	if n != 0 {
		t.Fatalf("approvals must not be buffered: %d pending", n)
	}
}

// Commands are the bare word or `word: args`; casual sentences that merely
// start with a command word are not applied (review M1).
func TestRelayCommandParsing(t *testing.T) {
	cases := map[string]string{ // text -> parsed command ("" = not a command)
		"approve":                        "approve",
		"  LGTM!  ":                      "lgtm",
		"done.":                          "done",
		"approve: focus on tests":        "approve",
		"retry:\nuse the v2 endpoint":    "retry",
		"reject : dup of #12":            "reject",
		"Done with lunch":                "",
		"done?":                          "",
		"approve? not sure about step 3": "",
		"reject, wait no":                "",
		"Approve-ish":                    "",
		"what is the status?":            "",
	}
	for text, want := range cases {
		got := ""
		if m := reCmd.FindStringSubmatch(text); m != nil && isCommand(strings.ToLower(m[1])) {
			got = strings.ToLower(m[1])
		}
		if got != want {
			t.Errorf("%q parsed as %q, want %q", text, got, want)
		}
	}
	// Capture needs `new:` or `new ` — "news: ..." is not a capture.
	for text, want := range map[string]bool{
		"new: fix it": true, "new fix it": true, "New skopos: x": true,
		"news: CI is green": false, "newer build?": false,
	} {
		if got := reNew.MatchString(text); got != want {
			t.Errorf("reNew(%q) = %v, want %v", text, got, want)
		}
	}
}

// A leading command word that does not parse gets a syntax hint, not an
// action; a multi-line capture still honors the workspace selector.
func TestRelayHintAndMultilineSelector(t *testing.T) {
	svc, _, plansSvc, fake, relay := relayStack(t)
	item := mustReachGate(t, svc, plansSvc, "hint")
	if relay.Apply(context.Background(), item.ID, "approve? not sure about step 3") {
		t.Fatal("a non-command must not be acked")
	}
	if got, _ := svc.GetItem(context.Background(), item.ID); got.Status != inbox.StatusAwaitingApproval {
		t.Fatalf("a non-command must not approve: %s", got.Status)
	}
	if !strings.Contains(fake.last().text, "Not applied") {
		t.Fatalf("expected a syntax hint: %q", fake.last().text)
	}
	relay.resolveWS = func(sel string) (string, bool) { return "github.com/x/skopos", sel == "skopos" }
	if !relay.TopLevel(context.Background(), "new skopos: fix the parser\nmore detail here", "900.1") {
		t.Fatal("capture must be handled")
	}
	items, err := svc.ListItems(context.Background(), "github.com/x/skopos", "", "slack-new", "")
	if err != nil || len(items) != 1 {
		t.Fatalf("multi-line capture must land in the selected workspace: %v err %v", items, err)
	}
	if !strings.Contains(fake.last().text, "github.com/x/skopos") {
		t.Fatalf("confirmation must name the workspace: %q", fake.last().text)
	}
}
