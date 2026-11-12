package wsconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/confrev"
	"github.com/martinsuchenak/skopos/internal/events"
)

// Wrapper definitions (tmp/central-config — design.md, phase 2d): a wrapper
// is defined once, by name, and a workspace's or group's stage block
// attaches it to runs by rule (StageCaps.Wrap). Its argv is a command on
// every worker that uses it, so only a fleet admin defines one, and a
// worker runs it only with worker.server_commands.

// KindWrapper is the revision kind of wrapper definitions (owner
// "wrapper:<name>").
const KindWrapper = "wrapper"

// WrapperAuditEntity is the audit-log entity type of wrapper definitions;
// the entity id is the wrapper's name.
const WrapperAuditEntity = "wrapper"

// WrapperView is a definition with its revision and the executor id its
// runs record.
type WrapperView struct {
	WrapperDef
	Executor  string    `json:"executor"`
	Rev       string    `json:"rev"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	// UsedBy: the owners ("workspace:<id>", "group:<id>") whose stage
	// blocks name the wrapper.
	UsedBy []string `json:"used_by,omitempty"`
}

func wrapperOwner(name string) string { return "wrapper:" + name }

type wrapperSetKey struct{}

// WithWrapperSet makes config checks under ctx treat exactly these wrappers
// as defined, instead of the stored ones: a document that states the full
// set of wrappers is checked against the set it will leave behind.
func WithWrapperSet(ctx context.Context, names []string) context.Context {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return context.WithValue(ctx, wrapperSetKey{}, set)
}

// Wrappers lists the definitions, by name. Any authenticated caller may
// read them: a worker needs them to run what a stage block names.
func (s *Service) Wrappers(ctx context.Context) ([]WrapperView, error) {
	heads, err := s.revs.Heads(ctx, KindWrapper)
	if err != nil {
		return nil, err
	}
	out := make([]WrapperView, 0, len(heads))
	for _, rev := range heads {
		var d WrapperDef
		if err := json.Unmarshal(rev.Content, &d); err != nil {
			return nil, fmt.Errorf("stored wrapper definition %s: %w", rev.ID, err)
		}
		out = append(out, WrapperView{WrapperDef: d, Executor: d.Executor(), Rev: rev.ID, UpdatedAt: rev.CreatedAt, UpdatedBy: rev.Actor})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// WrapperUsers returns, per wrapper name, the owners whose stored stage
// blocks name it ("workspace:<id>" / "group:<id>", sorted). Internal: no
// scope check.
func (s *Service) WrapperUsers(ctx context.Context) (map[string][]string, error) {
	heads, err := s.revs.Heads(ctx, KindWorkspaceConfig)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for owner, rev := range heads {
		c, err := decode(rev)
		if err != nil {
			return nil, err
		}
		for _, name := range c.Wrappers() {
			out[name] = append(out[name], owner)
		}
	}
	for name := range out {
		sort.Strings(out[name])
	}
	return out, nil
}

// checkWrappers refuses a config whose stage blocks name a wrapper that is
// not defined: a rule would then stop every run it matches.
func (s *Service) checkWrappers(ctx context.Context, c *Config) error {
	if c == nil {
		return nil
	}
	names := c.Wrappers()
	if len(names) == 0 {
		return nil
	}
	defined, pending := ctx.Value(wrapperSetKey{}).(map[string]bool)
	if !pending {
		heads, err := s.revs.Heads(ctx, KindWrapper)
		if err != nil {
			return err
		}
		defined = make(map[string]bool, len(heads))
		for owner := range heads {
			defined[strings.TrimPrefix(owner, "wrapper:")] = true
		}
	}
	for _, name := range names {
		if !defined[name] {
			return fmt.Errorf("%w: stages: wrapper %q is not defined (define it first: skopos wrapper set %s --argv …)", ErrInvalid, name, name)
		}
	}
	return nil
}

// PlanWrapper checks a definition as SetWrapper would and reports whether
// saving it would change anything ("create", "update" or "" = no change),
// writing nothing.
func (s *Service) PlanWrapper(ctx context.Context, d WrapperDef) (string, error) {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return "", err
	}
	norm, err := NormalizeWrapper(d)
	if err != nil {
		return "", err
	}
	content, _ := json.Marshal(norm)
	before, err := s.revs.Head(ctx, KindWrapper, wrapperOwner(norm.Name))
	switch {
	case err != nil:
		return "", err
	case before == nil:
		return "create", nil
	case before.Hash != confrev.Hash(content):
		return "update", nil
	}
	return "", nil
}

// SetWrapper defines or redefines a wrapper. Fleet admins only: its argv
// runs on every worker a stage block sends it to.
func (s *Service) SetWrapper(ctx context.Context, d WrapperDef, via string) (*WrapperView, bool, error) {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return nil, false, err
	}
	via = audit.CleanVia(via)
	norm, err := NormalizeWrapper(d)
	if err != nil {
		return nil, false, err
	}
	content, _ := json.Marshal(norm)
	owner := wrapperOwner(norm.Name)
	before, err := s.revs.Head(ctx, KindWrapper, owner)
	if err != nil {
		return nil, false, err
	}
	actor, keyID := auth.Actor(ctx)
	rev, changed, err := s.revs.Put(ctx, KindWrapper, owner, content, confrev.Meta{Actor: actor, ActorKeyID: keyID, Via: via})
	if err != nil {
		return nil, false, err
	}
	if changed {
		action, was := "wrapper.create", "none"
		if before != nil {
			action, was = "wrapper.update", before.ID
		}
		// The argv is recorded in full: it is a command on every worker
		// that uses the wrapper.
		s.recordWrapper(ctx, norm.Name, action, via, "revision "+rev.ID+" (was "+was+"), executor "+norm.Executor()+": "+describeWrapper(norm))
		s.wrappersChanged()
	}
	return &WrapperView{WrapperDef: norm, Executor: norm.Executor(), Rev: rev.ID, UpdatedAt: rev.CreatedAt, UpdatedBy: rev.Actor}, changed, nil
}

// DeleteWrapper removes a definition. A wrapper a stored stage block still
// names is refused unless force is set: every run its rules match would
// stop until the rule goes or the wrapper is defined again.
func (s *Service) DeleteWrapper(ctx context.Context, name, via string, force bool) (bool, error) {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return false, err
	}
	name = strings.TrimSpace(name)
	if !reWrapperName.MatchString(name) {
		return false, fmt.Errorf("%w: wrapper %s", ErrNotFound, name)
	}
	if head, err := s.revs.Head(ctx, KindWrapper, wrapperOwner(name)); err != nil {
		return false, err
	} else if head == nil {
		return false, fmt.Errorf("%w: wrapper %s is not defined", ErrNotFound, name)
	}
	users, err := s.WrapperUsers(ctx)
	if err != nil {
		return false, err
	}
	if in := users[name]; len(in) > 0 && !force {
		return false, fmt.Errorf("%w: wrapper %s is named by the stage blocks of %s: runs their rules match would stop. Remove the rules first, or force the delete (skopos wrapper delete --force)", ErrConflict, name, strings.Join(in, ", "))
	}
	was, err := s.revs.Clear(ctx, KindWrapper, wrapperOwner(name))
	if err != nil {
		return false, err
	}
	if was == nil {
		return false, fmt.Errorf("%w: wrapper %s is not defined", ErrNotFound, name)
	}
	notes := "was revision " + was.ID
	if in := users[name]; len(in) > 0 {
		notes += "; forced while named by " + strings.Join(in, ", ")
	}
	s.recordWrapper(ctx, name, "wrapper.delete", audit.CleanVia(via), notes)
	s.wrappersChanged()
	return true, nil
}

func (s *Service) wrappersChanged() {
	s.mu.Lock()
	s.fleetAt = time.Time{}
	s.mu.Unlock()
	if s.publisher != nil {
		s.publisher.Publish(events.Event{Type: events.TypeWorkspaces})
	}
}

func (s *Service) recordWrapper(ctx context.Context, name, action, via, notes string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Record(ctx, audit.RecordInput{EntityType: WrapperAuditEntity, EntityID: name, Action: action, Via: via, Notes: notes})
}
