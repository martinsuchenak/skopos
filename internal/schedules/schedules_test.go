package schedules

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/inbox"
	_ "modernc.org/sqlite"
)

type fakeOwners struct{ members map[string][]string }

func (f fakeOwners) ScheduleOwner(ctx context.Context, kind, ref string, write bool) (string, error) {
	if write {
		if err := auth.RequireApprover(ctx); err != nil {
			return "", err
		}
	}
	return ref, nil
}

func (f fakeOwners) OwnerWorkspaces(_ context.Context, kind, id string) ([]string, error) {
	if kind == KindGroup {
		return f.members[id], nil
	}
	return []string{id}, nil
}

type fakeItems struct {
	mu      sync.Mutex
	items   map[string]*inbox.Item
	created []inbox.CreateInput
	actors  []string
	fail    string
}

func (f *fakeItems) CreateItem(ctx context.Context, in inbox.CreateInput) (*inbox.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.WorkspaceID == f.fail {
		return nil, fmt.Errorf("invalid inbox input: workspace %s has no template %q", in.WorkspaceID, in.Template)
	}
	f.created = append(f.created, in)
	actor, _ := auth.Actor(ctx)
	f.actors = append(f.actors, actor)
	it := &inbox.Item{ID: fmt.Sprintf("item-%d", len(f.created)), WorkspaceID: in.WorkspaceID, Status: inbox.StatusQueued}
	f.items[it.ID] = it
	return it, nil
}

func (f *fakeItems) ReadItem(_ context.Context, id string) (*inbox.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		return it, nil
	}
	return nil, inbox.ErrNotFound
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "s.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.RunMigrations(conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

type notes struct {
	mu      sync.Mutex
	started []string
	text    []string
}

func (n *notes) ScheduleStarted(_ context.Context, itemID, schedule string) {
	n.mu.Lock()
	n.started = append(n.started, schedule+":"+itemID)
	n.mu.Unlock()
}
func (n *notes) ScheduleNote(_ context.Context, text string) {
	n.mu.Lock()
	n.text = append(n.text, text)
	n.mu.Unlock()
}

// A group schedule fires once per occurrence per member, queued from the
// template; a member whose last item is still open gets none; missed
// occurrences fire once; a refused create is a failed firing, said once.
func TestFireDue(t *testing.T) {
	items := &fakeItems{items: map[string]*inbox.Item{}, fail: "ws-c"}
	svc := NewService(testDB(t), fakeOwners{members: map[string][]string{"g1": {"ws-b", "ws-a", "ws-c"}}}, items)
	n := &notes{}
	svc.SetNotifier(n)
	clock := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return clock }
	approver := auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "k", Name: "alex", Approver: true, AllWorkspaces: true})

	plain := auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "p", Name: "agent", AllWorkspaces: true})
	if _, err := svc.Create(plain, Input{Name: "deps", OwnerKind: KindGroup, OwnerID: "g1", Template: "deps", Rhythm: "monthly"}, ""); err == nil {
		t.Fatal("a schedule needs the approver permission")
	}
	sc, err := svc.Create(approver, Input{Name: "Deps", OwnerKind: KindGroup, OwnerID: "g1", Template: "deps", Rhythm: "monthly", Timezone: "Europe/London"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Name != "deps" || !sc.Next.Equal(time.Date(2026, 4, 1, 5, 0, 0, 0, time.UTC)) || !sc.Enabled {
		t.Fatalf("created: %+v", sc)
	}
	if _, err := svc.Create(approver, Input{Name: "deps", OwnerKind: KindGroup, OwnerID: "g1", Template: "deps", Rhythm: "monthly"}, ""); err == nil || !strings.Contains(err.Error(), "already held") {
		t.Fatalf("a second schedule of the name: %v", err)
	}
	for _, bad := range []Input{
		{Name: "x", OwnerKind: KindGroup, OwnerID: "g1", Template: "deps", Rhythm: "every day"},
		{Name: "x", OwnerKind: KindGroup, OwnerID: "g1", Template: "deps", Rhythm: "daily", Timezone: "Mars/Olympus"},
		{Name: "x", OwnerKind: "team", OwnerID: "g1", Template: "deps", Rhythm: "daily"},
		{Name: "x y", OwnerKind: KindGroup, OwnerID: "g1", Template: "deps", Rhythm: "daily"},
	} {
		if _, err := svc.Create(approver, bad, ""); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}

	// Not due yet: nothing.
	svc.FireDue(context.Background())
	if len(items.created) != 0 {
		t.Fatal("nothing fires before its time")
	}
	// Two months later (the server was down for a month): one firing.
	clock = time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	svc.FireDue(context.Background())
	if len(items.created) != 2 || items.created[0].WorkspaceID != "ws-a" || items.created[1].WorkspaceID != "ws-b" ||
		items.created[0].Template != "deps" || items.created[0].Start != inbox.StartQueue || items.created[0].AuthorAgentID != "schedule:deps" {
		t.Fatalf("one item per member, from the template, queued: %+v", items.created)
	}
	if items.actors[0] != "schedule deps (root)" {
		t.Fatalf("started in the schedule's name: %q", items.actors[0])
	}
	if len(n.started) != 2 || len(n.text) != 1 || !strings.Contains(n.text[0], "ws-c") || !strings.Contains(n.text[0], `no template "deps"`) {
		t.Fatalf("notes: %v %v", n.started, n.text)
	}
	got, _ := svc.Get(approver, sc.ID)
	if !got.Next.Equal(time.Date(2026, 6, 1, 5, 0, 0, 0, time.UTC)) || !got.Last.Equal(time.Date(2026, 4, 1, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("moved past now, the missed one fired once: next %s last %s", got.Next, got.Last)
	}
	svc.FireDue(context.Background())
	if len(items.created) != 2 {
		t.Fatal("fired once per occurrence")
	}

	// Next month: ws-a's item is still open, ws-b's is done.
	items.items["item-2"].Status = inbox.StatusDone
	clock = time.Date(2026, 6, 1, 5, 1, 0, 0, time.UTC)
	svc.FireDue(context.Background())
	if len(items.created) != 3 || items.created[2].WorkspaceID != "ws-b" {
		t.Fatalf("only where the last item is closed: %+v", items.created)
	}
	fs, _ := svc.Firings(approver, sc.ID)
	var skipped string
	for _, f := range fs {
		if f.WorkspaceID == "ws-a" && f.Outcome == OutcomeSkipped {
			skipped = f.Reason
		}
	}
	if !strings.Contains(skipped, "item-1") || !strings.Contains(skipped, "still queued") {
		t.Fatalf("the skip says why: %q (%+v)", skipped, fs)
	}

	// Paused: nothing fires; run by hand: fires now, the rhythm unchanged.
	off := false
	if _, err := svc.Update(approver, sc.ID, Input{Enabled: &off}, ""); err != nil {
		t.Fatal(err)
	}
	clock = time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC)
	svc.FireDue(context.Background())
	if len(items.created) != 3 {
		t.Fatal("a paused schedule does not fire")
	}
	items.items["item-1"].Status = inbox.StatusDone
	ran, err := svc.RunNow(approver, sc.ID, "")
	if err != nil || len(ran) != 3 || len(items.created) != 4 || ran[1].Outcome != OutcomeSkipped {
		t.Fatalf("run by hand: %+v %v (%d created)", ran, err, len(items.created))
	}
	// Run by hand after the due time: the next due occurrence sees that
	// item and adds none.
	on := true
	if _, err := svc.Update(approver, sc.ID, Input{Enabled: &on}, ""); err != nil {
		t.Fatal(err)
	}
	for _, it := range items.items {
		it.Status = inbox.StatusDone
	}
	clock = time.Date(2026, 8, 1, 5, 0, 10, 0, time.UTC) // the occurrence was 05:00, the loop not yet run
	if _, err := svc.RunNow(approver, sc.ID, ""); err != nil {
		t.Fatal(err)
	}
	before := len(items.created)
	svc.FireDue(context.Background())
	if len(items.created) != before {
		t.Fatalf("an item started by hand after the due time is the open one: %d → %d", before, len(items.created))
	}

	// The key that holds the schedule no longer approves: nothing starts.
	for _, it := range items.items {
		it.Status = inbox.StatusDone
	}
	svc.SetHolderCheck(func(_ context.Context, keyID, ws string) bool { return keyID != "k" })
	clock = clock.Add(time.Minute)
	ran, err = svc.RunNow(approver, sc.ID, "")
	if err != nil || len(ran) != 3 || ran[0].Outcome != OutcomeSkipped || !strings.Contains(ran[0].Reason, "no longer approves") {
		t.Fatalf("a holder that lost the workspace: %+v %v", ran, err)
	}
	if err := svc.Delete(approver, sc.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(approver, sc.ID); err != ErrNotFound {
		t.Fatalf("deleted: %v", err)
	}
}
