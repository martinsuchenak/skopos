package secretbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// AuditEntity is the audit-log entity type of a rotation; the entity id is
// the id of the key everything was sealed under.
const AuditEntity = "secret_key"

// Table is one place sealed values are stored.
type Table struct {
	Name   string   // the table
	What   string   // an SQL expression that names a row, for errors
	Cols   []string // the columns holding sealed values (NULL allowed)
	KeyCol string   // the column recording the key id, "" = none
	// Expire, when set, is a statement run before anything is sealed again
	// that clears values no longer in use (its one parameter is now, in
	// Unix ms); Live is the condition under which a column's value still
	// counts (same parameter). A value past its use is never a reason a
	// rotation fails.
	Expire string
	Live   map[string]string
}

// Tables are every place a sealed value lives. A new one is added here, or
// a rotation would leave it behind.
var Tables = []Table{
	{Name: "secrets", What: "owner || ' ' || name", Cols: []string{"sealed"}, KeyCol: "key_id"},
	{Name: "webhooks", What: "name", Cols: []string{"secret_enc", "old_secret_enc"},
		// A webhook's previous secret signs for a day after a rotation and
		// is dead weight after that.
		Expire: `UPDATE webhooks SET old_secret_enc = NULL, old_until_ms = NULL WHERE old_secret_enc IS NOT NULL AND old_until_ms IS NOT NULL AND old_until_ms < ?`,
		Live:   map[string]string{"old_secret_enc": `(old_until_ms IS NULL OR old_until_ms >= ?)`}},
}

// AuditSink records a rotation (audit.Service in production).
type AuditSink interface {
	Record(ctx context.Context, in audit.RecordInput) error
}

// Rotator re-seals everything the server stores under its current key.
type Rotator struct {
	db    *sql.DB
	ring  *Ring
	audit AuditSink
}

func NewRotator(db *sql.DB, ring *Ring) *Rotator { return &Rotator{db: db, ring: ring} }

func (r *Rotator) SetAuditSink(a AuditSink) { r.audit = a }

// Status is what the server holds and what each key sealed.
type Status struct {
	// Current is the id of the key new values are sealed under ("" = the
	// server has no secret key).
	Current string `json:"current"`
	// Previous are the ids of the other keys the server holds.
	Previous []string `json:"previous"`
	// Sealed counts stored values by the key that sealed them ("unnamed" =
	// from before keys were named).
	Sealed map[string]int `json:"sealed"`
	// Pending is how many values a rotation would seal again.
	Pending int `json:"pending"`
	// Unreadable counts values under a key the server does not hold.
	Unreadable int `json:"unreadable"`
}

// Status reports the keys held and what is sealed under each. Root only.
func (r *Rotator) Status(ctx context.Context) (*Status, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	return r.status(ctx)
}

func (r *Rotator) status(ctx context.Context) (*Status, error) {
	st := &Status{Current: r.ring.Current(), Previous: r.ring.Previous(), Sealed: map[string]int{}}
	if st.Previous == nil {
		st.Previous = []string{}
	}
	held := map[string]bool{r.ring.Current(): true}
	for _, id := range r.ring.Previous() {
		held[id] = true
	}
	nowMS := time.Now().UnixMilli()
	for _, t := range Tables {
		for _, col := range t.Cols {
			q, args := fmt.Sprintf(`SELECT %s FROM %s WHERE %s IS NOT NULL`, col, t.Name, col), []any{}
			if live := t.Live[col]; live != "" {
				q, args = q+" AND "+live, []any{nowMS}
			}
			rows, err := r.db.QueryContext(ctx, q, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var sealed []byte
				if err := rows.Scan(&sealed); err != nil {
					_ = rows.Close()
					return nil, err
				}
				id := SealedBy(sealed)
				if id == "" {
					continue
				}
				st.Sealed[id]++
				switch {
				case id == st.Current:
				case id == LegacyKey:
					// Unnamed: whether a held key opens it is only known by
					// trying.
					if _, err := r.ring.Open(sealed); err != nil {
						st.Unreadable++
					} else {
						st.Pending++
					}
				case held[id]:
					st.Pending++
				default:
					st.Unreadable++
				}
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
		}
	}
	return st, nil
}

// Result is what a rotation did.
type Result struct {
	Current  string `json:"current"`
	Resealed int    `json:"resealed"`
}

// Rotate seals every stored value that is not under the current key again
// under it, in one transaction: either all of them or none. A value under a
// key the server does not hold stops it. Root only.
func (r *Rotator) Rotate(ctx context.Context, via string) (*Result, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	if !r.ring.Enabled() {
		return nil, ErrNoKey
	}
	before, err := r.status(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	total := 0
	for _, t := range Tables {
		if t.Expire != "" {
			if _, err := tx.ExecContext(ctx, t.Expire, time.Now().UnixMilli()); err != nil {
				return nil, err
			}
		}
		n, err := ResealRows(ctx, tx, r.ring, t.Name, t.What, t.Cols, t.KeyCol)
		if err != nil {
			return nil, fmt.Errorf("nothing was changed: %w", err)
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if r.audit != nil && total > 0 {
		var from []string
		for id, n := range before.Sealed {
			if id != before.Current {
				from = append(from, fmt.Sprintf("%d under %s", n, id))
			}
		}
		sort.Strings(from)
		_ = r.audit.Record(ctx, audit.RecordInput{EntityType: AuditEntity, EntityID: r.ring.Current(), Action: "secret_key.rotate", Via: audit.CleanVia(via),
			Notes: fmt.Sprintf("%d stored values sealed again under key %s (were: %s)", total, r.ring.Current(), strings.Join(from, ", "))})
	}
	return &Result{Current: r.ring.Current(), Resealed: total}, nil
}

// Handler serves the secret key's status and the rotation.
type Handler struct {
	rot   *Rotator
	authn *auth.Authenticator
}

func NewHandler(rot *Rotator, authn *auth.Authenticator) *Handler {
	return &Handler{rot: rot, authn: authn}
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrRootRequired) {
		rest.RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	// No key, or a value that cannot be opened: the operator has to know
	// which (the message names the row, never a value).
	rest.RespondError(w, http.StatusConflict, err.Error())
}

// Status handles GET /api/fleet/secret-key.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	if h.authn.Authenticate(r) == nil {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	st, err := h.rot.Status(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, st)
}

// Rotate handles POST /api/fleet/secret-key/rotate.
func (h *Handler) Rotate(w http.ResponseWriter, r *http.Request) {
	if h.authn.Authenticate(r) == nil {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	res, err := h.rot.Rotate(r.Context(), rest.Via(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}
