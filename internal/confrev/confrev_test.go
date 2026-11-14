package confrev

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/db"
	logslog "github.com/paularlott/logger/slog"
	"io"
)

func testStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	log := logslog.New(logslog.Config{Level: "error", Writer: io.Discard})
	sqlDB, err := db.Connect(log, filepath.Join(t.TempDir(), "rev.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	return NewStore(sqlDB), sqlDB
}

// A change is a new revision and moves the head; saving the same content
// again is not a change; clearing keeps every revision; history is newest
// first; and nothing is ever updated in place.
func TestRevisionsAreAppendOnly(t *testing.T) {
	s, sqlDB := testStore(t)
	ctx := context.Background()
	tick := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { tick = tick.Add(time.Second); return tick })
	owner := WorkspaceOwner("github.com/acme/app")
	meta := Meta{Actor: "key ops", ActorKeyID: "k1", Via: "cli", Note: "first"}

	if head, err := s.Head(ctx, KindPolicy, owner); err != nil || head != nil {
		t.Fatalf("no head yet: %+v %v", head, err)
	}
	first, changed, err := s.Put(ctx, KindPolicy, owner, []byte(`{"allow":[{"provider":"anthropic"}]}`), meta)
	if err != nil || !changed || first.ID == "" || first.Hash != Hash([]byte(`{"allow":[{"provider":"anthropic"}]}`)) {
		t.Fatalf("first put: %+v %v %v", first, changed, err)
	}
	if first.Actor != "key ops" || first.ActorKeyID != "k1" || first.Via != "cli" || first.Note != "first" {
		t.Fatalf("who wrote it must be on the revision: %+v", first)
	}
	same, changed, err := s.Put(ctx, KindPolicy, owner, []byte(`{"allow":[{"provider":"anthropic"}]}`), Meta{Actor: "root"})
	if err != nil || changed || same.ID != first.ID {
		t.Fatalf("the same content is not a change: %+v %v %v", same, changed, err)
	}
	second, changed, err := s.Put(ctx, KindPolicy, owner, []byte(`{"allow":[{"provider":"anthropic"},{"provider":"local"}]}`), meta)
	if err != nil || !changed || second.ID == first.ID {
		t.Fatalf("second put: %+v %v %v", second, changed, err)
	}
	if head, _ := s.Head(ctx, KindPolicy, owner); head == nil || head.ID != second.ID {
		t.Fatalf("the head must be the newest: %+v", head)
	}
	// The first revision is still there, unchanged.
	if got, err := s.Get(ctx, first.ID); err != nil || string(got.Content) != `{"allow":[{"provider":"anthropic"}]}` || got.Hash != first.Hash {
		t.Fatalf("an old revision must stay as written: %+v %v", got, err)
	}

	was, err := s.Clear(ctx, KindPolicy, owner)
	if err != nil || was == nil || was.ID != second.ID {
		t.Fatalf("clear returns what was current: %+v %v", was, err)
	}
	if head, _ := s.Head(ctx, KindPolicy, owner); head != nil {
		t.Fatalf("cleared: %+v", head)
	}
	if again, err := s.Clear(ctx, KindPolicy, owner); err != nil || again != nil {
		t.Fatalf("clearing nothing: %+v %v", again, err)
	}
	// Putting back the content that was cleared IS a change (the head was
	// empty), and gets its own revision.
	third, changed, err := s.Put(ctx, KindPolicy, owner, []byte(`{"allow":[{"provider":"anthropic"}]}`), meta)
	if err != nil || !changed || third.ID == first.ID {
		t.Fatalf("put after clear: %+v %v %v", third, changed, err)
	}

	hist, err := s.History(ctx, KindPolicy, owner, 0)
	if err != nil || len(hist) != 3 || hist[0].ID != third.ID || hist[2].ID != first.ID {
		t.Fatalf("history newest first: %d %v", len(hist), err)
	}
	var n int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM config_revisions`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("three revisions, none removed: %d %v", n, err)
	}
}

func TestHeadsAndValidation(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	for _, owner := range []string{OwnerFleetCeiling, GroupOwner("g1"), WorkspaceOwner("ws-a")} {
		if _, _, err := s.Put(ctx, KindPolicy, owner, []byte(`{"allow":[]}`), Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Clear(ctx, KindPolicy, GroupOwner("g1")); err != nil {
		t.Fatal(err)
	}
	heads, err := s.Heads(ctx, KindPolicy)
	if err != nil || len(heads) != 2 || heads[OwnerFleetCeiling] == nil || heads[WorkspaceOwner("ws-a")] == nil {
		t.Fatalf("heads list only owners with a current revision: %v %v", heads, err)
	}
	if other, _ := s.Heads(ctx, "runner_def"); len(other) != 0 {
		t.Fatalf("heads are per kind: %v", other)
	}
	for name, put := range map[string]func() error{
		"not JSON": func() error {
			_, _, err := s.Put(ctx, KindPolicy, "fleet:default", []byte(`{nope`), Meta{})
			return err
		},
		"no owner": func() error { _, _, err := s.Put(ctx, KindPolicy, " ", []byte(`{}`), Meta{}); return err },
		"no kind":  func() error { _, _, err := s.Put(ctx, "", "fleet:default", []byte(`{}`), Meta{}); return err },
	} {
		if err := put(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown revision: %v", err)
	}
}

// A conditional write happens only while the revision the writer read is
// still the current one, and the comparison is part of the write: of two
// writers that both read the same revision, one gets ErrMoved.
func TestPutIfHead(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	none := ""
	first, changed, err := s.Put(ctx, "workspace_config", "workspace:a", []byte(`{"n":1}`), Meta{IfHead: &none})
	if err != nil || !changed {
		t.Fatalf("nothing stored, as read: %v %v", changed, err)
	}
	if _, _, err := s.Put(ctx, "workspace_config", "workspace:a", []byte(`{"n":2}`), Meta{IfHead: &none}); !errors.Is(err, ErrMoved) {
		t.Fatalf("read as empty, and it is not: %v", err)
	}
	read := first.ID
	results := make(chan error, 2)
	for _, body := range []string{`{"n":3}`, `{"n":4}`} {
		go func() {
			_, _, err := s.Put(ctx, "workspace_config", "workspace:a", []byte(body), Meta{IfHead: &read})
			results <- err
		}()
	}
	a, b := <-results, <-results
	moved := 0
	for _, err := range []error{a, b} {
		switch {
		case err == nil:
		case errors.Is(err, ErrMoved):
			moved++
		default:
			t.Fatalf("an unexpected error: %v", err)
		}
	}
	if moved != 1 {
		t.Fatalf("of two writers that read the same revision exactly one must be refused: %v, %v", a, b)
	}
}
