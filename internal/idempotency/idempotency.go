// Package idempotency makes POST requests safe to retry: a request carrying
// an Idempotency-Key header (draft-ietf-httpapi-idempotency-key-header) is
// remembered for 24 hours per API key, and the same key with the same
// request answers the stored response instead of acting twice. An
// integration that times out creating an item retries without making a
// second one.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
)

const (
	// Header is the request header naming the key.
	Header = "Idempotency-Key"
	// ReplayedHeader marks a stored response sent again.
	ReplayedHeader = "Idempotent-Replayed"
	// TTL is how long a key is remembered.
	TTL = 24 * time.Hour
	// abandonAfter: a key whose request never finished (the server
	// stopped mid-request) may be used again after this long.
	abandonAfter = 5 * time.Minute
	maxKey       = 255
	// maxBody bounds the request read for hashing (the API's own limit)
	// and the response stored for replay; a larger response is not kept.
	maxBody = 1 << 20
)

// Store keeps the keys in idempotency_keys.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func New(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

// Middleware applies to POST requests under any of the path prefixes; it
// must run after authentication (keys are per principal). Requests
// without the header pass untouched.
func (s *Store) Middleware(prefixes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(Header)
			if r.Method != http.MethodPost || key == "" || !hasPrefix(r.URL.Path, prefixes) {
				next.ServeHTTP(w, r)
				return
			}
			s.serve(w, r, next, key)
		})
	}
}

func hasPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func (s *Store) serve(w http.ResponseWriter, r *http.Request, next http.Handler, key string) {
	if !validKey(key) {
		respond(w, http.StatusBadRequest, "Idempotency-Key must be 1 to 255 printable characters")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	_ = r.Body.Close()
	if err != nil {
		respond(w, http.StatusBadRequest, "reading the request body failed")
		return
	}
	if len(body) > maxBody {
		respond(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	owner, person := ownerOf(r.Context())
	hash := requestHash(r.Method, r.URL.Path, r.URL.RawQuery, person, body)
	ctx := context.WithoutCancel(r.Context())

	prior, err := s.begin(ctx, owner, key, hash)
	if err != nil {
		respond(w, http.StatusInternalServerError, "internal error")
		return
	}
	if prior != nil {
		switch {
		case prior.hash != hash:
			respond(w, http.StatusUnprocessableEntity, "this Idempotency-Key was used for a different request")
		case prior.status == 0:
			respond(w, http.StatusConflict, "a request with this Idempotency-Key is still in progress")
		default:
			if prior.contentType != "" {
				w.Header().Set("Content-Type", prior.contentType)
			}
			w.Header().Set(ReplayedHeader, "true")
			w.WriteHeader(prior.status)
			_, _ = w.Write(prior.response)
		}
		return
	}

	rec := &recorder{ResponseWriter: w}
	done := false
	defer func() {
		if !done {
			// The handler panicked: free the key so a retry can run.
			s.forget(ctx, owner, key)
		}
	}()
	next.ServeHTTP(rec, r)
	done = true
	status := rec.statusCode()
	// Only a success is kept. A refused request changed nothing, so its
	// retry may run (after the key got a permission, say); a server error
	// may succeed on retry; an oversized response can't be replayed.
	if status >= 400 || rec.overflow {
		s.forget(ctx, owner, key)
		return
	}
	s.finish(ctx, owner, key, status, w.Header().Get("Content-Type"), rec.body.Bytes())
}

// requestHash identifies the request a key was used for, the person a
// delegate key named included.
func requestHash(method, path, query, person string, body []byte) string {
	sum := sha256.Sum256([]byte(method + "\n" + path + "\n" + query + "\n" + person + "\n" + string(body)))
	return hex.EncodeToString(sum[:])
}

// validKey: 1 to 255 printable ASCII characters.
func validKey(k string) bool {
	if len(k) == 0 || len(k) > maxKey {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// ownerOf scopes keys per API key (root and an auth-disabled server share
// "root"), and returns the person the request names (X-Skopos-On-Behalf-Of).
func ownerOf(ctx context.Context) (string, string) {
	p := auth.PrincipalFromContext(ctx)
	if p == nil {
		return "root", ""
	}
	if p.Root {
		return "root", p.OnBehalfOf
	}
	return "key:" + p.KeyID, p.OnBehalfOf
}

type entry struct {
	hash        string
	status      int
	contentType string
	response    []byte
}

// begin claims the key for this request (nil) or returns the request that
// holds it. Expired keys, and keys whose request was abandoned, are taken
// over.
func (s *Store) begin(ctx context.Context, owner, key, hash string) (*entry, error) {
	for range 2 {
		e, err := s.claim(ctx, owner, key, hash)
		if !errors.Is(err, sql.ErrNoRows) {
			return e, err
		}
		// Pruned between the two statements: once more.
	}
	return nil, errors.New("idempotency key kept changing")
}

func (s *Store) claim(ctx context.Context, owner, key, hash string) (*entry, error) {
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO idempotency_keys (owner, idem_key, request_hash, created_ms)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (owner, idem_key) DO UPDATE SET
			request_hash = excluded.request_hash, status = 0, content_type = '', response = NULL,
			created_ms = excluded.created_ms, done_ms = NULL
		WHERE idempotency_keys.created_ms < ?
		   OR (idempotency_keys.status = 0 AND idempotency_keys.created_ms < ?)`,
		owner, key, hash, now, now-TTL.Milliseconds(), now-abandonAfter.Milliseconds())
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, nil
	}
	var e entry
	err = s.db.QueryRowContext(ctx, `SELECT request_hash, status, content_type, COALESCE(response, X'') FROM idempotency_keys WHERE owner = ? AND idem_key = ?`,
		owner, key).Scan(&e.hash, &e.status, &e.contentType, &e.response)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) finish(ctx context.Context, owner, key string, status int, contentType string, body []byte) {
	_, _ = s.db.ExecContext(ctx, `UPDATE idempotency_keys SET status = ?, content_type = ?, response = ?, done_ms = ? WHERE owner = ? AND idem_key = ?`,
		status, contentType, body, s.now().UnixMilli(), owner, key)
}

func (s *Store) forget(ctx context.Context, owner, key string) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE owner = ? AND idem_key = ?`, owner, key)
}

// Prune drops keys past the TTL (the cleanup ticker).
func Prune(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_ms < ?`, now.Add(-TTL).UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// recorder passes the response through and keeps a copy for replay.
type recorder struct {
	http.ResponseWriter
	status   int
	body     bytes.Buffer
	overflow bool
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if !r.overflow {
		if r.body.Len()+len(b) > maxBody {
			r.overflow = true
			r.body.Reset()
		} else {
			r.body.Write(b)
		}
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func respond(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
