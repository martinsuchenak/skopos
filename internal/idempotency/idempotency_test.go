package idempotency

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
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

// server wraps a counting handler: 201 with the call number, 500 when the
// body says "fail", a panic when it says "panic".
func server(t *testing.T, s *Store, principal *auth.Principal) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		switch string(body) {
		case "fail":
			w.WriteHeader(http.StatusInternalServerError)
			return
		case "panic":
			panic("boom")
		case "refuse":
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"call":` + string(rune('0'+n)) + `}`))
	})
	withPrincipal := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
		})
	}
	recovering := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recover() != nil {
					w.WriteHeader(http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
	srv := httptest.NewServer(recovering(withPrincipal(s.Middleware("/api/inbox")(h))))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func post(t *testing.T, url, key, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(Header, key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return res, string(b)
}

func TestIdempotencyKey(t *testing.T) {
	sqlDB := testDB(t)
	s := New(sqlDB)
	srv, calls := server(t, s, &auth.Principal{KeyID: "k1", Name: "globex"})

	res, body := post(t, srv.URL+"/api/inbox", "job-1", `{"a":1}`)
	if res.StatusCode != http.StatusCreated || body != `{"call":1}` || res.Header.Get(ReplayedHeader) != "" {
		t.Fatalf("first: %d %q %v", res.StatusCode, body, res.Header)
	}
	// The retry gets the stored answer; the handler doesn't run again.
	res, body = post(t, srv.URL+"/api/inbox", "job-1", `{"a":1}`)
	if res.StatusCode != http.StatusCreated || body != `{"call":1}` || res.Header.Get(ReplayedHeader) != "true" ||
		res.Header.Get("Content-Type") != "application/json" || calls.Load() != 1 {
		t.Fatalf("replay: %d %q %v calls=%d", res.StatusCode, body, res.Header, calls.Load())
	}
	// Same key, different request.
	if res, _ := post(t, srv.URL+"/api/inbox", "job-1", `{"a":2}`); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("different body: %d", res.StatusCode)
	}
	if res, _ := post(t, srv.URL+"/api/inbox/x/queue", "job-1", `{"a":1}`); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("different path: %d", res.StatusCode)
	}
	// No header, or a path outside the prefixes: untouched.
	post(t, srv.URL+"/api/inbox", "", `{"a":1}`)
	post(t, srv.URL+"/api/inboxes", "job-1", `{"a":1}`)
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	if res, _ := post(t, srv.URL+"/api/inbox", strings.Repeat("x", 256), `{}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("long key: %d", res.StatusCode)
	}

	// A refusal is not kept: the retry runs.
	post(t, srv.URL+"/api/inbox", "job-4", "refuse")
	post(t, srv.URL+"/api/inbox", "job-4", "refuse")
	if calls.Load() != 5 {
		t.Fatalf("calls after 403s = %d, want 5", calls.Load())
	}

	// A server error is not kept: the retry runs.
	post(t, srv.URL+"/api/inbox", "job-2", "fail")
	post(t, srv.URL+"/api/inbox", "job-2", "fail")
	if calls.Load() != 7 {
		t.Fatalf("calls after 500s = %d, want 7", calls.Load())
	}
	// Nor is a panic.
	post(t, srv.URL+"/api/inbox", "job-3", "panic")
	post(t, srv.URL+"/api/inbox", "job-3", "panic")
	if calls.Load() != 9 {
		t.Fatalf("calls after panics = %d, want 9", calls.Load())
	}

	// Another key's identical Idempotency-Key is its own.
	other, otherCalls := server(t, s, &auth.Principal{KeyID: "k2", Name: "other"})
	if res, _ := post(t, other.URL+"/api/inbox", "job-1", `{"a":1}`); res.StatusCode != http.StatusCreated || res.Header.Get(ReplayedHeader) != "" || otherCalls.Load() != 1 {
		t.Fatalf("other key: %d %v", res.StatusCode, res.Header)
	}

	// A request still running answers 409; one abandoned for 5 minutes,
	// or a key past 24 hours, is taken over.
	ctx := context.Background()
	if prior, err := s.begin(ctx, "key:k1", "running", requestHash(http.MethodPost, "/api/inbox", "", "", []byte("h"))); err != nil || prior != nil {
		t.Fatal(prior, err)
	}
	if res, _ := post(t, srv.URL+"/api/inbox", "running", `h`); res.StatusCode != http.StatusConflict {
		t.Fatalf("in progress: %d", res.StatusCode)
	}
	s.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	if prior, err := s.begin(ctx, "key:k1", "running", "h2"); err != nil || prior != nil {
		t.Fatalf("abandoned key not taken over: %v %v", prior, err)
	}
	s.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if prior, err := s.begin(ctx, "key:k1", "job-1", "new"); err != nil || prior != nil {
		t.Fatalf("expired key not taken over: %v %v", prior, err)
	}
	if n, err := Prune(ctx, sqlDB, time.Now().Add(50*time.Hour)); err != nil || n == 0 {
		t.Fatalf("prune: %d %v", n, err)
	}
}
