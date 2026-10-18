package inbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
)

// stubQueuer is a RunQueuer whose QueueStage can fail or run a hook (to
// simulate a racing action between the queue and the transition).
type stubQueuer struct {
	mu        sync.Mutex
	queueErr  error
	onQueue   func()
	cancelled []string
	n         int
}

func (q *stubQueuer) QueueStage(ctx context.Context, itemID, workspaceID, stage, planRevID, baseSHA, branch string) (string, error) {
	q.mu.Lock()
	q.n++
	id := "run-" + string(rune('0'+q.n))
	err, hook := q.queueErr, q.onQueue
	q.mu.Unlock()
	if err != nil {
		return "", err
	}
	if hook != nil {
		hook()
	}
	return id, nil
}

func (q *stubQueuer) CancelRun(ctx context.Context, runID, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cancelled = append(q.cancelled, runID)
	return nil
}
func (q *stubQueuer) CancelItem(ctx context.Context, itemID, reason string) error { return nil }
func (q *stubQueuer) ResumeItem(ctx context.Context, itemID string) error         { return nil }

// agentCtx is a scoped key WITHOUT the approver permission (agent/worker).
func agentCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{
		KeyID: "k-agent", Name: "agent",
		Workspaces: map[string]struct{}{"ws-a": {}},
	})
}

func mustBlockedInPlanning(t *testing.T, svc *Service, title string) *Item {
	t.Helper()
	root := context.Background()
	item := mustCreate(t, svc, root, title)
	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusPlanning, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusBlocked, "question", "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetItem(root, item.ID)
	return got
}

// Review M6: a failed queue leaves the content untouched, and the next
// (successful) retry appends exactly one Answers section.
func TestRetryFailedQueueDoesNotAppend(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item := mustBlockedInPlanning(t, svc, "retry queue fails")
	q := &stubQueuer{queueErr: errors.New("queue down")}
	svc.SetRunQueuer(q)

	if _, err := svc.Retry(approverCtx(), item.ID, "first answer", ""); err == nil {
		t.Fatal("retry must fail when the queue fails")
	}
	got, _ := svc.GetItem(root, item.ID)
	if got.Content != item.Content || got.Status != StatusBlocked {
		t.Fatalf("failed retry must leave the item untouched: status %s content %q", got.Status, got.Content)
	}

	q.mu.Lock()
	q.queueErr = nil
	q.mu.Unlock()
	if _, err := svc.Retry(approverCtx(), item.ID, "second answer", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetItem(root, item.ID)
	if n := strings.Count(got.Content, "## Answers"); n != 1 || strings.Contains(got.Content, "first answer") || !strings.Contains(got.Content, "second answer") {
		t.Fatalf("exactly the successful answer must land once (%d sections): %q", n, got.Content)
	}
}

// Review M6: a transition that fails after the run was queued (the item
// moved underneath — here a racing reject) rolls the answer back with it and
// cancels the run it queued.
func TestRetryFailedTransitionRollsBackAnswer(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item := mustBlockedInPlanning(t, svc, "retry transition fails")
	q := &stubQueuer{}
	q.onQueue = func() {
		if _, err := svc.Reject(approverCtx(), item.ID, "racing reject", ""); err != nil {
			t.Errorf("racing reject: %v", err)
		}
	}
	svc.SetRunQueuer(q)

	if _, err := svc.Retry(approverCtx(), item.ID, "lost answer", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("retry over a discarded item must fail the matrix check: %v", err)
	}
	got, _ := svc.GetItem(root, item.ID)
	if strings.Contains(got.Content, "## Answers") {
		t.Fatalf("failed transition must not leave the answer behind: %q", got.Content)
	}
	if len(q.cancelled) != 1 {
		t.Fatalf("the queued run must be cancelled: %v", q.cancelled)
	}
}

// Review M6: concurrent retries — exactly one wins, and the content holds
// exactly the winner's answer (the loser fails the matrix check instead of
// silently losing its answer or duplicating the section).
func TestConcurrentRetriesKeepOneAnswer(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item := mustBlockedInPlanning(t, svc, "concurrent retry")
	// Barrier: both retries pass their pre-check and queue before either
	// transitions — the interleaving that lost an answer before M6.
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	go func() {
		<-arrived
		<-arrived
		close(release)
	}()
	svc.SetRunQueuer(&stubQueuer{onQueue: func() {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}})

	answers := []string{"answer from slack", "answer from dashboard"}
	errs := make([]error, len(answers))
	var wg sync.WaitGroup
	for i, a := range answers {
		wg.Add(1)
		go func(i int, a string) {
			defer wg.Done()
			_, errs[i] = svc.Retry(approverCtx(), item.ID, a, "")
		}(i, a)
	}
	wg.Wait()

	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner != -1 {
				t.Fatal("both retries succeeded")
			}
			winner = i
		}
	}
	if winner == -1 {
		t.Fatalf("one retry must succeed: %v", errs)
	}
	got, _ := svc.GetItem(root, item.ID)
	if n := strings.Count(got.Content, "## Answers"); n != 1 {
		t.Fatalf("exactly one Answers section, got %d: %q", n, got.Content)
	}
	if !strings.Contains(got.Content, answers[winner]) || strings.Contains(got.Content, answers[1-winner]) {
		t.Fatalf("content must hold exactly the winner's answer: %q", got.Content)
	}
}

func TestRetryAnswerCap(t *testing.T) {
	svc, _, _ := fullStack(t)
	item := mustBlockedInPlanning(t, svc, "retry cap")
	if _, err := svc.Retry(approverCtx(), item.ID, strings.Repeat("x", maxAnswerLen+1), ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized answer must be refused: %v", err)
	}
	got, _ := svc.GetItem(context.Background(), item.ID)
	if got.Status != StatusBlocked {
		t.Fatalf("refused retry must not move the item: %s", got.Status)
	}
}

// Review M7: past the plan gate, a non-approver key (agent/worker) cannot
// rewrite what the implementer reads; approver edits pass and are audited.
func TestPostApprovalEditsRequireApprover(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()

	// Before the gate, agents enrich freely.
	pre := mustCreate(t, svc, root, "pre-gate")
	if _, err := svc.Queue(approverCtx(), pre.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateItem(agentCtx(), pre.ID, UpdateInput{Content: "enriched by agent"}); err != nil {
		t.Fatalf("pre-approval agent edit: %v", err)
	}

	item := mustReadyForApproval(t, svc, plansSvc, "post-gate")
	if _, err := svc.Approve(approverCtx(), item.ID, ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Content: "rewritten by agent"}); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("approved item: agent content edit must be refused: %v", err)
	}
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Title: "retitled by agent"}); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("approved item: agent title edit must be refused: %v", err)
	}
	// Appending keeps the approved content intact — the worker records
	// review verdicts and amendment reasons this way with its executor key
	// (AppendSection: old content + a new section), so it must pass. A
	// "prefix" that drops part of the approved text is still a rewrite.
	before, _ := svc.GetItem(root, item.ID)
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Content: before.Content + "\n\n## review — acct\n\n**Verdict:** pass\n"}); err != nil {
		t.Fatalf("post-approval append by the executor must pass: %v", err)
	}
	appended, _ := svc.GetItem(root, item.ID)
	if !strings.HasPrefix(appended.Content, before.Content) || !strings.Contains(appended.Content, "**Verdict:** pass") {
		t.Fatalf("append must land after the approved content: %q", appended.Content)
	}
	if len(before.Content) > 1 {
		if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Content: before.Content[:len(before.Content)/2] + "\n\n## sneaky\n"}); !errors.Is(err, auth.ErrApproverRequired) {
			t.Fatalf("truncating the approved content is a rewrite: %v", err)
		}
	}
	// Re-sending the unchanged content (a tags-only form save) is not a
	// content edit.
	tags := []string{"triaged"}
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Content: appended.Content + "\n", Tags: &tags}); err != nil {
		t.Fatalf("tags-only edit must pass: %v", err)
	}

	// Blocked after approval: the status alone no longer says "approved";
	// the approval log does.
	if _, err := svc.SystemTransition(root, item.ID, StatusImplementing, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusBlocked, "question", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Content: "rewritten while blocked"}); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("blocked-after-approval: agent edit must be refused: %v", err)
	}

	// The approver may edit, and the edit is audited.
	if err := svc.UpdateItem(approverCtx(), item.ID, UpdateInput{Content: "clarified by Martin", Via: audit.ViaDashboard}); err != nil {
		t.Fatalf("approver edit: %v", err)
	}
	got, _ := svc.GetItem(root, item.ID)
	if got.Content != "clarified by Martin" {
		t.Fatalf("approver edit must land: %q", got.Content)
	}
	entries, err := svc.Timeline(root, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var edits []audit.Entry
	for _, e := range entries {
		if e.Action == "inbox.edit" {
			edits = append(edits, e)
		}
	}
	// Newest first: the approver's content edit, the agent's tags edit, then
	// the executor's append (audited like any edit).
	if len(edits) != 3 || edits[0].Via != audit.ViaDashboard || !strings.Contains(edits[0].Notes, "content") ||
		!strings.Contains(edits[1].Notes, "tags") || strings.Contains(edits[1].Notes, "content") ||
		!strings.Contains(edits[2].Notes, "content") {
		t.Fatalf("workflow edits must be audited with their fields: %+v", edits)
	}
}

// Review M7: a workflow item cannot move workspace (runs, plan and branch
// live in the old one); re-sending the current workspace is a no-op, and
// manual items still re-file.
func TestWorkflowItemCannotMoveWorkspace(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "pinned")
	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateItem(root, item.ID, UpdateInput{WorkspaceID: "ws-b"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("workflow item workspace move must be refused (root included): %v", err)
	}
	if err := svc.UpdateItem(root, item.ID, UpdateInput{WorkspaceID: "ws-a", Content: "same workspace"}); err != nil {
		t.Fatalf("same-workspace update must pass: %v", err)
	}
	got, _ := svc.GetItem(root, item.ID)
	if got.WorkspaceID != "ws-a" {
		t.Fatalf("workspace must not change: %s", got.WorkspaceID)
	}

	manual := mustCreate(t, svc, root, "refile")
	if err := svc.UpdateItem(root, manual.ID, UpdateInput{WorkspaceID: "ws-b"}); err != nil {
		t.Fatalf("manual item re-file: %v", err)
	}
}

// Human notes reach the agents from every surface (dashboard, REST, CLI —
// not just the Slack relay): approve notes become "## Approval notes",
// request-changes at the plan gate "## Review notes" and at review
// "## Change requests", appended with the transition.
func TestHumanNotesAppendedForAgents(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()

	item := mustReadyForApproval(t, svc, plansSvc, "notes")
	if _, err := svc.RequestChanges(approverCtx(), item.ID, "split step 2", audit.ViaDashboard); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetItem(root, item.ID)
	if got.Status != StatusPlanning || !strings.Contains(got.Content, "## Review notes\n\nsplit step 2") {
		t.Fatalf("re-plan notes: %s %q", got.Status, got.Content)
	}

	item2 := mustReadyForApproval(t, svc, plansSvc, "approve notes")
	if _, err := svc.Approve(approverCtx(), item2.ID, ApproveInput{Notes: "1) a — keep the old API", Via: audit.ViaDashboard}); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetItem(root, item2.ID)
	if !strings.Contains(got.Content, "## Approval notes\n\n1) a — keep the old API") {
		t.Fatalf("approval notes: %q", got.Content)
	}
	// No notes, no section.
	item3 := mustReadyForApproval(t, svc, plansSvc, "plain approve")
	if _, err := svc.Approve(approverCtx(), item3.ID, ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetItem(root, item3.ID); strings.Contains(got.Content, "## Approval notes") {
		t.Fatalf("empty notes must add nothing: %q", got.Content)
	}
	// At review: changes go to the implementer.
	if _, err := svc.SystemTransition(root, item2.ID, StatusImplementing, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item2.ID, StatusInReview, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestChanges(approverCtx(), item2.ID, "handle nil input", audit.ViaDashboard); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetItem(root, item2.ID)
	if got.Status != StatusImplementing || !strings.Contains(got.Content, "## Change requests\n\nhandle nil input") {
		t.Fatalf("change requests: %s %q", got.Status, got.Content)
	}
}

// The per-item agents switch: validated, stored typed (NULL = repository
// default), clearable with "" or "default"; on an agent-workflow item it is
// approver-only and audited like any workflow edit.
func TestItemAgentsSwitch(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()

	if _, err := svc.CreateItem(root, CreateInput{WorkspaceID: "ws-a", Title: "x", AuthorAgentID: "a", Agents: "crowd"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown agents value must be refused: %v", err)
	}
	item, err := svc.CreateItem(root, CreateInput{WorkspaceID: "ws-a", Title: "panel item", AuthorAgentID: "a", Agents: " Panel "})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetItem(root, item.ID); got.Agents != AgentsPanel {
		t.Fatalf("created with panel: %q", got.Agents)
	}
	// Manual item: any scoped key may set it; "default" clears it.
	solo, clear := "solo", "default"
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Agents: &solo}); err != nil {
		t.Fatalf("manual item switch: %v", err)
	}
	if err := svc.UpdateItem(root, item.ID, UpdateInput{Agents: &clear}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetItem(root, item.ID); got.Agents != "" {
		t.Fatalf("default clears the switch: %q", got.Agents)
	}

	// Workflow item: approver-only, audited.
	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Agents: &solo}); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("agent key switching a workflow item must be refused: %v", err)
	}
	if err := svc.UpdateItem(approverCtx(), item.ID, UpdateInput{Agents: &solo, Via: audit.ViaSlack}); err != nil {
		t.Fatalf("approver switch: %v", err)
	}
	if got, _ := svc.GetItem(root, item.ID); got.Agents != AgentsSolo {
		t.Fatalf("approver switch must land: %q", got.Agents)
	}
	entries, err := svc.Timeline(root, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "inbox.edit" && e.Via == audit.ViaSlack && strings.Contains(e.Notes, "agents") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the switch must be audited: %+v", entries)
	}
	// Re-sending the current value is not an edit (no approver needed).
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Agents: &solo}); err != nil {
		t.Fatalf("unchanged switch is a no-op: %v", err)
	}
}
