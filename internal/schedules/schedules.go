// Package schedules runs an item template on a rhythm: for a workspace, or
// for every workspace of a group, an item is started from the template and
// queued when the rhythm comes round. One firing creates each item at most
// once; an item the schedule started earlier that is still open is not
// followed by another.
package schedules

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/ids"
	"github.com/martinsuchenak/skopos/internal/inbox"
)

// Owner kinds, as the pipeline names them.
const (
	KindWorkspace = "workspace"
	KindGroup     = "group"
)

// Firing outcomes.
const (
	OutcomeCreating = "creating"
	OutcomeCreated  = "created"
	OutcomeSkipped  = "skipped"
	OutcomeFailed   = "failed"
)

// AuditEntity is the audit log's entity type for a schedule.
const AuditEntity = "schedule"

var (
	ErrNotFound     = errors.New("schedule not found")
	ErrInvalidInput = errors.New("invalid schedule")
	ErrConflict     = errors.New("schedule conflict")
	reName          = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// Schedule is a template run on a rhythm.
type Schedule struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OwnerKind string    `json:"owner_kind"`
	OwnerID   string    `json:"owner_id"`
	Template  string    `json:"template"`
	Rhythm    string    `json:"rhythm"`
	Timezone  string    `json:"timezone"`
	Enabled   bool      `json:"enabled"`
	Next      time.Time `json:"next,omitzero"`
	Last      time.Time `json:"last,omitzero"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
	// HolderKey is the key of the person who last created or changed it
	// ("" = root): the schedule fires only while that key still approves
	// for the workspace.
	HolderKey string `json:"-"`
}

// Firing is what one occurrence did in one workspace.
type Firing struct {
	ID          string    `json:"id"`
	ScheduleID  string    `json:"schedule_id"`
	Occurrence  time.Time `json:"occurrence"`
	WorkspaceID string    `json:"workspace_id"`
	ItemID      string    `json:"item_id,omitempty"`
	Outcome     string    `json:"outcome"`
	Reason      string    `json:"reason,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Input creates or changes a schedule. On an update, empty fields keep
// their value and Enabled nil keeps it.
type Input struct {
	Name      string `json:"name"`
	OwnerKind string `json:"owner_kind"`
	OwnerID   string `json:"owner_id"`
	Template  string `json:"template"`
	Rhythm    string `json:"rhythm"`
	Timezone  string `json:"timezone"`
	Enabled   *bool  `json:"enabled"`
}

// Owners resolves who a schedule is for (pipeline.Service).
type Owners interface {
	ScheduleOwner(ctx context.Context, kind, ref string, write bool) (string, error)
	OwnerWorkspaces(ctx context.Context, kind, id string) ([]string, error)
}

// Items creates and reads items (inbox.Service).
type Items interface {
	CreateItem(ctx context.Context, in inbox.CreateInput) (*inbox.Item, error)
	ReadItem(ctx context.Context, id string) (*inbox.Item, error)
}

// HolderCheck reports whether the key that holds a schedule may still start
// items in a workspace as an approver ("" = root: always).
type HolderCheck func(ctx context.Context, keyID, ws string) bool

// Notifier tells the person what a firing did: Started for an item it
// created, Note for one it did not.
type Notifier interface {
	ScheduleStarted(ctx context.Context, itemID, schedule string)
	ScheduleNote(ctx context.Context, text string)
}

// AuditRecorder writes the audit log.
type AuditRecorder interface {
	Record(ctx context.Context, in audit.RecordInput) error
}

// Service holds the schedules and fires them.
type Service struct {
	db       *sql.DB
	owners   Owners
	items    Items
	audit    AuditRecorder
	notifier Notifier
	holder   HolderCheck
	now      func() time.Time
	mu       sync.Mutex // one firing pass at a time
}

func NewService(db *sql.DB, owners Owners, items Items) *Service {
	return &Service{db: db, owners: owners, items: items, now: time.Now}
}

func (s *Service) SetAuditRecorder(a AuditRecorder) { s.audit = a }
func (s *Service) SetNotifier(n Notifier)           { s.notifier = n }

// SetHolderCheck wires the check of the holder's key at every firing (nil =
// none: tests).
func (s *Service) SetHolderCheck(fn HolderCheck) { s.holder = fn }

// --- storage ---

const columns = `id, name, owner_kind, owner_id, template, rhythm, timezone, enabled, next_ms, last_ms, created_ms, created_by, updated_ms, updated_by, holder_key`

func scan(row interface{ Scan(...any) error }) (*Schedule, error) {
	var sc Schedule
	var enabled int
	var next, created, updated int64
	var last sql.NullInt64
	if err := row.Scan(&sc.ID, &sc.Name, &sc.OwnerKind, &sc.OwnerID, &sc.Template, &sc.Rhythm, &sc.Timezone, &enabled,
		&next, &last, &created, &sc.CreatedBy, &updated, &sc.UpdatedBy, &sc.HolderKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	sc.Enabled = enabled == 1
	if next > 0 {
		sc.Next = time.UnixMilli(next).UTC()
	}
	if last.Valid {
		sc.Last = time.UnixMilli(last.Int64).UTC()
	}
	sc.CreatedAt, sc.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return &sc, nil
}

func (s *Service) get(ctx context.Context, id string) (*Schedule, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM schedules WHERE id = ?`, id))
}

func (s *Service) all(ctx context.Context, where string, args ...any) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM schedules `+where+` ORDER BY owner_kind, owner_id, name`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Schedule
	for rows.Next() {
		sc, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sc)
	}
	return out, rows.Err()
}

func msOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func (s *Service) save(ctx context.Context, sc *Schedule, create bool) error {
	enabled := 0
	if sc.Enabled {
		enabled = 1
	}
	var err error
	if create {
		_, err = s.db.ExecContext(ctx, `INSERT INTO schedules (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sc.ID, sc.Name, sc.OwnerKind, sc.OwnerID, sc.Template, sc.Rhythm, sc.Timezone, enabled, sc.Next.UnixMilli(), msOrNil(sc.Last),
			sc.CreatedAt.UnixMilli(), sc.CreatedBy, sc.UpdatedAt.UnixMilli(), sc.UpdatedBy, sc.HolderKey)
	} else {
		_, err = s.db.ExecContext(ctx, `UPDATE schedules SET name = ?, template = ?, rhythm = ?, timezone = ?, enabled = ?, next_ms = ?,
			updated_ms = ?, updated_by = ?, holder_key = ? WHERE id = ?`,
			sc.Name, sc.Template, sc.Rhythm, sc.Timezone, enabled, sc.Next.UnixMilli(), sc.UpdatedAt.UnixMilli(), sc.UpdatedBy, sc.HolderKey, sc.ID)
	}
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("%w: a schedule named %s is already held here", ErrConflict, sc.Name)
	}
	return err
}

// --- the person's side ---

func (s *Service) record(ctx context.Context, sc *Schedule, action, via, notes string) {
	if s.audit == nil {
		return
	}
	ws := ""
	if sc.OwnerKind == KindWorkspace {
		ws = sc.OwnerID
	}
	_ = s.audit.Record(ctx, audit.RecordInput{WorkspaceID: ws, EntityType: AuditEntity, EntityID: sc.ID, Action: action, Via: via, Notes: notes})
}

// next is the schedule's next occurrence after `after`.
func next(sc *Schedule, after time.Time) (time.Time, error) {
	r, err := ParseRhythm(sc.Rhythm)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	loc, err := time.LoadLocation(sc.Timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: timezone %q", ErrInvalidInput, sc.Timezone)
	}
	t := r.Next(after, loc)
	if t.IsZero() {
		return t, fmt.Errorf("%w: the rhythm %q never comes round", ErrInvalidInput, sc.Rhythm)
	}
	return t.UTC(), nil
}

// normalize checks the fields a person gives.
func normalize(sc *Schedule) error {
	sc.Name = strings.TrimSpace(strings.ToLower(sc.Name))
	sc.Template = strings.TrimSpace(strings.ToLower(sc.Template))
	sc.Rhythm = strings.Join(strings.Fields(sc.Rhythm), " ")
	sc.Timezone = strings.TrimSpace(sc.Timezone)
	if sc.Timezone == "" {
		sc.Timezone = "UTC"
	}
	switch {
	case !reName.MatchString(sc.Name):
		return fmt.Errorf("%w: name %q (lowercase letters, digits, - and _)", ErrInvalidInput, sc.Name)
	case !reName.MatchString(sc.Template):
		return fmt.Errorf("%w: template %q (a template's name)", ErrInvalidInput, sc.Template)
	}
	return nil
}

// Create holds a new schedule: an approver in the workspace's scope, root
// for a group (as for templates).
func (s *Service) Create(ctx context.Context, in Input, via string) (*Schedule, error) {
	kind := strings.TrimSpace(in.OwnerKind)
	if kind != KindWorkspace && kind != KindGroup {
		return nil, fmt.Errorf("%w: owner_kind is workspace or group", ErrInvalidInput)
	}
	owner, err := s.owners.ScheduleOwner(ctx, kind, in.OwnerID, true)
	if err != nil {
		return nil, err
	}
	actor, keyID := auth.Actor(ctx)
	now := s.now().UTC()
	sc := &Schedule{ID: ids.New(), Name: in.Name, OwnerKind: kind, OwnerID: owner, Template: in.Template, Rhythm: in.Rhythm, Timezone: in.Timezone,
		Enabled: in.Enabled == nil || *in.Enabled, CreatedAt: now, CreatedBy: actor, UpdatedAt: now, UpdatedBy: actor, HolderKey: keyID}
	if err := normalize(sc); err != nil {
		return nil, err
	}
	if sc.Next, err = next(sc, now); err != nil {
		return nil, err
	}
	if err := s.save(ctx, sc, true); err != nil {
		return nil, err
	}
	s.record(ctx, sc, "schedule.create", via, describe(sc))
	return sc, nil
}

// Update changes a schedule: who may create it may change it.
func (s *Service) Update(ctx context.Context, id string, in Input, via string) (*Schedule, error) {
	sc, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.owners.ScheduleOwner(ctx, sc.OwnerKind, sc.OwnerID, true); err != nil {
		return nil, err
	}
	before := describe(sc)
	if in.Name != "" {
		sc.Name = in.Name
	}
	if in.Template != "" {
		sc.Template = in.Template
	}
	if in.Rhythm != "" {
		sc.Rhythm = in.Rhythm
	}
	if in.Timezone != "" {
		sc.Timezone = in.Timezone
	}
	if in.Enabled != nil {
		sc.Enabled = *in.Enabled
	}
	if err := normalize(sc); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if sc.Next, err = next(sc, now); err != nil {
		return nil, err
	}
	sc.UpdatedAt = now
	// Who changes it holds it from then on.
	sc.UpdatedBy, sc.HolderKey = auth.Actor(ctx)
	if err := s.save(ctx, sc, false); err != nil {
		return nil, err
	}
	if after := describe(sc); after != before {
		s.record(ctx, sc, "schedule.update", via, after)
	}
	return sc, nil
}

// Delete removes a schedule and its firings' record.
func (s *Service) Delete(ctx context.Context, id, via string) error {
	sc, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.owners.ScheduleOwner(ctx, sc.OwnerKind, sc.OwnerID, true); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schedule_firings WHERE schedule_id = ?`, id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id); err != nil {
		return err
	}
	s.record(ctx, sc, "schedule.delete", via, describe(sc))
	return nil
}

// Get reads one schedule the caller may see (a foreign one is not found).
func (s *Service) Get(ctx context.Context, id string) (*Schedule, error) {
	sc, err := s.get(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if !s.mayRead(ctx, sc) {
		return nil, ErrNotFound
	}
	return sc, nil
}

func (s *Service) mayRead(ctx context.Context, sc *Schedule) bool {
	_, err := s.owners.ScheduleOwner(ctx, sc.OwnerKind, sc.OwnerID, false)
	return err == nil
}

// List lists the schedules the caller may see.
func (s *Service) List(ctx context.Context) ([]Schedule, error) {
	all, err := s.all(ctx, "")
	if err != nil {
		return nil, err
	}
	out := []Schedule{}
	for i := range all {
		if s.mayRead(ctx, &all[i]) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// Firings lists a schedule's newest firings.
func (s *Service) Firings(ctx context.Context, id string) ([]Firing, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, schedule_id, occurrence_ms, workspace_id, item_id, outcome, reason, created_ms
		FROM schedule_firings WHERE schedule_id = ? ORDER BY occurrence_ms DESC, workspace_id LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Firing{}
	for rows.Next() {
		var f Firing
		var occ, created int64
		if err := rows.Scan(&f.ID, &f.ScheduleID, &occ, &f.WorkspaceID, &f.ItemID, &f.Outcome, &f.Reason, &created); err != nil {
			return nil, err
		}
		f.Occurrence, f.CreatedAt = time.UnixMilli(occ).UTC(), time.UnixMilli(created).UTC()
		out = append(out, f)
	}
	return out, rows.Err()
}

// RunNow fires a schedule at once (to try a template out); its rhythm is
// unchanged.
func (s *Service) RunNow(ctx context.Context, id, via string) ([]Firing, error) {
	sc, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.owners.ScheduleOwner(ctx, sc.OwnerKind, sc.OwnerID, true); err != nil {
		return nil, err
	}
	s.record(ctx, sc, "schedule.run", via, "fired by hand")
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fire(context.WithoutCancel(ctx), sc, s.now().UTC().Truncate(time.Millisecond))
}

func describe(sc *Schedule) string {
	state := "on"
	if !sc.Enabled {
		state = "paused"
	}
	return fmt.Sprintf("schedule %s: template %s, rhythm %q %s, %s", sc.Name, sc.Template, sc.Rhythm, sc.Timezone, state)
}

// --- firing ---

// Start runs the firing loop until ctx ends: due schedules are fired once a
// minute.
func (s *Service) Start(ctx context.Context) {
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			s.FireDue(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

// FireDue fires every enabled schedule whose time has come, then moves it to
// its next occurrence after now: occurrences missed while the server was
// down are fired once, not one by one.
func (s *Service) FireDue(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	due, err := s.all(ctx, `WHERE enabled = 1 AND next_ms <= ?`, now.UnixMilli())
	if err != nil {
		return
	}
	for i := range due {
		sc := &due[i]
		occurrence := sc.Next
		_, _ = s.fire(ctx, sc, occurrence)
		nxt, err := next(sc, now)
		if err != nil {
			// A rhythm that no longer parses (a timezone removed): pause it
			// rather than fire it every pass.
			_, _ = s.db.ExecContext(ctx, `UPDATE schedules SET enabled = 0 WHERE id = ?`, sc.ID)
			s.note(ctx, fmt.Sprintf("📅 Schedule %s is paused: %v", sc.Name, err))
			continue
		}
		_, _ = s.db.ExecContext(ctx, `UPDATE schedules SET next_ms = ?, last_ms = ? WHERE id = ?`, nxt.UnixMilli(), occurrence.UnixMilli(), sc.ID)
	}
}

// fire creates the occurrence's items, one per workspace.
func (s *Service) fire(ctx context.Context, sc *Schedule, occurrence time.Time) ([]Firing, error) {
	workspaces, err := s.owners.OwnerWorkspaces(ctx, sc.OwnerKind, sc.OwnerID)
	if err != nil {
		return nil, err
	}
	sort.Strings(workspaces)
	// The server starts the items in the name of the schedule, with the
	// approver's permission the person who held it had.
	pctx := auth.WithPrincipal(ctx, &auth.Principal{Root: true, Approver: true, KeyID: "schedule", Name: "schedule", OnBehalfOf: "schedule " + sc.Name})
	var out []Firing
	for _, ws := range workspaces {
		f := Firing{ID: ids.New(), ScheduleID: sc.ID, Occurrence: occurrence, WorkspaceID: ws, Outcome: OutcomeCreating, CreatedAt: s.now().UTC()}
		res, err := s.db.ExecContext(ctx, `INSERT INTO schedule_firings (id, schedule_id, occurrence_ms, workspace_id, item_id, outcome, reason, created_ms)
			VALUES (?, ?, ?, ?, '', ?, '', ?) ON CONFLICT (schedule_id, occurrence_ms, workspace_id) DO NOTHING`,
			f.ID, sc.ID, occurrence.UnixMilli(), ws, OutcomeCreating, f.CreatedAt.UnixMilli())
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // this occurrence already fired here
		}
		f.Outcome, f.ItemID, f.Reason = s.fireOne(pctx, sc, ws, occurrence)
		_, _ = s.db.ExecContext(ctx, `UPDATE schedule_firings SET outcome = ?, item_id = ?, reason = ? WHERE id = ?`, f.Outcome, f.ItemID, f.Reason, f.ID)
		switch f.Outcome {
		case OutcomeCreated:
			if s.notifier != nil {
				s.notifier.ScheduleStarted(ctx, f.ItemID, sc.Name)
			}
		default:
			s.note(ctx, fmt.Sprintf("📅 Schedule %s did not start an item in `%s`: %s", sc.Name, ws, f.Reason))
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *Service) fireOne(ctx context.Context, sc *Schedule, ws string, occurrence time.Time) (outcome, itemID, reason string) {
	// The person behind the schedule must still be allowed to start the
	// work: a revoked key, or one no longer an approver for the workspace,
	// stops it here.
	if s.holder != nil && !s.holder(ctx, sc.HolderKey, ws) {
		return OutcomeSkipped, "", "the key that holds the schedule no longer approves for this workspace: whoever may, saves the schedule again to hold it"
	}
	// The item it started last time here is still open: none is added.
	var prev string
	// By creation, not occurrence: an item started by hand after the due
	// time is the newest one too.
	err := s.db.QueryRowContext(ctx, `SELECT item_id FROM schedule_firings
		WHERE schedule_id = ? AND workspace_id = ? AND outcome = ? AND item_id != ''
		ORDER BY created_ms DESC, id DESC LIMIT 1`, sc.ID, ws, OutcomeCreated).Scan(&prev)
	if err == nil && prev != "" {
		if it, err := s.items.ReadItem(ctx, prev); err == nil && it.Status != inbox.StatusDone && it.Status != inbox.StatusDiscarded {
			return OutcomeSkipped, "", fmt.Sprintf("the item it started before (%s) is still %s", prev, strings.ReplaceAll(string(it.Status), "_", " "))
		}
	}
	item, err := s.items.CreateItem(ctx, inbox.CreateInput{WorkspaceID: ws, Template: sc.Template, AuthorAgentID: "schedule:" + sc.Name,
		Start: inbox.StartQueue, Via: audit.ViaSystem})
	if err != nil {
		return OutcomeFailed, "", firstLine(err.Error())
	}
	if item.StartError != "" {
		return OutcomeCreated, item.ID, "created, not queued: " + firstLine(item.StartError)
	}
	return OutcomeCreated, item.ID, ""
}

func (s *Service) note(ctx context.Context, text string) {
	if s.notifier != nil {
		s.notifier.ScheduleNote(ctx, text)
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
