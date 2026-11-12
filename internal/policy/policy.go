// Package policy decides which runners may work on a workspace
// (tmp/central-config — design.md, phase 1a). It is pure: rules, the facts
// a worker reports about a runner, and the match between them. Storage and
// who may change a policy live in internal/pipeline; the claim that applies
// it in internal/runs.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrInvalid rejects a policy that cannot be stored.
var ErrInvalid = errors.New("invalid policy")

// StageEmbeddings is the pseudo-stage of the code index's embeddings
// endpoint: a workspace whose policy does not allow that endpoint's
// provider for it is not embedded.
const StageEmbeddings = "embeddings"

// stages a rule may name: the pipeline's, and embeddings.
var stages = map[string]bool{
	"plan": true, "implement": true, "review": true, "qa": true,
	"compare": true, "synthesize": true, "second_opinion": true, StageEmbeddings: true,
}

// kinds a rule may name: the runner kinds the worker knows.
var kinds = map[string]bool{"claude": true, "opencode": true, "codex": true, "antigravity": true, "command": true}

// Rule allows a runner when every field it sets matches the runner's
// facts. Provider, Model and Runner are globs (`*` matches any run of
// characters, `/` included; `?` one character), compared without regard to
// case. A rule that sets no provider matches any provider, an unknown one
// included.
type Rule struct {
	Provider string   `json:"provider,omitempty" toml:"provider,omitempty"`
	Model    string   `json:"model,omitempty" toml:"model,omitempty"`
	Kind     string   `json:"kind,omitempty" toml:"kind,omitempty"`
	Runner   string   `json:"runner,omitempty" toml:"runner,omitempty"`
	Stages   []string `json:"stages,omitempty" toml:"stages,omitempty"`
	// Executor: how the stage must be contained — "isolated", "contained",
	// or a glob on the executor id (executor.go). Empty = any executor.
	Executor string `json:"executor,omitempty" toml:"executor,omitempty"`
}

// Policy is a list of allow rules: a runner may be used when at least one
// matches. No rules allows nothing.
type Policy struct {
	Allow []Rule `json:"allow" toml:"allow"`
}

// Facts are what a worker reports about one runner it offers.
type Facts struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind,omitempty"`
	Model    string   `json:"model,omitempty"`
	Effort   string   `json:"effort,omitempty"`
	Provider string   `json:"provider,omitempty"`
	Endpoint string   `json:"endpoint,omitempty"`
	Stages   []string `json:"stages,omitempty"` // the stages it can run; empty = all
	// Executor: the executor a run of this runner would get in the
	// workspace and stage being decided (executor.go). The worker reports
	// executors apart from the runner facts; whoever matches a rule fills
	// this in for the case at hand. "" = not reported.
	Executor string `json:"executor,omitempty"`
}

// CanRun reports whether the runner can run stage at all (a read-only
// runner cannot implement), and — when the executor for the case at hand is
// filled in — whether the worker can run it there (executor none:…).
func (f Facts) CanRun(stage string) bool {
	if Unavailable(f.Executor) {
		return false
	}
	if len(f.Stages) == 0 {
		return true
	}
	for _, s := range f.Stages {
		if s == stage {
			return true
		}
	}
	return false
}

// Normalize validates a policy and returns its stored form: fields trimmed
// and lower-cased where case carries no meaning, stages sorted and
// deduplicated.
func Normalize(p Policy) (Policy, error) {
	out := Policy{Allow: make([]Rule, 0, len(p.Allow))}
	if len(p.Allow) > 64 {
		return out, fmt.Errorf("%w: at most 64 rules", ErrInvalid)
	}
	for i, r := range p.Allow {
		n := Rule{
			Provider: strings.ToLower(strings.TrimSpace(r.Provider)),
			Model:    strings.TrimSpace(r.Model),
			Kind:     strings.ToLower(strings.TrimSpace(r.Kind)),
			Runner:   strings.TrimSpace(r.Runner),
			Executor: strings.ToLower(strings.TrimSpace(r.Executor)),
		}
		if err := validExecutorRequirement(n.Executor); err != nil {
			return out, fmt.Errorf("%w: rule %d: %v", ErrInvalid, i+1, err)
		}
		for name, v := range map[string]string{"provider": n.Provider, "model": n.Model, "runner": n.Runner, "executor": n.Executor} {
			if len(v) > 200 || strings.ContainsAny(v, " \t\r\n") {
				return out, fmt.Errorf("%w: rule %d: %s must be one word of at most 200 characters", ErrInvalid, i+1, name)
			}
		}
		if n.Kind != "" && !kinds[n.Kind] {
			return out, fmt.Errorf("%w: rule %d: unknown kind %q (claude, opencode, codex, antigravity, command)", ErrInvalid, i+1, r.Kind)
		}
		seen := map[string]bool{}
		for _, s := range r.Stages {
			s = strings.ToLower(strings.TrimSpace(s))
			if s == "" || seen[s] {
				continue
			}
			if !stages[s] {
				return out, fmt.Errorf("%w: rule %d: unknown stage %q (%s)", ErrInvalid, i+1, s, strings.Join(StageNames(), ", "))
			}
			seen[s] = true
			n.Stages = append(n.Stages, s)
		}
		sort.Strings(n.Stages)
		out.Allow = append(out.Allow, n)
	}
	return out, nil
}

// StageNames lists the stages a rule may name.
func StageNames() []string {
	out := make([]string, 0, len(stages))
	for s := range stages {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Matches reports whether the rule allows a runner with these facts for a
// stage.
func (r Rule) Matches(f Facts, stage string) bool {
	if len(r.Stages) > 0 {
		found := false
		for _, s := range r.Stages {
			if s == stage {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if r.Kind != "" && !strings.EqualFold(r.Kind, f.Kind) {
		return false
	}
	return (r.Provider == "" || glob(r.Provider, f.Provider)) &&
		(r.Model == "" || glob(r.Model, f.Model)) &&
		(r.Runner == "" || glob(r.Runner, f.Name)) &&
		ExecutorSatisfies(r.Executor, f.Executor)
}

// CouldMatch is Matches without the executor requirement: whether the rule
// allows the runner on some worker, whatever contains its runs there.
func (r Rule) CouldMatch(f Facts, stage string) bool {
	r.Executor = ""
	return r.Matches(f, stage)
}

// String renders the rule the way the CLI takes it.
func (r Rule) String() string {
	var parts []string
	for _, kv := range [][2]string{{"provider", r.Provider}, {"model", r.Model}, {"kind", r.Kind}, {"runner", r.Runner}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	if len(r.Stages) > 0 {
		parts = append(parts, "stages="+strings.Join(r.Stages, "|"))
	}
	if r.Executor != "" {
		parts = append(parts, "executor="+r.Executor)
	}
	if len(parts) == 0 {
		return "any"
	}
	return strings.Join(parts, ",")
}

// ParseRule reads a rule as the CLI writes it: comma-separated key=value
// pairs (provider, model, kind, runner, executor, stages with `|` between
// names), or
// "any" for a rule that matches every runner.
func ParseRule(s string) (Rule, error) {
	var r Rule
	s = strings.TrimSpace(s)
	if s == "" {
		return r, fmt.Errorf("%w: empty rule", ErrInvalid)
	}
	if strings.EqualFold(s, "any") {
		return r, nil
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if !ok || v == "" {
			return r, fmt.Errorf("%w: rule %q: %q is not key=value", ErrInvalid, s, part)
		}
		if k == "stage" {
			k = "stages"
		}
		if seen[k] && k != "stages" {
			return r, fmt.Errorf("%w: rule %q sets %s twice (one rule, one value: add another --allow)", ErrInvalid, s, k)
		}
		seen[k] = true
		switch k {
		case "provider":
			r.Provider = v
		case "model":
			r.Model = v
		case "kind":
			r.Kind = v
		case "runner":
			r.Runner = v
		case "stages":
			r.Stages = append(r.Stages, strings.Split(v, "|")...)
		case "executor":
			r.Executor = v
		default:
			return r, fmt.Errorf("%w: rule %q: unknown key %q (provider, model, kind, runner, stages, executor)", ErrInvalid, s, k)
		}
	}
	return r, nil
}

// Layer is one policy that applies to a workspace, and where it comes from.
type Layer struct {
	// Source: "fleet ceiling", "fleet default", "group <name>" or
	// "workspace".
	Source string `json:"source"`
	// Rev is the revision that holds it.
	Rev    string `json:"rev"`
	Policy Policy `json:"policy"`
}

// Effective is every policy that applies to one workspace. A runner must
// pass each layer: the fleet ceiling when there is one, then the
// workspace's and its groups' policies — or the fleet default when it has
// none of those. No layers means no policy: everything is allowed.
type Effective struct {
	Layers []Layer `json:"layers"`
	// Unreadable: the policy could not be read (DenyAll). Nothing passes
	// it, and nothing irreversible should be decided from it either: the
	// caller holds off instead of treating the workspace as refused.
	Unreadable bool `json:"-"`
}

// DenyAll is a policy nothing passes, for when the real one cannot be
// read: failing closed beats treating an unreadable policy as none.
func DenyAll(why string) *Effective {
	return &Effective{Layers: []Layer{{Source: why, Rev: "deny-all"}}, Unreadable: true}
}

// Held reports whether the policy could not be read.
func (e *Effective) Held() bool { return e != nil && e.Unreadable }

// AllowAll reports whether no policy applies.
func (e *Effective) AllowAll() bool { return e == nil || len(e.Layers) == 0 }

// Decision is the outcome of matching one runner against a policy.
type Decision struct {
	Allowed bool
	// Rules: for each layer, the index of the rule that allowed the runner
	// (allowed decisions only).
	Rules []int
	// Refused names the layer that allowed nothing ("" when allowed).
	Refused string
}

// Allows matches a runner's facts against every layer for a stage.
func (e *Effective) Allows(f Facts, stage string) Decision {
	d := Decision{Allowed: true}
	if e.AllowAll() {
		return d
	}
	for _, l := range e.Layers {
		hit := -1
		for i, r := range l.Policy.Allow {
			if r.Matches(f, stage) {
				hit = i
				break
			}
		}
		if hit < 0 {
			return Decision{Refused: l.Source}
		}
		d.Rules = append(d.Rules, hit)
	}
	return d
}

// CouldAllow reports whether the runner could pass on some worker: every
// layer has a rule that allows it once the executor is left aside. A
// refusal here is certain — no worker's containment changes it.
func (e *Effective) CouldAllow(f Facts, stage string) Decision {
	d := Decision{Allowed: true}
	if e.AllowAll() {
		return d
	}
	for _, l := range e.Layers {
		hit := -1
		for i, r := range l.Policy.Allow {
			if r.CouldMatch(f, stage) {
				hit = i
				break
			}
		}
		if hit < 0 {
			return Decision{Refused: l.Source}
		}
		d.Rules = append(d.Rules, hit)
	}
	return d
}

// Rev identifies the effective policy: a hash of its layers' revisions, in
// order ("" when no policy applies).
func (e *Effective) Rev() string {
	if e.AllowAll() {
		return ""
	}
	h := sha256.New()
	for _, l := range e.Layers {
		h.Write([]byte(l.Rev))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Revs lists the layers' revisions, in order.
func (e *Effective) Revs() []string {
	if e.AllowAll() {
		return nil
	}
	out := make([]string, len(e.Layers))
	for i, l := range e.Layers {
		out[i] = l.Rev
	}
	return out
}

// Glob matches s against pattern the way a rule's fields do: without
// regard to case, `*` any run of characters (including `/`), `?` any one.
func Glob(pattern, s string) bool { return glob(pattern, s) }

// glob matches s against pattern without regard to case: `*` is any run of
// characters (including `/`), `?` any one character.
func glob(pattern, s string) bool {
	p, t := []rune(strings.ToLower(pattern)), []rune(strings.ToLower(s))
	pi, ti, star, mark := 0, 0, -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]):
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ti
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
