package cmd

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/runs"
)

// Review findings in the Slack thread: `findings` lists them numbered,
// `waive <n>` accepts one, `changes <n,…>: notes` sends findings back, and
// the next gate message carries the list. A reply by number is never
// buffered, and after a restart the relay asks for the list again.
func TestRelayFindings(t *testing.T) {
	svc, runsSvc, plansSvc, fake, relay := relayStack(t)
	root := context.Background()
	item := mustReachGate(t, svc, plansSvc, "findings in slack")
	if _, err := svc.Approve(approverCtx(), item.ID, inbox.ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	if err := recordRunWithHead(t, runsSvc, item.ID, "abc123def456"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetSlackThread(root, item.ID, "900.1"); err != nil {
		t.Fatal(err)
	}
	state := func(text string) string {
		t.Helper()
		fs, err := svc.Findings(approverCtx(), item.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fs {
			if f.Text == text {
				return f.State
			}
		}
		return ""
	}
	say := func(text string) string {
		t.Helper()
		if !relay.Apply(root, item.ID, text) {
			t.Fatalf("%q must be handled", text)
		}
		return fake.last().text
	}

	// Nothing recorded yet.
	if got := say("findings"); got != "No review findings on this item." {
		t.Fatalf("no findings: %q", got)
	}
	// A second opinion leaves two findings nobody follows up.
	say("second codex")
	so, err := runsSvc.ClaimNext(root, runs.ClaimInput{WorkerID: "w-codex", Runners: []string{"codex"}, Workspaces: []string{"ws-a"}})
	if err != nil || so == nil {
		t.Fatalf("claim: %+v %v", so, err)
	}
	if _, err := runsSvc.Report(root, so.ID, runs.ReportInput{WorkerID: "w-codex", Status: runs.StatusSucceeded, Verdict: "fail", Summary: "missing auth check",
		Findings: []runs.Finding{{Severity: "nit", Text: "rename x"}, {Severity: "blocking", Text: "no checkAccess on the new action"}}}, so.LeaseToken); err != nil {
		t.Fatal(err)
	}
	list := say("findings")
	for _, want := range []string{
		"*Findings*",
		"1. _not followed up · blocking_ no checkAccess on the new action _(second opinion · codex)_", // blocking first
		"2. _not followed up · nit_ rename x",
		"`changes 2,3: <notes>` sends findings back to be fixed · `waive 2: <why>` accepts one",
	} {
		if !strings.Contains(list, want) {
			t.Fatalf("the list lacks %q:\n%s", want, list)
		}
	}

	// Prose that starts with a command word is not a command.
	if relay.Apply(root, item.ID, "changes needed soon") || relay.Apply(root, item.ID, "waive goodbye") {
		t.Fatal("prose is not a command")
	}
	if got := say("waive 9: no"); !strings.Contains(got, "No finding 9 — the list has 2.") {
		t.Fatalf("an unknown number: %q", got)
	}
	// A blocking finding is not accepted without a reason.
	if got := say("waive 1"); !strings.Contains(got, "a reason is required to waive a blocking finding") || state("no checkAccess on the new action") != "noted" {
		t.Fatalf("waive without a reason: %q", got)
	}
	if got := say("waive 2: cosmetic"); !strings.Contains(got, "Waived: rename x") || state("rename x") != "waived" {
		t.Fatalf("waive: %q, state %s", got, state("rename x"))
	}

	// `changes 1: notes` sends the blocking one back and starts the fix.
	say("changes 1: add the access check")
	if got, _ := svc.GetItem(root, item.ID); got.Status != inbox.StatusImplementing || !strings.Contains(got.Content, "add the access check") || !strings.Contains(got.Content, "Findings to fix:") {
		t.Fatalf("changes with a finding: %s\n%s", got.Status, got.Content)
	}
	if st := state("no checkAccess on the new action"); st != "open" {
		t.Fatalf("sent back: %s", st)
	}

	// Mid-run, a reply by number is not kept for later: the numbers are
	// those of the list shown now.
	if got := say("changes 1: again"); !strings.Contains(got, "applies at the gate") {
		t.Fatalf("a reply by number mid-run: %q", got)
	}

	// The fix is reported with the implementer's word; the gate message
	// carries the list with the state, and how to answer it.
	impl, err := runsSvc.ClaimNext(root, runs.ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if err != nil || impl == nil || impl.Stage != runs.StageImplement {
		t.Fatalf("claim the fix: %+v %v", impl, err)
	}
	fs, _ := svc.Findings(approverCtx(), item.ID)
	var blockID string
	for _, f := range fs {
		if f.Severity == "blocking" {
			blockID = f.ID
		}
	}
	if _, err := runsSvc.Report(root, impl.ID, runs.ReportInput{WorkerID: "w1", Status: runs.StatusSucceeded, HeadSHA: "def456abc789", Branch: "agent/x",
		FindingAnswers: []findings.Answer{{ID: blockID, Status: "fixed", Note: "added checkAccess"}}}, impl.LeaseToken); err != nil {
		t.Fatal(err)
	}
	gate := ""
	for _, m := range fake.texts() {
		if strings.Contains(m, "Branch ready") {
			gate = m
		}
	}
	for _, want := range []string{"*Findings*", "1. _fixed, not confirmed · blocking_ no checkAccess on the new action", "_1 waived_", "`changes 2,3: <notes>` sends findings back"} {
		if !strings.Contains(gate, want) {
			t.Fatalf("the gate message lacks %q:\n%s", want, gate)
		}
	}

	// After a restart the relay does not know which list was read.
	relay.mu.Lock()
	relay.shownFindings = nil
	relay.mu.Unlock()
	if got := say("waive 1: fine"); !strings.Contains(got, "reply `findings` for the current one") {
		t.Fatalf("after a restart: %q", got)
	}
	if st := state("no checkAccess on the new action"); st != "fixed" {
		t.Fatalf("nothing was waived blind: %s", st)
	}
}

// The list keeps to a budget of its own and numbers only what it shows;
// agent-written text cannot ping a channel or pose as a link; a list with
// nothing to pick says so; prose that starts with "findings" is left alone.
func TestRelayFindingsList(t *testing.T) {
	svc, runsSvc, plansSvc, fake, relay := relayStack(t)
	root := context.Background()
	item := mustReachGate(t, svc, plansSvc, "a long list")
	if _, err := svc.Approve(approverCtx(), item.ID, inbox.ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	if err := recordRunWithHead(t, runsSvc, item.ID, "abc123def456"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetSlackThread(root, item.ID, "900.1"); err != nil {
		t.Fatal(err)
	}
	if !relay.Apply(root, item.ID, "second codex") {
		t.Fatal("second codex must be handled")
	}
	so, err := runsSvc.ClaimNext(root, runs.ClaimInput{WorkerID: "w-codex", Runners: []string{"codex"}, Workspaces: []string{"ws-a"}})
	if err != nil || so == nil {
		t.Fatalf("claim: %+v %v", so, err)
	}
	many := []runs.Finding{{Severity: "blocking", Text: "<!channel> see <https://evil.example|the fix guide> & more"}}
	for i := 0; i < 14; i++ {
		many = append(many, runs.Finding{Severity: "nit", Text: strings.Repeat("a long finding that goes on ", 12)})
	}
	if _, err := runsSvc.Report(root, so.ID, runs.ReportInput{WorkerID: "w-codex", Status: runs.StatusSucceeded, Verdict: "fail", Summary: "many", Findings: many}, so.LeaseToken); err != nil {
		t.Fatal(err)
	}

	before := len(fake.texts())
	if relay.Apply(root, item.ID, "findings look reasonable to me") || len(fake.texts()) != before {
		t.Fatal("a sentence that starts with findings is neither a command nor answered")
	}

	if !relay.Apply(root, item.ID, "findings") {
		t.Fatal("findings must be handled")
	}
	list := fake.last().text
	if strings.Contains(list, "<!channel>") || strings.Contains(list, "<https://") || !strings.Contains(list, "&lt;!channel&gt; see &lt;https://evil.example|the fix guide&gt; &amp; more") {
		t.Fatalf("agent text must be escaped:\n%s", list)
	}
	if len(list) > 1500 {
		t.Fatalf("the list keeps to its budget: %d bytes", len(list))
	}
	shown := strings.Count(list, "_not followed up · ")
	if shown < 3 || shown >= 15 || !strings.Contains(list, "more (see the dashboard)") {
		t.Fatalf("a long list is cut and says so: %d shown\n%s", shown, list)
	}
	if !strings.HasSuffix(strings.TrimSpace(list), "`waive 2: <why>` accepts one") {
		t.Fatalf("the commands stay under the list:\n%s", list)
	}
	// Numbers past what was shown name nothing, though the record has more.
	relay.Apply(root, item.ID, "waive "+strconv.Itoa(shown+1)+": x")
	if got := fake.last().text; !strings.Contains(got, "No finding "+strconv.Itoa(shown+1)+" — the list has "+strconv.Itoa(shown)+".") {
		t.Fatalf("a number that was not shown: %q", got)
	}

	// With everything settled the list has nothing to pick, and says so.
	fs, _ := svc.Findings(approverCtx(), item.ID)
	for _, f := range fs {
		if _, err := svc.WaiveFinding(approverCtx(), item.ID, f.ID, "accepted", "cli"); err != nil {
			t.Fatal(err)
		}
	}
	relay.Apply(root, item.ID, "findings")
	if got := fake.last().text; !strings.Contains(got, "_15 waived_") || strings.Contains(got, "1. ") {
		t.Fatalf("all settled: %q", got)
	}
	relay.Apply(root, item.ID, "waive 1: x")
	if got := fake.last().text; !strings.Contains(got, "There is no finding to pick") {
		t.Fatalf("nothing to pick: %q", got)
	}
}
