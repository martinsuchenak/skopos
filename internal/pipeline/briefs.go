package pipeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/ids"
	"github.com/martinsuchenak/skopos/internal/runs"
)

// Briefs held on the server: the text a pipeline member with a focus is
// given (`focus@runner`), stored with the pipeline settings of a workspace
// or a group instead of in the repository. One on the server wins over the
// repository's `.skopos/focus/<name>.md` of the same name; a workspace's
// own wins over its group's.
//
// Every save is a new row (pipeline_briefs is append-only): the newest row
// of an owner's name is the brief, a row with deleted = 1 says there is
// none. The history is what was in force when.

// MaxBriefBytes bounds a brief: the size a worker reads of a repository's
// focus brief.
const MaxBriefBytes = 16 << 10

// reBriefName is a focus name, as the pipeline's members carry it and the
// worker's files are named.
var reBriefName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Brief is one revision of a brief.
type Brief struct {
	// Rev is the revision id (time-sortable).
	Rev       string    `json:"rev"`
	OwnerKind string    `json:"owner_kind"`
	OwnerID   string    `json:"owner_id"`
	Name      string    `json:"name"`
	Body      string    `json:"body,omitempty"`
	Deleted   bool      `json:"deleted,omitempty"`
	Bytes     int       `json:"bytes"`
	SHA256    string    `json:"sha256,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	// Source says where a workspace gets it from: "workspace" or
	// "group:<name>". Set on reads for a workspace.
	Source string `json:"source,omitempty"`
}

// BriefView is one brief as a workspace or a group has it, with its
// history (newest first, without bodies).
type BriefView struct {
	Brief
	History []Brief `json:"history,omitempty"`
}

func briefHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// NormalizeBrief checks a brief's name and text.
func NormalizeBrief(name, body string) (string, string, error) {
	name = strings.TrimSpace(name)
	if !reBriefName.MatchString(name) {
		return "", "", fmt.Errorf("%w: brief name %q (lowercase letters, digits, - and _, as in focus@runner)", ErrInvalidInput, name)
	}
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	switch {
	case body == "":
		return "", "", fmt.Errorf("%w: the brief is empty", ErrInvalidInput)
	case len(body) > MaxBriefBytes:
		return "", "", fmt.Errorf("%w: the brief is %d bytes; at most %d", ErrInvalidInput, len(body), MaxBriefBytes)
	case !utf8.ValidString(body) || strings.ContainsRune(body, 0):
		return "", "", fmt.Errorf("%w: the brief is not text", ErrInvalidInput)
	}
	return name, body, nil
}

// --- storage ---

const briefColumns = `id, owner_kind, owner_id, name, body, deleted, created_ms, created_by`

func scanBrief(row interface{ Scan(...any) error }, withBody bool) (Brief, error) {
	var b Brief
	var deleted int
	var ms int64
	if err := row.Scan(&b.Rev, &b.OwnerKind, &b.OwnerID, &b.Name, &b.Body, &deleted, &ms, &b.CreatedBy); err != nil {
		return Brief{}, err
	}
	b.Deleted, b.CreatedAt, b.Bytes = deleted == 1, time.UnixMilli(ms).UTC(), len(b.Body)
	if !b.Deleted {
		b.SHA256 = briefHash(b.Body)
	}
	if !withBody {
		b.Body = ""
	}
	return b, nil
}

// InsertBrief appends a revision.
func (s *Storage) InsertBrief(ctx context.Context, b Brief) error {
	deleted := 0
	if b.Deleted {
		deleted = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pipeline_briefs (`+briefColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.Rev, b.OwnerKind, b.OwnerID, b.Name, b.Body, deleted, b.CreatedAt.UnixMilli(), b.CreatedBy)
	if err != nil {
		return fmt.Errorf("saving the brief: %w", err)
	}
	return nil
}

// CurrentBrief is the newest revision of an owner's brief, deleted or not
// (ok = false when it was never set).
func (s *Storage) CurrentBrief(ctx context.Context, kind, id, name string) (Brief, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+briefColumns+` FROM pipeline_briefs
		WHERE owner_kind = ? AND owner_id = ? AND name = ? ORDER BY id DESC LIMIT 1`, kind, id, name)
	b, err := scanBrief(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return Brief{}, false, nil
	}
	if err != nil {
		return Brief{}, false, fmt.Errorf("reading the brief: %w", err)
	}
	return b, true, nil
}

// OwnerBriefs lists an owner's briefs as they stand (no deleted ones),
// by name.
func (s *Storage) OwnerBriefs(ctx context.Context, kind, id string, withBody bool) ([]Brief, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+briefColumns+` FROM pipeline_briefs b
		WHERE owner_kind = ? AND owner_id = ?
		  AND id = (SELECT MAX(id) FROM pipeline_briefs WHERE owner_kind = b.owner_kind AND owner_id = b.owner_id AND name = b.name)
		  AND deleted = 0
		ORDER BY name`, kind, id)
	if err != nil {
		return nil, fmt.Errorf("listing briefs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Brief
	for rows.Next() {
		b, err := scanBrief(rows, withBody)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BriefHistory lists every revision of an owner's brief, newest first,
// without bodies.
func (s *Storage) BriefHistory(ctx context.Context, kind, id, name string) ([]Brief, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+briefColumns+` FROM pipeline_briefs
		WHERE owner_kind = ? AND owner_id = ? AND name = ? ORDER BY id DESC LIMIT 50`, kind, id, name)
	if err != nil {
		return nil, fmt.Errorf("reading the brief's history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Brief
	for rows.Next() {
		b, err := scanBrief(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GroupsHoldingBrief lists the groups that hold a brief of this name now.
func (s *Storage) GroupsHoldingBrief(ctx context.Context, name string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_id FROM pipeline_briefs b
		WHERE owner_kind = 'group' AND name = ?
		  AND id = (SELECT MAX(id) FROM pipeline_briefs WHERE owner_kind = 'group' AND owner_id = b.owner_id AND name = b.name)
		  AND deleted = 0`, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// --- service ---

// authorizeBriefWrite: a workspace's briefs are its approvers' (they decide
// what the agents that check the work are told, as with the pipeline); a
// group's are root's.
func (s *Service) authorizeBriefWrite(ctx context.Context, kind, id string) (string, string, error) {
	if kind == KindWorkspace {
		if err := auth.RequireWorkspace(ctx, strings.TrimSpace(id)); err != nil {
			return kind, id, err
		}
		if err := auth.RequireApprover(ctx); err != nil {
			return kind, id, err
		}
	}
	return s.authorizeRead(ctx, kind, id)
}

// effectiveBriefs resolves a workspace's briefs: its own, then for every
// other name the first group (by name) that holds one. Two groups holding
// the same name is reported and resolved by group name until fixed.
func (s *Service) effectiveBriefs(ctx context.Context, ws string, withBody bool) ([]Brief, []string, error) {
	own, err := s.storage.OwnerBriefs(ctx, KindWorkspace, ws, withBody)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]Brief{}
	for _, b := range own {
		b.Source = KindWorkspace
		byName[b.Name] = b
	}
	groups, err := s.storage.GroupsContaining(ctx, ws)
	if err != nil {
		return nil, nil, err
	}
	holders := map[string][]string{}
	for _, g := range groups {
		gb, err := s.storage.OwnerBriefs(ctx, KindGroup, g.ID, withBody)
		if err != nil {
			return nil, nil, err
		}
		for _, b := range gb {
			holders[b.Name] = append(holders[b.Name], g.Name)
			if _, ok := byName[b.Name]; ok {
				continue
			}
			b.Source = "group:" + g.Name
			byName[b.Name] = b
		}
	}
	var conflicts []string
	for name, names := range holders {
		if len(names) > 1 && byName[name].Source != KindWorkspace {
			conflicts = append(conflicts, fmt.Sprintf("the brief %s is held by groups %s; using %s", name, strings.Join(names, ", "), names[0]))
		}
	}
	sort.Strings(conflicts)
	out := make([]Brief, 0, len(byName))
	for _, b := range byName {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, conflicts, nil
}

// Briefs lists the briefs of a workspace (its own and the ones it inherits,
// with where each comes from) or of a group, without bodies. Read like the
// pipeline: any key with the workspace in scope; groups root only.
func (s *Service) Briefs(ctx context.Context, kind, id string) ([]Brief, error) {
	kind, id, err := s.authorizeRead(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if kind == KindGroup {
		return s.storage.OwnerBriefs(ctx, kind, id, false)
	}
	out, _, err := s.effectiveBriefs(ctx, id, false)
	return out, err
}

// Brief returns one brief with its text and the history of the owner that
// holds it: for a workspace the one in force (its own, else its group's).
func (s *Service) Brief(ctx context.Context, kind, id, name string) (*BriefView, error) {
	kind, id, err := s.authorizeRead(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	var cur Brief
	found := false
	if kind == KindGroup {
		b, ok, err := s.storage.CurrentBrief(ctx, kind, id, name)
		if err != nil {
			return nil, err
		}
		cur, found = b, ok && !b.Deleted
	} else {
		all, _, err := s.effectiveBriefs(ctx, id, true)
		if err != nil {
			return nil, err
		}
		for _, b := range all {
			if b.Name == name {
				cur, found = b, true
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: no brief %q is held here", ErrNotFound, name)
	}
	v := &BriefView{Brief: cur}
	// The history is the holder's own: a group's is shown to a workspace
	// that inherits it only by its newest revision.
	if cur.OwnerKind == kind && cur.OwnerID == id {
		if v.History, err = s.storage.BriefHistory(ctx, kind, id, name); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// SetBrief saves a brief for a workspace or a group. Saving the text it
// already has changes nothing.
func (s *Service) SetBrief(ctx context.Context, kind, id, name, body, via string) (*Brief, error) {
	kind, id, err := s.authorizeBriefWrite(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	name, body, err = NormalizeBrief(name, body)
	if err != nil {
		return nil, err
	}
	if kind == KindGroup {
		if err := s.checkBriefConflict(ctx, id, name); err != nil {
			return nil, err
		}
	}
	before, had, err := s.storage.CurrentBrief(ctx, kind, id, name)
	if err != nil {
		return nil, err
	}
	if had && !before.Deleted && before.Body == body {
		before.Body = ""
		return &before, nil
	}
	actor, _ := auth.Actor(ctx)
	b := Brief{Rev: ids.New(), OwnerKind: kind, OwnerID: id, Name: name, Body: body, CreatedAt: s.now().UTC(), CreatedBy: actor}
	if err := s.storage.InsertBrief(ctx, b); err != nil {
		return nil, err
	}
	b.Bytes, b.SHA256 = len(body), briefHash(body)
	what := "set"
	if had && !before.Deleted {
		what = "changed"
	}
	s.briefChanged(ctx, kind, id, "pipeline.brief_set", via,
		fmt.Sprintf("brief %s %s: %d bytes, sha256 %s (revision %s)", name, what, b.Bytes, b.SHA256[:12], b.Rev))
	b.Body = ""
	return &b, nil
}

// DeleteBrief removes an owner's brief: its members get the group's, or
// the repository's file, from then on.
func (s *Service) DeleteBrief(ctx context.Context, kind, id, name, via string) error {
	kind, id, err := s.authorizeBriefWrite(ctx, kind, id)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	before, had, err := s.storage.CurrentBrief(ctx, kind, id, name)
	if err != nil {
		return err
	}
	if !had || before.Deleted {
		return fmt.Errorf("%w: no brief %q is held here", ErrNotFound, name)
	}
	actor, _ := auth.Actor(ctx)
	b := Brief{Rev: ids.New(), OwnerKind: kind, OwnerID: id, Name: name, Deleted: true, CreatedAt: s.now().UTC(), CreatedBy: actor}
	if err := s.storage.InsertBrief(ctx, b); err != nil {
		return err
	}
	s.briefChanged(ctx, kind, id, "pipeline.brief_delete", via, fmt.Sprintf("brief %s removed (revision %s)", name, b.Rev))
	return nil
}

func (s *Service) briefChanged(ctx context.Context, kind, id, action, via, notes string) {
	s.invalidateBriefs()
	s.record(ctx, kind, id, action, via, notes)
	s.publish(kind, id)
}

// checkBriefConflict refuses a group holding a brief another group already
// holds for one of its workspaces: which text a run gets must never depend
// on group names.
func (s *Service) checkBriefConflict(ctx context.Context, groupID, name string) error {
	holders, err := s.storage.GroupsHoldingBrief(ctx, name)
	if err != nil {
		return err
	}
	delete(holders, groupID)
	if len(holders) == 0 {
		return nil
	}
	members, err := s.storage.GroupWorkspaces(ctx, groupID)
	if err != nil {
		return err
	}
	var problems []string
	for _, ws := range members {
		groups, err := s.storage.GroupsContaining(ctx, ws)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if holders[g.ID] {
				problems = append(problems, fmt.Sprintf("workspace %s is also in group %s, which holds a brief %s", ws, g.Name, name))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%w: %s — hold it on one group only, or on the workspace", ErrConflict, strings.Join(problems, "; "))
	}
	return nil
}

// --- the run queue's side ---

func (s *Service) invalidateBriefs() {
	s.mu.Lock()
	s.briefCache = map[string]cachedBriefs{}
	s.mu.Unlock()
}

type cachedBriefs struct {
	briefs []Brief
	at     time.Time
}

// workspaceBriefs is a workspace's briefs with their text, cached like the
// routing (a save drops the cache).
func (s *Service) workspaceBriefs(ctx context.Context, ws string) ([]Brief, error) {
	s.mu.Lock()
	c, ok := s.briefCache[ws]
	s.mu.Unlock()
	if ok && s.now().Sub(c.at) < cacheTTL {
		return c.briefs, nil
	}
	all, _, err := s.effectiveBriefs(ctx, ws, true)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.briefCache == nil {
		s.briefCache = map[string]cachedBriefs{}
	}
	s.briefCache[ws] = cachedBriefs{briefs: all, at: s.now()}
	s.mu.Unlock()
	return all, nil
}

// RunBrief is the brief the server holds for a run's focus, for the claim
// answer (nil = none: the worker reads the repository's). Internal: the
// claim has authorized the caller for the workspace.
func (s *Service) RunBrief(ctx context.Context, ws, focus string) (*runs.RunBrief, error) {
	if strings.TrimSpace(focus) == "" {
		return nil, nil
	}
	all, err := s.workspaceBriefs(ctx, ws)
	if err != nil {
		return nil, err
	}
	for _, b := range all {
		if b.Name == focus {
			return &runs.RunBrief{Name: b.Name, Body: b.Body, Rev: b.Rev, Source: b.Source}, nil
		}
	}
	return nil, nil
}

// HoldsBriefs reports whether a member of the workspace's pipeline has a
// focus the server holds a brief for: a worker that cannot take a brief
// from the claim answer would run such a member with other instructions.
// An unreadable state counts as held (the run then waits for a worker that
// can, rather than run with the wrong text).
func (s *Service) HoldsBriefs(ctx context.Context, ws string) bool {
	all, err := s.workspaceBriefs(ctx, ws)
	if err != nil {
		return true
	}
	if len(all) == 0 {
		return false
	}
	held := map[string]bool{}
	for _, b := range all {
		held[b.Name] = true
	}
	r, err := s.resolve(ctx, ws)
	if err != nil {
		return true
	}
	for _, st := range r.effective {
		for _, m := range st.Members {
			if m.Focus != "" && held[m.Focus] {
				return true
			}
		}
	}
	return false
}
