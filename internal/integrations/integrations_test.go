package integrations

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	return sqlDB
}

// The Standard Webhooks reference vector.
func TestSignMatchesStandardWebhooks(t *testing.T) {
	secret, _ := base64.StdEncoding.DecodeString("MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	got := Sign([][]byte{secret}, "msg_p5jXN8AQM9LWM0D4loKWxJek", 1614265330, []byte(`{"test": 2432232314}`))
	if got != "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=" {
		t.Fatalf("signature = %s", got)
	}
	if two := Sign([][]byte{secret, []byte("old")}, "m", 1, nil); strings.Count(two, "v1,") != 2 || !strings.Contains(two, " ") {
		t.Fatalf("two secrets: %s", two)
	}
}

// receiver records requests and answers with the next queued code (200
// when none).
type receiver struct {
	mu    sync.Mutex
	codes []int
	got   []*http.Request
	body  [][]byte
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, req)
	r.body = append(r.body, b)
	code := 200
	if len(r.codes) > 0 {
		code, r.codes = r.codes[0], r.codes[1:]
	}
	w.WriteHeader(code)
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

type stack struct {
	db    *sql.DB
	svc   *Service
	disp  *Dispatcher
	em    *Emitter
	clock time.Time
	rcv   *receiver
	url   string
}

func newStack(t *testing.T) *stack {
	t.Helper()
	sqlDB := testDB(t)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	loopback, _ := ParseCIDRs([]string{"127.0.0.1"})
	st := &stack{db: sqlDB, em: NewEmitter(), clock: time.Now(), rcv: rcv, url: srv.URL + "/hooks"}
	st.svc = NewService(sqlDB, st.em, "test-integrations-key", URLPolicy{AllowHTTP: []string{u.Hostname()}, AllowPrivate: loopback})
	st.svc.PublicURL = "https://skopos.example"
	st.svc.now = func() time.Time { return st.clock }
	st.em.now = st.svc.now
	st.disp = NewDispatcher(st.svc, nil)
	return st
}

func root() context.Context { return context.Background() }

func (st *stack) emit(t *testing.T, evs ...Event) {
	t.Helper()
	if err := st.em.Emit(root(), st.db, evs...); err != nil {
		t.Fatal(err)
	}
}

func (st *stack) run(t *testing.T) {
	t.Helper()
	if err := st.disp.RunOnce(root()); err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestDeliverySignedAndFiltered(t *testing.T) {
	st := newStack(t)
	created, err := st.svc.Create(root(), Input{Name: ptr("jobs"), URL: ptr(st.url), Workspaces: ptr([]string{"ws"})})
	if err != nil {
		t.Fatal(err)
	}
	// Events from before the subscription aren't its; other workspaces and
	// opt-in types aren't either.
	st.emit(t,
		Event{Type: TypeItemStatusChanged, WorkspaceID: "ws", ItemID: "i1", Data: map[string]any{"status": "queued"}},
		Event{Type: TypeItemStatusChanged, WorkspaceID: "other", ItemID: "i2"},
		Event{Type: TypeRunStarted, WorkspaceID: "ws", ItemID: "i1"},
	)
	st.run(t)
	st.run(t)
	if st.rcv.count() != 1 {
		t.Fatalf("deliveries = %d, want 1", st.rcv.count())
	}
	req, body := st.rcv.got[0], st.rcv.body[0]
	secret, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(created.Secret, "whsec_"))
	ts, _ := strconv.ParseInt(req.Header.Get("webhook-timestamp"), 10, 64)
	if want := Sign([][]byte{secret}, req.Header.Get("webhook-id"), ts, body); req.Header.Get("webhook-signature") != want {
		t.Fatalf("signature %q, want %q", req.Header.Get("webhook-signature"), want)
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	links, _ := p.Data["links"].(map[string]any)
	if p.Type != TypeItemStatusChanged || p.Data["item_id"] != "i1" || p.Data["status"] != "queued" ||
		links["api"] != "https://skopos.example/api/inbox/i1/state" || !strings.HasPrefix(req.Header.Get("webhook-id"), "msg_") {
		t.Fatalf("payload: %s", body)
	}

	// Naming run.started gets it.
	if _, err := st.svc.Update(root(), created.Webhook.ID, Input{Types: ptr([]string{TypeRunStarted})}); err != nil {
		t.Fatal(err)
	}
	st.emit(t, Event{Type: TypeRunStarted, WorkspaceID: "ws", ItemID: "i1"}, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "i3"})
	st.run(t)
	if st.rcv.count() != 2 {
		t.Fatalf("after types: %d", st.rcv.count())
	}
}

func TestRetriesDisableAndRedeliver(t *testing.T) {
	st := newStack(t)
	created, err := st.svc.Create(root(), Input{Name: ptr("jobs"), URL: ptr(st.url), Workspaces: ptr([]string{"ws"})})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Webhook.ID
	st.rcv.codes = []int{500, 503}
	st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "i1"})
	st.run(t)
	ds, _ := st.svc.Deliveries(root(), id, "", 0)
	if len(ds) != 1 || ds[0].State != StatePending || ds[0].Attempts != 1 || ds[0].LastCode != 500 || len(ds[0].Log) != 1 {
		t.Fatalf("after a 500: %+v", ds)
	}
	st.run(t) // not due yet
	if st.rcv.count() != 1 {
		t.Fatalf("retried early: %d", st.rcv.count())
	}
	st.clock = st.clock.Add(6 * time.Second)
	st.run(t)
	st.clock = st.clock.Add(6 * time.Minute)
	st.run(t)
	ds, _ = st.svc.Deliveries(root(), id, "", 0)
	if ds[0].State != StateDelivered || ds[0].Attempts != 3 || len(ds[0].Log) != 3 {
		t.Fatalf("after retries: %+v", ds[0])
	}
	if w, _ := st.svc.Get(root(), id); w.FailingSince != nil {
		t.Fatal("a success clears the failure mark")
	}

	// A run of failed attempts over 3 days — a new event every 9 hours,
	// none delivered — disables it; events wait.
	st.rcv.codes = slices.Repeat([]int{500}, 200)
	for i := 0; i < 10; i++ {
		st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "f" + strconv.Itoa(i)})
		st.run(t)
		st.clock = st.clock.Add(9 * time.Hour)
	}
	w, _ := st.svc.Get(root(), id)
	if w.Enabled || !strings.Contains(w.DisabledReason, "3 days") {
		t.Fatalf("not disabled: %+v", w)
	}
	st.rcv.mu.Lock()
	st.rcv.codes = nil
	st.rcv.mu.Unlock()
	before := st.rcv.count()
	st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "i3"})
	st.run(t)
	if st.rcv.count() != before {
		t.Fatal("a disabled webhook was delivered to")
	}
	// Re-enabled: the event that waited goes out, with the retries that
	// came due meanwhile; the rest at their next retries.
	if _, err := st.svc.Update(root(), id, Input{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ { // four per pass, oldest first
		st.run(t)
	}
	if ds, _ := st.svc.Deliveries(root(), id, StateDelivered, 200); !slices.ContainsFunc(ds, func(d Delivery) bool { return d.ItemID == "i3" }) {
		t.Fatal("the event that waited wasn't delivered")
	}
	for i := 0; i < 6; i++ {
		st.clock = st.clock.Add(11 * time.Hour)
		st.run(t)
	}
	if pending, _ := st.svc.Deliveries(root(), id, StatePending, 200); len(pending) != 0 {
		t.Fatalf("still pending after the retries: %d", len(pending))
	}

	// A failure after a quiet spell starts a new run: one blip doesn't
	// disable a subscription whose last failure was days ago.
	st.rcv.codes = slices.Repeat([]int{500}, 8)
	st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "lost"})
	for _, wait := range append([]time.Duration{0}, retrySchedule...) {
		st.clock = st.clock.Add(wait + time.Second)
		st.run(t)
	}
	if lost, _ := st.svc.Deliveries(root(), id, StateFailed, 0); len(lost) != 1 {
		t.Fatalf("the lost delivery: %+v", lost)
	}
	st.clock = st.clock.Add(48 * time.Hour)
	st.rcv.codes = []int{503}
	st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "blip"})
	st.run(t)
	if w, _ := st.svc.Get(root(), id); !w.Enabled {
		t.Fatalf("one blip after a quiet spell disabled it: %s", w.DisabledReason)
	}
	st.clock = st.clock.Add(6 * time.Second)
	st.run(t)

	// 410 Gone disables at once; a redelivery keeps the webhook-id.
	st.rcv.codes = []int{410}
	st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "i4"})
	st.run(t)
	if w, _ := st.svc.Get(root(), id); w.Enabled {
		t.Fatal("410 did not disable")
	}
	failed, _ := st.svc.Deliveries(root(), id, StateFailed, 0) // newest first
	if len(failed) == 0 || failed[0].ItemID != "i4" || failed[0].LastCode != 410 {
		t.Fatalf("failed: %+v", failed)
	}
	firstID := st.rcv.got[len(st.rcv.got)-1].Header.Get("webhook-id")
	_, _ = st.svc.Update(root(), id, Input{Enabled: ptr(true)})
	if n, err := st.svc.Redeliver(root(), id, failed[0].ID, time.Time{}, ""); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	st.run(t)
	if got := st.rcv.got[len(st.rcv.got)-1].Header.Get("webhook-id"); got != firstID {
		t.Fatalf("redelivery webhook-id %q, want %q", got, firstID)
	}
}

func TestRotateSignsWithBothDuringGrace(t *testing.T) {
	st := newStack(t)
	created, _ := st.svc.Create(root(), Input{Name: ptr("jobs"), URL: ptr(st.url), Workspaces: ptr([]string{"ws"})})
	rotated, err := st.svc.Rotate(root(), created.Webhook.ID, "")
	if err != nil || rotated.Secret == created.Secret {
		t.Fatal(err)
	}
	if _, err := st.svc.Test(root(), created.Webhook.ID, ""); err != nil {
		t.Fatal(err)
	}
	st.run(t)
	if sig := st.rcv.got[0].Header.Get("webhook-signature"); strings.Count(sig, "v1,") != 2 {
		t.Fatalf("during grace: %s", sig)
	}
	var p Payload
	_ = json.Unmarshal(st.rcv.body[0], &p)
	if p.Type != TypeWebhookTest {
		t.Fatalf("test event: %s", st.rcv.body[0])
	}
	st.clock = st.clock.Add(25 * time.Hour)
	_, _ = st.svc.Test(root(), created.Webhook.ID, "")
	st.run(t)
	if sig := st.rcv.got[1].Header.Get("webhook-signature"); strings.Count(sig, "v1,") != 1 {
		t.Fatalf("after grace: %s", sig)
	}
}

func TestURLPolicy(t *testing.T) {
	p := URLPolicy{}
	for _, bad := range []string{"http://example.com/x", "ftp://example.com", "https://u:p@example.com", "https://127.0.0.1/x", "https://10.1.2.3/x",
		"https://[::1]/x", "https://169.254.169.254/latest", "https://100.64.1.1/", "srv+https://name/x", "https:///x"} {
		if _, err := p.checkURL(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := p.checkURL("https://hooks.example.com/skopos"); err != nil {
		t.Fatal(err)
	}
	private, _ := ParseCIDRs([]string{"10.0.0.0/8"})
	p = URLPolicy{AllowPrivate: private, AllowHTTP: []string{"jobs.internal"}}
	if _, err := p.checkURL("https://10.1.2.3/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.checkURL("http://jobs.internal/x"); err != nil {
		t.Fatal(err)
	}

	// A name that resolves to a private address is refused at connect time.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	client := newClient(URLPolicy{AllowHTTP: []string{"localhost"}})
	_, err = client.Get("http://localhost:" + port + "/")
	if err == nil || !strings.Contains(err.Error(), "private address") {
		t.Fatalf("dial to localhost: %v", err)
	}
}

func TestAccess(t *testing.T) {
	st := newStack(t)
	approver := auth.WithPrincipal(root(), &auth.Principal{KeyID: "k", Name: "ops", Approver: true, Workspaces: map[string]struct{}{"ws": {}}})
	plain := auth.WithPrincipal(root(), &auth.Principal{KeyID: "p", Name: "plain", Workspaces: map[string]struct{}{"ws": {}}})
	in := Input{Name: ptr("jobs"), URL: ptr(st.url), Workspaces: ptr([]string{"ws"})}
	if _, err := st.svc.Create(plain, in); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("non-approver: %v", err)
	}
	other := in
	other.Workspaces = ptr([]string{"ws", "elsewhere"})
	if _, err := st.svc.Create(approver, other); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("out of scope: %v", err)
	}
	group := in
	group.Groups = ptr([]string{"g"})
	if _, err := st.svc.Create(approver, group); err == nil {
		t.Fatal("approver subscribed a group")
	}
	mine, err := st.svc.Create(approver, in)
	if err != nil {
		t.Fatal(err)
	}
	rootHook, _ := st.svc.Create(root(), other)
	list, _ := st.svc.List(approver)
	if len(list) != 1 || list[0].ID != mine.Webhook.ID {
		t.Fatalf("approver sees: %+v", list)
	}
	if _, err := st.svc.Get(approver, rootHook.Webhook.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign webhook: %v", err)
	}
	if _, err := st.svc.Create(root(), Input{Name: ptr("x"), URL: ptr(st.url), Types: ptr([]string{"nope"}), Workspaces: ptr([]string{"ws"})}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown type: %v", err)
	}
	noKey := NewService(st.db, st.em, "", URLPolicy{})
	if _, err := noKey.Create(root(), in); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
}

func TestFeed(t *testing.T) {
	st := newStack(t)
	st.emit(t,
		Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "i1"},
		Event{Type: TypeItemCreated, WorkspaceID: "other", ItemID: "i2"},
		Event{Type: TypeRunStarted, WorkspaceID: "ws", ItemID: "i1"},
	)
	scoped := auth.WithPrincipal(root(), &auth.Principal{KeyID: "k", Name: "jobs", Workspaces: map[string]struct{}{"ws": {}}})
	page, err := st.svc.Feed(scoped, FeedQuery{})
	if err != nil || len(page.Events) != 2 || page.NextAfter != 3 {
		t.Fatalf("scoped: %+v %v", page, err)
	}
	page, _ = st.svc.Feed(root(), FeedQuery{Types: []string{TypeRunStarted}})
	if len(page.Events) != 1 || page.Events[0].Type != TypeRunStarted {
		t.Fatalf("types: %+v", page)
	}
	page, _ = st.svc.Feed(root(), FeedQuery{After: 3})
	if len(page.Events) != 0 || page.NextAfter != 3 {
		t.Fatalf("caught up: %+v", page)
	}
	// A long poll returns when an event arrives.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = st.em.Emit(root(), st.db, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "i9"})
	}()
	started := time.Now()
	page, _ = st.svc.Feed(scoped, FeedQuery{After: 3, Wait: 5 * time.Second})
	if len(page.Events) != 1 || time.Since(started) > 3*time.Second {
		t.Fatalf("long poll: %+v after %s", page, time.Since(started))
	}
	// Pruned past the cursor: 410 with the oldest kept.
	if err := Prune(root(), st.db, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "i10"})
	if _, err := st.svc.Feed(root(), FeedQuery{After: 1}); err == nil {
		t.Fatal("no gone error")
	} else if g, ok := isGone(err); !ok || g.Oldest != 5 {
		t.Fatalf("gone: %v", err)
	}
}

// Events a subscription still has to deliver survive pruning.
func TestPruneKeepsUndelivered(t *testing.T) {
	st := newStack(t)
	created, _ := st.svc.Create(root(), Input{Name: ptr("jobs"), URL: ptr(st.url), Workspaces: ptr([]string{"ws"})})
	st.rcv.codes = []int{500}
	st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "i1"})
	st.run(t)
	if err := Prune(root(), st.db, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ds, _ := st.svc.Deliveries(root(), created.Webhook.ID, StatePending, 0)
	if len(ds) != 1 {
		t.Fatalf("pending delivery's event pruned: %+v", ds)
	}
}

type auditRows struct {
	mu   sync.Mutex
	rows []audit.RecordInput
}

func (a *auditRows) Record(_ context.Context, in audit.RecordInput) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, in)
	return nil
}

// The review fixes: a subscription with a backlog gets perWebhook attempts
// per pass and never holds up another; the deliveries log keeps a delivery
// whose event was pruned; groups scope by members and patterns; audit
// entries carry the subscription's workspace; an emptied log still
// reports a stale cursor.
func TestReviewFixes(t *testing.T) {
	st := newStack(t)
	rec := &auditRows{}
	st.svc.SetAuditRecorder(rec)
	busy, _ := st.svc.Create(root(), Input{Name: ptr("busy"), URL: ptr(st.url), Workspaces: ptr([]string{"busy"})})
	quiet, _ := st.svc.Create(root(), Input{Name: ptr("quiet"), URL: ptr(st.url), Workspaces: ptr([]string{"quiet"})})
	for i := 0; i < 10; i++ {
		st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "busy", ItemID: "b" + strconv.Itoa(i)})
	}
	st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "quiet", ItemID: "q1"})
	st.run(t)
	busyDone, _ := st.svc.Deliveries(root(), busy.Webhook.ID, StateDelivered, 0)
	quietDone, _ := st.svc.Deliveries(root(), quiet.Webhook.ID, StateDelivered, 0)
	if len(busyDone) != perWebhook || len(quietDone) != 1 {
		t.Fatalf("one pass: busy %d (want %d), quiet %d", len(busyDone), perWebhook, len(quietDone))
	}
	for st.rcv.count() < 11 {
		st.run(t)
	}

	// A delivery outlives its pruned event in the log.
	if _, err := st.db.Exec(`DELETE FROM integration_events WHERE item_id = 'q1'`); err != nil {
		t.Fatal(err)
	}
	if ds, _ := st.svc.Deliveries(root(), quiet.Webhook.ID, "", 0); len(ds) != 1 || ds[0].EventType != "" {
		t.Fatalf("pruned event's delivery: %+v", ds)
	}

	// Groups: members and patterns, each group read once per pass.
	reads := 0
	st.svc.GroupExists = func(_ context.Context, g string) string { return "g-" + g }
	st.svc.GroupScope = func(_ context.Context, g string) ([]string, []string, error) {
		reads++
		return []string{"member/ws"}, []string{"github.com/acme/*"}, nil
	}
	group, err := st.svc.Create(root(), Input{Name: ptr("group"), URL: ptr(st.url), Groups: ptr([]string{"team"})})
	if err != nil || group.Webhook.Groups[0] != "g-team" {
		t.Fatalf("group create: %+v %v", group, err)
	}
	st.emit(t,
		Event{Type: TypeItemCreated, WorkspaceID: "member/ws", ItemID: "g1"},
		Event{Type: TypeItemCreated, WorkspaceID: "github.com/acme/app", ItemID: "g2"},
		Event{Type: TypeItemCreated, WorkspaceID: "github.com/other/app", ItemID: "g3"},
	)
	st.run(t)
	if ds, _ := st.svc.Deliveries(root(), group.Webhook.ID, "", 0); len(ds) != 2 || reads != 1 {
		t.Fatalf("group deliveries %d (want 2), group reads %d (want 1)", len(ds), reads)
	}

	// Audit entries name the subscription's workspace.
	rec.mu.Lock()
	first := rec.rows[0]
	rec.mu.Unlock()
	if first.Action != "webhook.create" || first.WorkspaceID != "busy" {
		t.Fatalf("audit: %+v", first)
	}

	// Every row pruned: a cursor behind the last issued seq is stale.
	if _, err := st.db.Exec(`DELETE FROM webhook_deliveries; DELETE FROM integration_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.svc.Feed(root(), FeedQuery{After: 2}); err == nil {
		t.Fatal("empty log: no gone error")
	}
	var last int64
	_ = st.db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'integration_events'`).Scan(&last)
	if page, err := st.svc.Feed(root(), FeedQuery{After: last}); err != nil || len(page.Events) != 0 {
		t.Fatalf("caught up on an empty log: %+v %v", page, err)
	}
}

// A delivery deleted with its subscription while the POST was in flight
// records nothing and reports no error.
func TestFinishAfterDelete(t *testing.T) {
	st := newStack(t)
	created, _ := st.svc.Create(root(), Input{Name: ptr("jobs"), URL: ptr(st.url), Workspaces: ptr([]string{"ws"})})
	st.rcv.codes = []int{500}
	st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "i1"})
	st.run(t)
	ds, _ := st.svc.Deliveries(root(), created.Webhook.ID, "", 0)
	if err := st.svc.Delete(root(), created.Webhook.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.disp.finish(root(), &created.Webhook, due{id: ds[0].ID, webhookID: created.Webhook.ID, attempts: 1}, 200, "", 5); err != nil {
		t.Fatalf("finish after delete: %v", err)
	}
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM webhook_attempts`).Scan(&n)
	if n != 0 {
		t.Fatalf("attempts left: %d", n)
	}
}

// The disable alert fires once; an old secret sealed under an earlier
// integrations key is left out of the signatures, not a failed delivery.
func TestDisableAlertAndUnreadableOldSecret(t *testing.T) {
	st := newStack(t)
	var alerts []string
	st.svc.SetDisableNotifier(func(_ context.Context, w Webhook, reason string) { alerts = append(alerts, w.Name+": "+reason) })
	created, _ := st.svc.Create(root(), Input{Name: ptr("jobs"), URL: ptr(st.url), Workspaces: ptr([]string{"ws"})})
	st.rcv.codes = []int{410, 410, 410, 410}
	for i := 0; i < 4; i++ {
		st.emit(t, Event{Type: TypeItemCreated, WorkspaceID: "ws", ItemID: "i" + strconv.Itoa(i)})
	}
	st.run(t)
	if len(alerts) != 1 || !strings.Contains(alerts[0], "410") {
		t.Fatalf("alerts: %v", alerts)
	}

	// A rotation, then a new integrations key: the old secret can't be read.
	_, _ = st.svc.Update(root(), created.Webhook.ID, Input{Enabled: ptr(true)})
	rotated, err := st.svc.Rotate(root(), created.Webhook.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	other := NewService(st.db, st.em, "a-new-key", st.svc.policy)
	other.now = st.svc.now
	sealed, _ := other.seal(mustSecret(t, rotated.Secret))
	if _, err := st.db.Exec(`UPDATE webhooks SET secret_enc = ? WHERE id = ?`, sealed, created.Webhook.ID); err != nil {
		t.Fatal(err)
	}
	st.svc = other
	st.disp = NewDispatcher(other, nil)
	st.rcv.codes = nil
	before := st.rcv.count()
	if _, err := other.Test(root(), created.Webhook.ID, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		st.run(t)
	}
	last := st.rcv.got[len(st.rcv.got)-1]
	if st.rcv.count() <= before || strings.Count(last.Header.Get("webhook-signature"), "v1,") != 1 {
		t.Fatalf("delivery with an unreadable old secret: %d requests, signature %q", st.rcv.count()-before, last.Header.Get("webhook-signature"))
	}
}

func mustSecret(t *testing.T, shown string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(shown, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An edit names only the fields whose value changed.
func TestUpdateAuditsOnlyChanges(t *testing.T) {
	st := newStack(t)
	rec := &auditRows{}
	st.svc.SetAuditRecorder(rec)
	created, _ := st.svc.Create(root(), Input{Name: ptr("jobs"), URL: ptr(st.url), Workspaces: ptr([]string{"ws", "ws2"}), Types: ptr([]string{TypePlanReady, TypeItemNeedsYou})})
	if _, err := st.svc.Update(root(), created.Webhook.ID, Input{Name: ptr("jobs-2"), URL: ptr(st.url),
		Workspaces: ptr([]string{"ws2", "ws"}), Groups: ptr([]string{}), Types: ptr([]string{TypeItemNeedsYou, TypePlanReady})}); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	last := rec.rows[len(rec.rows)-1]
	rec.mu.Unlock()
	if last.Action != "webhook.update" || last.Notes != "name" {
		t.Fatalf("audit: %+v", last)
	}
}
