package wsconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/confrev"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/policy"
)

// RunnerAuditEntity is the audit-log entity type of runner definitions; the
// entity id is the runner's name.
const RunnerAuditEntity = "runner_def"

// RunnerDef defines a runner once, for every worker: which CLI it is and
// the model and effort it runs with. A worker offers it when one of its
// accounts is of that kind (and, when the account lists models, the model
// matches). Provider is the declared provider — what a runner policy
// matches; a worker whose endpoint contradicts it does not offer the runner.
type RunnerDef struct {
	Name     string `json:"name" toml:"-"`
	Kind     string `json:"kind" toml:"kind"`
	Model    string `json:"model,omitempty" toml:"model,omitempty"`
	Effort   string `json:"effort,omitempty" toml:"effort,omitempty"`
	Provider string `json:"provider,omitempty" toml:"provider,omitempty"`
}

// RunnerView is a definition with its revision.
type RunnerView struct {
	RunnerDef
	Rev       string    `json:"rev"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// Fleet is what every worker fetches before it claims: the runner
// definitions, named by one revision.
type Fleet struct {
	Rev     string      `json:"rev"`
	Runners []RunnerDef `json:"runners"`
}

// Kinds a runner definition may name. A `command` runner's argv is a
// command on the worker's host: it stays in the worker's file.
var DefinableKinds = []string{"claude", "opencode", "codex", "antigravity"}

var (
	reRunnerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reProvider   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	reEffortWord = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)
)

// effortLevels are the values each CLI's effort setting takes (from its
// --help / config reference). opencode's --variant is provider-specific and
// a command template's {effort} is the operator's own: any plain word.
var effortLevels = map[string][]string{
	"claude":      {"low", "medium", "high", "xhigh", "max"},
	"antigravity": {"low", "medium", "high", "xhigh", "max"},
	"codex":       {"minimal", "low", "medium", "high", "xhigh"},
}

// EffortLevels lists the effort values a runner kind takes; nil = any plain
// word (opencode variants, command templates).
func EffortLevels(kind string) []string { return effortLevels[kind] }

// ValidateEffort refuses an effort a runner kind cannot take ("" is always
// fine), so a setting is never silently dropped. command is the argv
// template of a command runner. The server (a runner definition) and the
// worker (an account) both use it.
func ValidateEffort(kind string, command []string, effort string) error {
	if effort == "" {
		return nil
	}
	if levels, ok := effortLevels[kind]; ok {
		if !slices.Contains(levels, effort) {
			return fmt.Errorf("effort %q: the %s runner takes %s", effort, kind, strings.Join(levels, ", "))
		}
		return nil
	}
	if !reEffortWord.MatchString(effort) {
		return fmt.Errorf("effort %q: use a plain word (letters, digits, - _ ., at most 32)", effort)
	}
	if kind == "command" && !TemplateHasEffort(command) {
		return fmt.Errorf("effort %q: the command has no {effort} placeholder", effort)
	}
	return nil
}

// TemplateHasEffort: a command template uses {effort}.
func TemplateHasEffort(command []string) bool {
	return slices.ContainsFunc(command, func(s string) bool { return strings.Contains(s, "{effort}") })
}

// NormalizeRunner validates a definition and returns its stored form.
func NormalizeRunner(d RunnerDef) (RunnerDef, error) {
	out := RunnerDef{Name: strings.TrimSpace(d.Name), Kind: strings.ToLower(strings.TrimSpace(d.Kind)),
		Model: strings.TrimSpace(d.Model), Effort: strings.TrimSpace(d.Effort), Provider: strings.ToLower(strings.TrimSpace(d.Provider))}
	bad := func(format string, args ...any) (RunnerDef, error) {
		return RunnerDef{}, fmt.Errorf("%w: runner %s: %s", ErrInvalid, out.Name, fmt.Sprintf(format, args...))
	}
	if !reRunnerName.MatchString(out.Name) {
		return RunnerDef{}, fmt.Errorf("%w: runner name %q: letters, digits, '.', '_' and '-', at most 64", ErrInvalid, d.Name)
	}
	if !slices.Contains(DefinableKinds, out.Kind) {
		if out.Kind == "command" {
			return bad("a command runner's argv runs on the worker's host: it stays in the worker's file (runner_config)")
		}
		return bad("kind %q is not one of %s", out.Kind, strings.Join(DefinableKinds, ", "))
	}
	if len(out.Model) > 200 || strings.ContainsAny(out.Model, " \t\n") {
		return bad("model %q is not a model name", out.Model)
	}
	if out.Kind == "opencode" && out.Model != "" && !strings.Contains(out.Model, "/") {
		return bad("an opencode model is <provider>/<model> (got %q)", out.Model)
	}
	if err := ValidateEffort(out.Kind, nil, out.Effort); err != nil {
		return bad("%v", err)
	}
	if out.Provider != "" {
		if !reProvider.MatchString(out.Provider) {
			return bad("provider %q: a lower-case name (letters, digits, '.', '_', '-')", out.Provider)
		}
		// What the CLI itself says about where the model lives must not be
		// contradicted: an opencode model names its provider.
		if out.Kind == "opencode" && out.Model != "" {
			if derived, _ := policy.ProviderForOpencode(out.Model, ""); policy.Contradicts(out.Provider, derived) {
				return bad("provider %q contradicts the model %s, which is %s's", out.Provider, out.Model, derived)
			}
		}
	}
	return out, nil
}

// Facts is what a policy can be asked about the definition before any
// worker reports: certain only when the provider is declared.
func (d RunnerDef) Facts() policy.Facts {
	return policy.Facts{Name: d.Name, Kind: d.Kind, Model: d.Model, Effort: d.Effort, Provider: d.Provider}
}

func runnerOwner(name string) string { return "runner:" + name }

// Runners lists the definitions, by name. Any authenticated caller may read
// them: they say which runner names mean what on every worker.
func (s *Service) Runners(ctx context.Context) ([]RunnerView, error) {
	heads, err := s.revs.Heads(ctx, KindRunnerDef)
	if err != nil {
		return nil, err
	}
	out := make([]RunnerView, 0, len(heads))
	for _, rev := range heads {
		var d RunnerDef
		if err := json.Unmarshal(rev.Content, &d); err != nil {
			return nil, fmt.Errorf("stored runner definition %s: %w", rev.ID, err)
		}
		out = append(out, RunnerView{RunnerDef: d, Rev: rev.ID, UpdatedAt: rev.CreatedAt, UpdatedBy: rev.Actor})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Fleet returns the definitions with the revision that names them all
// ("" when there are none).
func (s *Service) Fleet(ctx context.Context) (*Fleet, error) {
	views, err := s.Runners(ctx)
	if err != nil {
		return nil, err
	}
	f := &Fleet{Runners: make([]RunnerDef, len(views))}
	h := sha256.New()
	for i, v := range views {
		f.Runners[i] = v.RunnerDef
		h.Write([]byte(v.Name + "=" + v.Rev + "\n"))
	}
	if len(views) > 0 {
		f.Rev = hex.EncodeToString(h.Sum(nil))[:16]
	}
	return f, nil
}

// FleetRev is Fleet's revision alone, cached briefly: every claim answer
// carries it. "" when nothing is defined or it cannot be read (a worker
// keeps what it holds).
func (s *Service) FleetRev(ctx context.Context) string {
	s.mu.Lock()
	rev, at := s.fleetRev, s.fleetAt
	s.mu.Unlock()
	if !at.IsZero() && s.now().Sub(at) < cacheTTL {
		return rev
	}
	f, err := s.Fleet(ctx)
	if err != nil {
		return rev
	}
	s.mu.Lock()
	s.fleetRev, s.fleetAt = f.Rev, s.now()
	s.mu.Unlock()
	return f.Rev
}

// RunnerFacts returns the defined runners' facts by name, for the checks
// that can be certain before any worker reports (a pipeline save).
func (s *Service) RunnerFacts(ctx context.Context) (map[string]policy.Facts, error) {
	views, err := s.Runners(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]policy.Facts, len(views))
	for _, v := range views {
		out[v.Name] = v.Facts()
	}
	return out, nil
}

// PlanRunner checks a definition as SetRunner would and reports whether
// saving it would change anything ("create", "update" or "" = no change),
// writing nothing.
func (s *Service) PlanRunner(ctx context.Context, d RunnerDef) (string, error) {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return "", err
	}
	norm, err := NormalizeRunner(d)
	if err != nil {
		return "", err
	}
	content, _ := json.Marshal(norm)
	before, err := s.revs.Head(ctx, KindRunnerDef, runnerOwner(norm.Name))
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

// SetRunner defines or redefines a runner. Fleet admins only: it changes
// which model every worker runs under that name.
func (s *Service) SetRunner(ctx context.Context, d RunnerDef, via string) (*RunnerView, bool, error) {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return nil, false, err
	}
	via = audit.CleanVia(via)
	norm, err := NormalizeRunner(d)
	if err != nil {
		return nil, false, err
	}
	content, _ := json.Marshal(norm)
	owner := runnerOwner(norm.Name)
	before, err := s.revs.Head(ctx, KindRunnerDef, owner)
	if err != nil {
		return nil, false, err
	}
	actor, keyID := auth.Actor(ctx)
	rev, changed, err := s.revs.Put(ctx, KindRunnerDef, owner, content, confrev.Meta{Actor: actor, ActorKeyID: keyID, Via: via})
	if err != nil {
		return nil, false, err
	}
	if changed {
		action, was := "runner_def.create", "none"
		if before != nil {
			action, was = "runner_def.update", before.ID
		}
		s.recordRunner(ctx, norm.Name, action, via, "revision "+rev.ID+" (was "+was+"): "+describeRunner(norm))
		s.runnersChanged()
	}
	return &RunnerView{RunnerDef: norm, Rev: rev.ID, UpdatedAt: rev.CreatedAt, UpdatedBy: rev.Actor}, changed, nil
}

// DeleteRunner removes a definition: workers then offer the name only when
// their own file defines it.
func (s *Service) DeleteRunner(ctx context.Context, name, via string) (bool, error) {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return false, err
	}
	name = strings.TrimSpace(name)
	if !reRunnerName.MatchString(name) {
		return false, fmt.Errorf("%w: runner %s", ErrNotFound, name)
	}
	was, err := s.revs.Clear(ctx, KindRunnerDef, runnerOwner(name))
	if err != nil {
		return false, err
	}
	if was == nil {
		return false, fmt.Errorf("%w: runner %s is not defined", ErrNotFound, name)
	}
	s.recordRunner(ctx, name, "runner_def.delete", via, "was revision "+was.ID)
	s.runnersChanged()
	return true, nil
}

func (s *Service) runnersChanged() {
	s.mu.Lock()
	s.fleetAt = time.Time{}
	s.mu.Unlock()
	if s.publisher != nil {
		s.publisher.Publish(events.Event{Type: events.TypeWorkspaces})
	}
	if s.onRunners != nil {
		s.onRunners()
	}
}

func (s *Service) recordRunner(ctx context.Context, name, action, via, notes string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Record(ctx, audit.RecordInput{EntityType: RunnerAuditEntity, EntityID: name, Action: action, Via: via, Notes: notes})
}

func describeRunner(d RunnerDef) string {
	parts := []string{"kind " + d.Kind}
	for _, kv := range [][2]string{{"model", d.Model}, {"effort", d.Effort}, {"provider", d.Provider}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+" "+kv[1])
		}
	}
	return strings.Join(parts, ", ")
}
