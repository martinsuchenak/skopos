package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/martinsuchenak/skopos/internal/policy"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// Runner definitions from the server (tmp/central-config — design.md,
// phase 2). The server defines each runner once — its kind, model, effort
// and declared provider. An account in the worker's file then only says
// which CLI it is a login for:
//
//	[[worker.accounts]]
//	name = "claude-1"
//	kind = "claude"
//	env  = { CLAUDE_CONFIG_DIR = "…" }
//
// and feeds every defined runner of that kind (models narrows it to the
// runners whose model matches a pattern). An account that names a runner
// itself keeps working: when the server defines that name, the
// definition's model and effort replace the account's.
//
// c.Accounts is what runs use: the file's accounts combined with the
// definitions. It is rebuilt whenever the definitions change, while the
// worker is idle.

// poolAccount: the account names a kind, not a runner.
func (a *Account) poolAccount() bool { return strings.TrimSpace(a.Kind) != "" }

// quota is where an account's quota state lives: an account built from the
// file's shares its login, and so its limits, with every other runner the
// same login feeds.
func (a *Account) quota() *Account {
	if a.base != nil {
		return a.base
	}
	return a
}

// def returns the server's definition of a runner, nil when it has none.
func (c *Config) def(name string) *wsconfig.RunnerDef {
	if name == "" {
		name = "claude"
	}
	for i := range c.defs {
		if c.defs[i].Name == name {
			return &c.defs[i]
		}
	}
	return nil
}

// RunnerDefs is the server's definitions the worker holds, and their
// revision ("" = none).
func (c *Config) RunnerDefs() ([]wsconfig.RunnerDef, string) { return c.defs, c.defsRev }

// localKind is the kind the worker's own file gives a runner name.
func (c *Config) localKind(name string) string {
	if name == "" {
		name = "claude"
	}
	if spec := c.RunnerSpecs[name]; spec != nil && spec.Kind != "" {
		return spec.Kind
	}
	return name
}

func modelsMatch(patterns []string, model string) bool {
	if len(patterns) == 0 {
		return true
	}
	return slices.ContainsFunc(patterns, func(p string) bool { return policy.Glob(p, model) })
}

// splitAccounts runs once at load: it keeps the file's accounts apart from
// the ones runs use, and checks the accounts that name a kind.
func (c *Config) splitAccounts() error {
	if c.fileAccounts == nil {
		c.fileAccounts = append([]*Account{}, c.Accounts...)
	}
	seen := map[string]bool{}
	for _, a := range c.fileAccounts {
		a.Kind = strings.ToLower(strings.TrimSpace(a.Kind))
		if !a.poolAccount() {
			continue
		}
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("an account with kind %q needs a name", a.Kind)
		}
		if seen[a.Name] {
			return fmt.Errorf("account %s: two accounts with a kind share the name", a.Name)
		}
		seen[a.Name] = true
		if !slices.Contains(wsconfig.DefinableKinds, a.Kind) {
			return fmt.Errorf("account %s: kind %q is not one of %s (a command runner names its runner)", a.Name, a.Kind, strings.Join(wsconfig.DefinableKinds, ", "))
		}
		if a.Runner != "" || a.Model != "" || a.Effort != "" {
			return fmt.Errorf("account %s: an account with kind takes its runners, model and effort from the server's runner definitions — remove runner, model and effort (models = [...] narrows which definitions it feeds)", a.Name)
		}
	}
	c.ApplyRunnerDefs(nil)
	return nil
}

// ApplyRunnerDefs rebuilds the accounts runs use from the file's accounts
// and the server's definitions (nil = none), and returns what an operator
// should know about the result. Call it only while no run is executing.
func (c *Config) ApplyRunnerDefs(f *wsconfig.Fleet) []string {
	c.defs, c.defsRev = nil, ""
	var wrappers []wsconfig.WrapperDef
	if f != nil {
		c.defs, c.defsRev = slices.Clone(f.Runners), f.Rev
		wrappers = f.Wrappers
	}
	// The wrapper definitions come with the runner definitions, under the
	// same revision (stagecaps.go).
	c.setWrappers(wrappers)
	var out []*Account
	var notes []string
	for _, a := range c.fileAccounts {
		if !a.poolAccount() {
			name := accountRunner(a)
			d := c.def(name)
			if d == nil {
				out = append(out, a)
				continue
			}
			if kind := c.localKind(name); kind != d.Kind {
				notes = append(notes, fmt.Sprintf("account %s: runner %s is %s here and the server defines it as %s — not offered", a.Name, name, kind, d.Kind))
				continue
			}
			if (a.Model != "" && a.Model != d.Model) || (a.Effort != "" && a.Effort != d.Effort) {
				notes = append(notes, fmt.Sprintf("account %s: the server defines runner %s (%s) — the account's own %s is not used",
					a.Name, name, orDefault(runDetail(d.Model, d.Effort)), orDefault(runDetail(a.Model, a.Effort))))
			}
			cp := *a
			cp.Model, cp.Effort, cp.base = d.Model, d.Effort, a
			out = append(out, &cp)
			continue
		}
		fed := false
		for _, d := range c.defs {
			if d.Kind != a.Kind || !modelsMatch(a.Models, d.Model) {
				continue
			}
			cp := *a
			cp.Runner, cp.Model, cp.Effort, cp.base = d.Name, d.Model, d.Effort, a
			out = append(out, &cp)
			fed = true
		}
		if !fed && len(c.defs) > 0 {
			notes = append(notes, fmt.Sprintf("account %s (kind %s) feeds no runner: the server defines none it matches", a.Name, a.Kind))
		}
	}
	c.Accounts = out
	// A defined runner is offered only when what this worker would report
	// agrees with the definition: an endpoint that contradicts the declared
	// provider, or accounts that would send a runner's requests to
	// different places, take the runner out here.
	drop := map[string]string{}
	first := map[string]policy.Facts{}
	for _, a := range c.Accounts {
		name := accountRunner(a)
		d := c.def(name)
		if d == nil {
			continue
		}
		facts, derived := c.accountFacts(a)
		if policy.Contradicts(d.Provider, derived) {
			drop[name] = fmt.Sprintf("the server declares provider %s, and account %s's endpoint %s is %s", d.Provider, a.Name, facts.Endpoint, derived)
			continue
		}
		// The same for a provider this worker's file declares for the
		// runner's kind (runner_config.<kind>), which a defined runner
		// without a declaration of its own reports: the startup check ran
		// before the definitions arrived.
		if policy.Contradicts(facts.Provider, derived) {
			drop[name] = fmt.Sprintf("this worker's file declares provider %s for it, and account %s's endpoint %s is %s", facts.Provider, a.Name, facts.Endpoint, derived)
			continue
		}
		if prev, ok := first[name]; !ok {
			first[name] = facts
		} else if prev.Provider != facts.Provider || prev.Endpoint != facts.Endpoint {
			drop[name] = fmt.Sprintf("its accounts differ in where requests go (%s, and %s on account %s)", factsLine(prev), factsLine(facts), a.Name)
		}
	}
	c.dropRunners(drop, &notes)
	if !c.runnersExplicit {
		c.Runners = c.UsedRunners()
		return notes
	}
	// worker.runners restricts what the worker offers: a defined runner its
	// accounts feed and the list leaves out is not offered, which is easy
	// to miss.
	for _, name := range c.UsedRunners() {
		if c.def(name) != nil && !slices.Contains(c.Runners, name) {
			notes = append(notes, fmt.Sprintf("runner %s is defined on the server and an account here feeds it, but worker.runners does not list it: not offered (add it, or remove worker.runners to offer every runner the accounts feed)", name))
		}
	}
	return notes
}

// dropRunners removes the accounts of runners that must not be offered.
func (c *Config) dropRunners(drop map[string]string, notes *[]string) {
	if len(drop) == 0 {
		return
	}
	c.Accounts = slices.DeleteFunc(c.Accounts, func(a *Account) bool { return drop[accountRunner(a)] != "" })
	names := make([]string, 0, len(drop))
	for name := range drop {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		*notes = append(*notes, fmt.Sprintf("runner %s is not offered: %s", name, drop[name]))
	}
}

// refreshFleet fetches the server's runner definitions and applies them
// when they changed. It reports whether the server answered.
func (w *Worker) refreshFleet(ctx context.Context) bool {
	f, err := w.client.FleetConfig(ctx)
	if err != nil {
		if refused(err) {
			// The server answers and refuses (the key may not read them):
			// waiting changes nothing, and a worker that holds no
			// definitions offers what its own file configures.
			if !w.fleetRefused {
				w.fleetRefused = true
				w.log.Error("runner definitions: the server refuses this worker's key (it needs workspaces read access); the worker offers only the runners its own file configures", "err", err)
			}
			return true
		}
		w.log.Warn("runner definitions could not be fetched; keeping what this worker holds", "err", err)
		return false
	}
	w.fleetRefused = false
	rev := ""
	if f != nil {
		rev = f.Rev
	}
	if _, held := w.cfg.RunnerDefs(); held == rev && w.fleetLoaded {
		return true
	}
	w.fleetLoaded = true
	notes := w.cfg.ApplyRunnerDefs(f)
	// A defined runner needs its CLI to pass the lockdown check here.
	drop := map[string]string{}
	for _, name := range w.cfg.UsedRunners() {
		if w.cfg.def(name) == nil {
			continue
		}
		r, err := w.cfg.RunnerFor(name)
		if err == nil {
			err = w.cfg.CheckRunner(ctx, name, r, "")
		}
		if err != nil {
			drop[name] = err.Error()
		}
	}
	w.cfg.dropRunners(drop, &notes)
	if len(drop) > 0 && !w.cfg.runnersExplicit {
		w.cfg.Runners = w.cfg.UsedRunners()
	}
	for _, n := range notes {
		w.log.Warn("runner definitions: " + n)
	}
	w.resetFacts()
	// A wrapper that changed is checked again before the next claim.
	w.checkWrappers(ctx, true)
	w.client.SetFleetHeld(rev)
	if rev != "" || len(notes) > 0 {
		w.log.Info("runner definitions applied", "rev", rev, "runners", strings.Join(w.cfg.UsedRunners(), ","))
		// Emitted as an audit event on the server when the claim reports
		// the revision (worker.config_applied).
	}
	return true
}

// maybeRefreshFleet refetches while idle when the claim answer names other
// runner definitions than the worker holds.
func (w *Worker) maybeRefreshFleet(ctx context.Context) {
	if _, held := w.cfg.RunnerDefs(); w.client.FleetRev() != held {
		w.refreshFleet(ctx)
	}
}
