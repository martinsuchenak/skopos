package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/ids"
	"github.com/martinsuchenak/skopos/internal/labels"
)

// Item templates: a request that repeats ("update the dependencies and the
// related code"), held for a workspace or a group, from which an item is
// started instead of typed again. Held like briefs: every save is a new row
// of item_templates, the newest row of an owner's name is the template,
// deleted = 1 says there is none; a workspace's own wins over its group's.

// Template limits.
const (
	MaxTemplateBrief = 16 << 10
	maxTemplateTitle = 200
)

var reTemplateTag = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,63}$`)

// TemplateFields are what a template sets on the item it starts.
type TemplateFields struct {
	// Title and Brief may hold {date}, {month}, {year} and {workspace}.
	Title string   `json:"title"`
	Brief string   `json:"brief"`
	Tags  []string `json:"tags,omitempty"`
	Needs []string `json:"needs,omitempty"`
	// Agents: "", "panel" or "solo".
	Agents string `json:"agents,omitempty"`
	// AutoApprovePlan: the item's plan is approved automatically when the
	// planner asks no question.
	AutoApprovePlan bool `json:"auto_approve_plan,omitempty"`
}

// Template is one revision of a template.
type Template struct {
	TemplateFields
	Rev       string    `json:"rev"`
	OwnerKind string    `json:"owner_kind"`
	OwnerID   string    `json:"owner_id"`
	Name      string    `json:"name"`
	Deleted   bool      `json:"deleted,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	// Source: where a workspace gets it from, "workspace" or
	// "group:<name>" (reads for a workspace).
	Source string `json:"source,omitempty"`
}

// NormalizeTemplate checks a template's name and fields.
func NormalizeTemplate(name string, f TemplateFields) (string, TemplateFields, error) {
	name = strings.TrimSpace(name)
	if !reBriefName.MatchString(name) {
		return "", f, fmt.Errorf("%w: template name %q (lowercase letters, digits, - and _)", ErrInvalidInput, name)
	}
	f.Title = strings.Join(strings.Fields(f.Title), " ")
	f.Brief = strings.TrimSpace(strings.ReplaceAll(f.Brief, "\r\n", "\n"))
	switch {
	case f.Title == "":
		return "", f, fmt.Errorf("%w: the template has no title", ErrInvalidInput)
	case len(f.Title) > maxTemplateTitle:
		return "", f, fmt.Errorf("%w: the title is %d bytes; at most %d", ErrInvalidInput, len(f.Title), maxTemplateTitle)
	case len(f.Brief) > MaxTemplateBrief:
		return "", f, fmt.Errorf("%w: the brief is %d bytes; at most %d", ErrInvalidInput, len(f.Brief), MaxTemplateBrief)
	case !utf8.ValidString(f.Title + f.Brief), strings.ContainsRune(f.Title+f.Brief, 0):
		return "", f, fmt.Errorf("%w: the template is not text", ErrInvalidInput)
	}
	var tags []string
	seen := map[string]bool{}
	for _, t := range f.Tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		if !reTemplateTag.MatchString(t) {
			return "", f, fmt.Errorf("%w: tag %q (lowercase letters, digits and . _ / -)", ErrInvalidInput, t)
		}
		seen[t] = true
		tags = append(tags, t)
	}
	f.Tags = tags
	needs, err := labels.Normalize(f.Needs)
	if err != nil {
		return "", f, fmt.Errorf("%w: needs: %v", ErrInvalidInput, err)
	}
	f.Needs = needs
	switch f.Agents = strings.ToLower(strings.TrimSpace(f.Agents)); f.Agents {
	case "", "default":
		f.Agents = ""
	case "panel", "solo":
	default:
		return "", f, fmt.Errorf("%w: agents is panel, solo or empty, not %q", ErrInvalidInput, f.Agents)
	}
	return name, f, nil
}

// --- storage ---

const templateColumns = `id, owner_kind, owner_id, name, body, deleted, created_ms, created_by`

func scanTemplate(row interface{ Scan(...any) error }) (Template, error) {
	var t Template
	var body string
	var deleted int
	var ms int64
	if err := row.Scan(&t.Rev, &t.OwnerKind, &t.OwnerID, &t.Name, &body, &deleted, &ms, &t.CreatedBy); err != nil {
		return Template{}, err
	}
	t.Deleted, t.CreatedAt = deleted == 1, time.UnixMilli(ms).UTC()
	if !t.Deleted && body != "" {
		if err := json.Unmarshal([]byte(body), &t.TemplateFields); err != nil {
			return Template{}, fmt.Errorf("reading template %s: %w", t.Name, err)
		}
	}
	return t, nil
}

// InsertTemplate appends a revision.
func (s *Storage) InsertTemplate(ctx context.Context, t Template) error {
	body, deleted := "", 0
	if t.Deleted {
		deleted = 1
	} else {
		b, err := json.Marshal(t.TemplateFields)
		if err != nil {
			return err
		}
		body = string(b)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO item_templates (`+templateColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Rev, t.OwnerKind, t.OwnerID, t.Name, body, deleted, t.CreatedAt.UnixMilli(), t.CreatedBy)
	if err != nil {
		return fmt.Errorf("saving the template: %w", err)
	}
	return nil
}

// CurrentTemplate is the newest revision of an owner's template, deleted or
// not (ok = false when it was never set).
func (s *Storage) CurrentTemplate(ctx context.Context, kind, id, name string) (Template, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+templateColumns+` FROM item_templates
		WHERE owner_kind = ? AND owner_id = ? AND name = ? ORDER BY id DESC LIMIT 1`, kind, id, name)
	t, err := scanTemplate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Template{}, false, nil
	}
	if err != nil {
		return Template{}, false, err
	}
	return t, true, nil
}

// OwnerTemplates lists an owner's templates as they stand, by name.
func (s *Storage) OwnerTemplates(ctx context.Context, kind, id string) ([]Template, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+templateColumns+` FROM item_templates t
		WHERE owner_kind = ? AND owner_id = ?
		  AND id = (SELECT MAX(id) FROM item_templates WHERE owner_kind = t.owner_kind AND owner_id = t.owner_id AND name = t.name)
		  AND deleted = 0
		ORDER BY name`, kind, id)
	if err != nil {
		return nil, fmt.Errorf("listing templates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GroupsHoldingTemplate lists the groups that hold a template of this name.
func (s *Storage) GroupsHoldingTemplate(ctx context.Context, name string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_id FROM item_templates t
		WHERE owner_kind = 'group' AND name = ?
		  AND id = (SELECT MAX(id) FROM item_templates WHERE owner_kind = 'group' AND owner_id = t.owner_id AND name = t.name)
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

// effectiveTemplates resolves a workspace's templates: its own, then for
// every other name the first group (by name) that holds one.
func (s *Service) effectiveTemplates(ctx context.Context, ws string) ([]Template, error) {
	own, err := s.storage.OwnerTemplates(ctx, KindWorkspace, ws)
	if err != nil {
		return nil, err
	}
	byName := map[string]Template{}
	for _, t := range own {
		t.Source = KindWorkspace
		byName[t.Name] = t
	}
	groups, err := s.storage.GroupsContaining(ctx, ws)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		gt, err := s.storage.OwnerTemplates(ctx, KindGroup, g.ID)
		if err != nil {
			return nil, err
		}
		for _, t := range gt {
			if _, ok := byName[t.Name]; ok {
				continue
			}
			t.Source = "group:" + g.Name
			byName[t.Name] = t
		}
	}
	out := make([]Template, 0, len(byName))
	for _, t := range byName {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Templates lists the templates of a workspace (its own and the ones it
// inherits) or of a group. Read like the pipeline.
func (s *Service) Templates(ctx context.Context, kind, id string) ([]Template, error) {
	kind, id, err := s.authorizeRead(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if kind == KindGroup {
		return s.storage.OwnerTemplates(ctx, kind, id)
	}
	return s.effectiveTemplates(ctx, id)
}

// Template returns one template: for a workspace the one in force.
func (s *Service) Template(ctx context.Context, kind, id, name string) (*Template, error) {
	kind, id, err := s.authorizeRead(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	return s.lookupTemplate(ctx, kind, id, strings.TrimSpace(name))
}

func (s *Service) lookupTemplate(ctx context.Context, kind, id, name string) (*Template, error) {
	if kind == KindGroup {
		t, ok, err := s.storage.CurrentTemplate(ctx, kind, id, name)
		if err != nil {
			return nil, err
		}
		if !ok || t.Deleted {
			return nil, fmt.Errorf("%w: no template %q is held here", ErrNotFound, name)
		}
		return &t, nil
	}
	all, err := s.effectiveTemplates(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		if t.Name == name {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("%w: workspace %s has no template %q", ErrNotFound, id, name)
}

// TemplateFor is the template a workspace starts an item from: its own, else
// its group's. Internal caller (the inbox, which authorised the create;
// schedules): no permission check.
func (s *Service) TemplateFor(ctx context.Context, ws, name string) (*Template, error) {
	return s.lookupTemplate(ctx, KindWorkspace, strings.TrimSpace(ws), strings.TrimSpace(name))
}

// SetTemplate saves a template for a workspace (an approver in its scope)
// or a group (root). Saving what it already holds changes nothing.
func (s *Service) SetTemplate(ctx context.Context, kind, id, name string, f TemplateFields, via string) (*Template, error) {
	kind, id, err := s.authorizeBriefWrite(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	name, f, err = NormalizeTemplate(name, f)
	if err != nil {
		return nil, err
	}
	if kind == KindGroup {
		if err := s.checkTemplateConflict(ctx, id, name); err != nil {
			return nil, err
		}
	}
	before, had, err := s.storage.CurrentTemplate(ctx, kind, id, name)
	if err != nil {
		return nil, err
	}
	if had && !before.Deleted && sameTemplate(before.TemplateFields, f) {
		return &before, nil
	}
	actor, _ := auth.Actor(ctx)
	t := Template{TemplateFields: f, Rev: ids.New(), OwnerKind: kind, OwnerID: id, Name: name, CreatedAt: s.now().UTC(), CreatedBy: actor}
	if err := s.storage.InsertTemplate(ctx, t); err != nil {
		return nil, err
	}
	what := "set"
	if had && !before.Deleted {
		what = "changed"
	}
	notes := fmt.Sprintf("template %s %s: %q, brief %d bytes", name, what, f.Title, len(f.Brief))
	if f.AutoApprovePlan {
		notes += ", plan approved automatically"
	}
	if len(f.Needs) > 0 {
		notes += ", needs " + strings.Join(f.Needs, ",")
	}
	s.record(ctx, kind, id, "template.set", via, notes+" (revision "+t.Rev+")")
	s.publish(kind, id)
	return &t, nil
}

// DeleteTemplate removes an owner's template.
func (s *Service) DeleteTemplate(ctx context.Context, kind, id, name, via string) error {
	kind, id, err := s.authorizeBriefWrite(ctx, kind, id)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	before, had, err := s.storage.CurrentTemplate(ctx, kind, id, name)
	if err != nil {
		return err
	}
	if !had || before.Deleted {
		return fmt.Errorf("%w: no template %q is held here", ErrNotFound, name)
	}
	actor, _ := auth.Actor(ctx)
	t := Template{Rev: ids.New(), OwnerKind: kind, OwnerID: id, Name: name, Deleted: true, CreatedAt: s.now().UTC(), CreatedBy: actor}
	if err := s.storage.InsertTemplate(ctx, t); err != nil {
		return err
	}
	s.record(ctx, kind, id, "template.delete", via, fmt.Sprintf("template %s removed (revision %s)", name, t.Rev))
	s.publish(kind, id)
	return nil
}

func sameTemplate(a, b TemplateFields) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// checkTemplateConflict refuses a group holding a template another group
// holds for one of its workspaces: which request an item gets must never
// depend on group names.
func (s *Service) checkTemplateConflict(ctx context.Context, groupID, name string) error {
	holders, err := s.storage.GroupsHoldingTemplate(ctx, name)
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
				problems = append(problems, fmt.Sprintf("workspace %s is also in group %s, which holds a template %s", ws, g.Name, name))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%w: %s — hold it on one group only, or on the workspace", ErrConflict, strings.Join(problems, "; "))
	}
	return nil
}

// ItemTemplate is the template a workspace holds of a name, as the inbox
// starts an item from it (inbox.SetTemplates), with notFound telling the
// caller there is none.
func (s *Service) ItemTemplate(ctx context.Context, ws, name string) (t *Template, notFound bool, err error) {
	t, err = s.TemplateFor(ctx, ws, name)
	if errors.Is(err, ErrNotFound) {
		return nil, true, err
	}
	return t, false, err
}

// ScheduleOwner checks the caller may read (write = false) or hold
// (write = true) something for a workspace or group, as for templates, and
// returns the owner's id (a group named by its name or id).
func (s *Service) ScheduleOwner(ctx context.Context, kind, ref string, write bool) (string, error) {
	var id string
	var err error
	if write {
		_, id, err = s.authorizeBriefWrite(ctx, kind, ref)
	} else {
		_, id, err = s.authorizeRead(ctx, kind, ref)
	}
	return id, err
}

// OwnerWorkspaces lists the workspaces an owner stands for: a workspace
// itself, or a group's members (explicit and by pattern). Internal caller.
func (s *Service) OwnerWorkspaces(ctx context.Context, kind, id string) ([]string, error) {
	if kind == KindWorkspace {
		ok, err := s.storage.WorkspaceExists(ctx, id)
		if err != nil || !ok {
			return nil, err
		}
		return []string{id}, nil
	}
	return s.storage.GroupWorkspaces(ctx, id)
}
