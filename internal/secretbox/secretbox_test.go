package secretbox

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"

	_ "modernc.org/sqlite"
)

// legacySeal seals the way webhook secrets were sealed before keys were
// named: nonce and ciphertext, nothing else.
func legacySeal(t *testing.T, secret string, plain []byte) []byte {
	t.Helper()
	raw := sha256.Sum256([]byte(secret))
	block, _ := aes.NewCipher(raw[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return gcm.Seal(nonce, nonce, plain, nil)
}

func TestRing(t *testing.T) {
	old, cur := New("old key"), New("new key", "old key")
	if !old.Enabled() || old.Current() == cur.Current() || len(cur.Previous()) != 1 || cur.Previous()[0] != old.Current() {
		t.Fatalf("keys: %q %q %v", old.Current(), cur.Current(), cur.Previous())
	}
	if KeyID("old key") != old.Current() || strings.Contains(old.Current(), "old") || len(old.Current()) != 12 {
		t.Fatalf("a key id is a fingerprint: %q", old.Current())
	}
	sealed, err := old.Seal([]byte("the value"))
	if err != nil || bytes.Contains(sealed, []byte("the value")) || SealedBy(sealed) != old.Current() {
		t.Fatalf("sealed: %q %v", sealed, err)
	}
	// Sealing twice gives different bytes (a fresh nonce each time).
	again, _ := old.Seal([]byte("the value"))
	if bytes.Equal(sealed, again) {
		t.Fatal("two seals of one value are identical")
	}
	// The ring that holds the old key as previous opens it, and re-seals
	// it under its current key.
	if plain, err := cur.Open(sealed); err != nil || string(plain) != "the value" {
		t.Fatalf("open with the previous key: %q %v", plain, err)
	}
	if cur.UnderCurrent(sealed) {
		t.Fatal("a value under the previous key is not under the current one")
	}
	out, changed, err := cur.Reseal(sealed)
	if err != nil || !changed || SealedBy(out) != cur.Current() {
		t.Fatalf("reseal: %v %v %s", changed, err, SealedBy(out))
	}
	if same, changed, _ := cur.Reseal(out); changed || !bytes.Equal(same, out) {
		t.Fatal("a value under the current key was sealed again")
	}
	// A ring without the key that sealed a value does not open it.
	if _, err := New("another key").Open(sealed); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("an unknown key: %v", err)
	}
	// A value relabelled as another key's is not opened as that key's.
	relabelled := bytes.Replace(out, []byte(cur.Current()), []byte(old.Current()), 1)
	if _, err := cur.Open(relabelled); err == nil {
		t.Fatal("a relabelled value was opened")
	}
	// From before keys were named: opened by whichever held key sealed it.
	legacy := legacySeal(t, "old key", []byte("webhook secret"))
	if SealedBy(legacy) != LegacyKey {
		t.Fatalf("legacy: %s", SealedBy(legacy))
	}
	if plain, err := cur.Open(legacy); err != nil || string(plain) != "webhook secret" {
		t.Fatalf("a legacy value: %q %v", plain, err)
	}
	if _, err := New("another key").Open(legacy); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("a legacy value under a key not held: %v", err)
	}
	// No key: nothing is sealed or opened.
	none := New("", "old key")
	if none.Enabled() || none.Current() != "" {
		t.Fatal("a ring without a current key is disabled")
	}
	if _, err := none.Seal([]byte("x")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("seal without a key: %v", err)
	}
	if _, err := none.Open(sealed); !errors.Is(err, ErrNoKey) {
		t.Fatalf("open without a key: %v", err)
	}
	if plain, err := none.Open(nil); err != nil || plain != nil {
		t.Fatal("an empty value is empty")
	}
}

type sink struct {
	mu  sync.Mutex
	got []audit.RecordInput
}

func (s *sink) Record(_ context.Context, in audit.RecordInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, in)
	return nil
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "s.db") + "?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)"
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

func TestStore(t *testing.T) {
	sqlDB := testDB(t)
	s := NewStore(sqlDB, New("key one"))
	ctx := context.Background()
	if _, err := s.Get(ctx, "mcp_server:x", "token"); !errors.Is(err, ErrNotSet) {
		t.Fatalf("not set: %v", err)
	}
	if err := s.Set(ctx, "mcp_server:x", "token", []byte("s3cret-value"), "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "mcp_server:x", "other", []byte("another-value"), "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "mcp_server:y", "token", []byte("y-value-12345"), "key ops"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(ctx, "mcp_server:x", "token"); err != nil || string(v) != "s3cret-value" {
		t.Fatalf("get: %q %v", v, err)
	}
	// The stored row holds no plaintext.
	var sealed []byte
	var keyID string
	if err := sqlDB.QueryRow(`SELECT sealed, key_id FROM secrets WHERE owner = 'mcp_server:x' AND name = 'token'`).Scan(&sealed, &keyID); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("s3cret-value")) || keyID != s.Ring().Current() {
		t.Fatalf("stored: %q %s", sealed, keyID)
	}
	list, err := s.List(ctx, "mcp_server:x")
	if err != nil || len(list) != 2 || list["token"].UpdatedBy != "root" || list["token"].KeyID != keyID {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := s.Set(ctx, "mcp_server:x", "token", nil, "root"); err == nil {
		t.Fatal("an empty value was stored")
	}
	if err := s.Set(ctx, "mcp_server:x", "token", bytes.Repeat([]byte("x"), MaxValueBytes+1), "root"); err == nil {
		t.Fatal("an oversized value was stored")
	}
	gone, err := s.Keep(ctx, "mcp_server:x", []string{"token"})
	if err != nil || len(gone) != 1 || gone[0] != "other" {
		t.Fatalf("keep: %v %v", gone, err)
	}
	if had, _ := s.Delete(ctx, "mcp_server:x", "token"); !had {
		t.Fatal("delete")
	}
	if had, _ := s.Delete(ctx, "mcp_server:x", "token"); had {
		t.Fatal("deleted twice")
	}
	if err := s.DeleteOwner(ctx, "mcp_server:y"); err != nil {
		t.Fatal(err)
	}
	var n int
	if _ = sqlDB.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&n); n != 0 {
		t.Fatalf("%d secrets left", n)
	}
	// Without a key nothing is stored.
	if err := NewStore(sqlDB, New("")).Set(ctx, "mcp_server:x", "token", []byte("s3cret-value"), "root"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
}

// A rotation seals everything again under the current key — MCP server
// secrets and webhook secrets, the unnamed ones of an earlier release
// included — all of it or none, and is recorded.
func TestRotate(t *testing.T) {
	sqlDB := testDB(t)
	ctx := context.Background()
	old := New("old key")
	store := NewStore(sqlDB, old)
	if err := store.Set(ctx, "mcp_server:x", "token", []byte("s3cret-value"), "root"); err != nil {
		t.Fatal(err)
	}
	named, _ := old.Seal([]byte("webhook two"))
	for _, w := range []struct {
		id          string
		secret, was []byte
	}{{"w1", legacySeal(t, "old key", []byte("webhook one")), legacySeal(t, "old key", []byte("webhook one before"))}, {"w2", named, nil}} {
		if _, err := sqlDB.Exec(`INSERT INTO webhooks (id, name, url, secret_enc, old_secret_enc, created_by, created_ms, updated_ms) VALUES (?, ?, 'https://x', ?, ?, 'root', 1, 1)`,
			w.id, w.id, w.secret, w.was); err != nil {
			t.Fatal(err)
		}
	}
	// The server now runs with a new key and still holds the old one.
	ring := New("new key", "old key")
	rot := NewRotator(sqlDB, ring)
	audits := &sink{}
	rot.SetAuditSink(audits)
	scoped := auth.WithPrincipal(ctx, &auth.Principal{KeyID: "k1", Name: "ops", Admin: true, AllWorkspaces: true})
	if _, err := rot.Rotate(scoped, "cli"); !errors.Is(err, auth.ErrRootRequired) {
		t.Fatalf("a rotation is the root key's: %v", err)
	}
	if _, err := rot.Status(scoped); !errors.Is(err, auth.ErrRootRequired) {
		t.Fatalf("the status is the root key's: %v", err)
	}
	st, err := rot.Status(ctx)
	if err != nil || st.Current != ring.Current() || st.Pending != 4 || st.Unreadable != 0 || st.Sealed[old.Current()] != 2 || st.Sealed[LegacyKey] != 2 {
		t.Fatalf("status before: %+v %v", st, err)
	}
	res, err := rot.Rotate(ctx, "cli")
	if err != nil || res.Resealed != 4 || res.Current != ring.Current() {
		t.Fatalf("rotate: %+v %v", res, err)
	}
	st, _ = rot.Status(ctx)
	if st.Pending != 0 || st.Unreadable != 0 || st.Sealed[ring.Current()] != 4 || len(st.Sealed) != 1 {
		t.Fatalf("status after: %+v", st)
	}
	if len(audits.got) != 1 || audits.got[0].Action != "secret_key.rotate" || audits.got[0].EntityType != AuditEntity || audits.got[0].EntityID != ring.Current() ||
		!strings.Contains(audits.got[0].Notes, "4 stored values") || strings.Contains(audits.got[0].Notes, "key") && strings.Contains(audits.got[0].Notes, "old key") {
		t.Fatalf("audit: %+v", audits.got)
	}
	// The new key alone opens everything now.
	only := New("new key")
	if v, err := NewStore(sqlDB, only).Get(ctx, "mcp_server:x", "token"); err != nil || string(v) != "s3cret-value" {
		t.Fatalf("after dropping the old key: %q %v", v, err)
	}
	var keyID string
	if _ = sqlDB.QueryRow(`SELECT key_id FROM secrets`).Scan(&keyID); keyID != ring.Current() {
		t.Fatalf("the row names the key that sealed it: %s", keyID)
	}
	rows, err := sqlDB.Query(`SELECT secret_enc, old_secret_enc FROM webhooks ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	want := [][2]string{{"webhook one", "webhook one before"}, {"webhook two", ""}}
	for i := 0; rows.Next(); i++ {
		var a, b []byte
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		pa, erra := only.Open(a)
		pb, errb := only.Open(b)
		if erra != nil || errb != nil || string(pa) != want[i][0] || string(pb) != want[i][1] {
			t.Fatalf("webhook %d: %q %q %v %v", i, pa, pb, erra, errb)
		}
	}
	// Again: nothing to do, nothing recorded.
	if res, err := rot.Rotate(ctx, "cli"); err != nil || res.Resealed != 0 || len(audits.got) != 1 {
		t.Fatalf("a second rotation: %+v %v", res, err)
	}

	// A value under a key the server does not hold stops a rotation, and
	// nothing is changed.
	if err := NewStore(sqlDB, New("lost key")).Set(ctx, "mcp_server:z", "token", []byte("unreachable-value"), "root"); err != nil {
		t.Fatal(err)
	}
	next := NewRotator(sqlDB, New("newest key", "new key"))
	st, _ = next.Status(ctx)
	if st.Unreadable != 1 || st.Pending != 4 {
		t.Fatalf("status with a lost key: %+v", st)
	}
	if _, err := next.Rotate(ctx, "cli"); !errors.Is(err, ErrUnknownKey) || !strings.Contains(err.Error(), "mcp_server:z token") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("a rotation with an unreadable value: %v", err)
	}
	if st, _ := next.Status(ctx); st.Sealed[KeyID("newest key")] != 0 {
		t.Fatalf("a failed rotation changed something: %+v", st)
	}
	if _, err := NewRotator(sqlDB, New("")).Rotate(ctx, "cli"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("a rotation without a key: %v", err)
	}
}

// A webhook's previous signing secret is of no use a day after a rotation.
// One left under a key the server no longer holds must not stop a key
// rotation: it is dropped. One still in use does stop it.
func TestRotateDropsExpiredWebhookSecrets(t *testing.T) {
	sqlDB := testDB(t)
	ctx := context.Background()
	lost := New("a key lost long ago")
	dead, _ := lost.Seal([]byte("an old signing secret"))
	cur := New("current key")
	live, _ := cur.Seal([]byte("the signing secret"))
	insert := func(id string, oldUntil any) {
		t.Helper()
		if _, err := sqlDB.Exec(`INSERT INTO webhooks (id, name, url, secret_enc, old_secret_enc, old_until_ms, created_by, created_ms, updated_ms) VALUES (?, ?, 'https://x', ?, ?, ?, 'root', 1, 1)`,
			id, id, live, dead, oldUntil); err != nil {
			t.Fatal(err)
		}
	}
	insert("expired", int64(1000)) // long past
	rot := NewRotator(sqlDB, New("next key", "current key"))
	st, err := rot.Status(ctx)
	if err != nil || st.Unreadable != 0 || st.Pending != 1 {
		t.Fatalf("an expired old secret is not counted: %+v %v", st, err)
	}
	res, err := rot.Rotate(ctx, "cli")
	if err != nil || res.Resealed != 1 {
		t.Fatalf("rotate: %+v %v", res, err)
	}
	var left int
	if _ = sqlDB.QueryRow(`SELECT COUNT(*) FROM webhooks WHERE old_secret_enc IS NOT NULL`).Scan(&left); left != 0 {
		t.Fatal("the expired old secret was kept")
	}
	// Still within its day: it has to be openable, or nothing is changed.
	if _, err := sqlDB.Exec(`UPDATE webhooks SET old_secret_enc = ?, old_until_ms = ?`, dead, int64(1)<<50); err != nil {
		t.Fatal(err)
	}
	again := NewRotator(sqlDB, New("newest key", "next key"))
	if st, _ := again.Status(ctx); st.Unreadable != 1 {
		t.Fatalf("a live old secret under a lost key: %+v", st)
	}
	if _, err := again.Rotate(ctx, "cli"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("a live old secret under a lost key must stop the rotation: %v", err)
	}
}
