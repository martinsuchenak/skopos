package worker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// Workspace config from the server (tmp/central-config — design.md, phase
// 2). A workspace with a config of its own on the server is managed: every
// shared field — branches, setup and check commands, timeouts, push
// settings, the MCP lists — comes from there, and what the local file says
// for those fields is ignored. The file keeps the machine's own facts: the
// clone path, setup_env, the push token's variable, profiles.
//
// A run executes under exactly the revision its claim named
// (run.ConfigRev): the worker fetches it and holds it before the run
// starts. Idle work (preflight, the sweep, indexing) uses the newest
// revision fetched.

// managedWS is what the worker holds for one managed workspace.
type managedWS struct {
	rev  string
	repo *Repo // the local repo with the server's fields laid over it
	// planTimeout, implTimeout, mcpDeny, mcpServers: worker-wide in the
	// file, per workspace on the server.
	planTimeout, implTimeout time.Duration
	mcpDeny, mcpServers      []string
	// local: the command fields this worker keeps from its own file
	// (worker.server_commands is off) although the server sets them.
	local []string
	// commands: the command fields as applied, for the change log.
	commands map[string]string
}

// sharedRepoFields names the repo fields of the file a managed workspace
// ignores, with whether each was set in the file.
func (r *Repo) sharedSet() []string {
	var out []string
	add := func(set bool, name string) {
		if set {
			out = append(out, name)
		}
	}
	add(r.DefaultBranch != "", "default_branch")
	add(r.BranchPrefix != "", "branch_prefix")
	add(r.BranchTemplate != "", "branch_template")
	add(r.Checkout != "", "checkout")
	add(len(r.Protected) > 0, "protected_branches")
	add(len(r.Setup) > 0, "setup")
	add(len(r.Checks) > 0, "checks")
	add(r.CheckFixRounds != nil, "check_fix_rounds")
	add(len(r.Preflight) > 0, "preflight")
	add(r.PreflightInterval != 0, "preflight_interval")
	add(r.SetupTimeout != 0, "setup_timeout")
	add(r.CheckTimeout != 0, "check_timeout")
	add(r.QuietTimeout != 0, "quiet_timeout")
	add(r.Push.Kind != "", "push.kind")
	add(r.Push.RemoteURL != "", "push.remote_url")
	add(r.Push.PullRequest != "", "push.pull_request")
	add(r.Push.Steps != nil, "push.steps")
	return out
}

// ignoredFor lists the fields of the worker's file a managed workspace does
// not use: the repo's shared fields, and the worker-wide ones that are per
// workspace on the server. mcp_deny is not among them: the file's list is
// added to the workspace's.
func (c *Config) ignoredFor(ws string) []string {
	var out []string
	if local := c.Repos[ws]; local != nil {
		out = slices.Clone(local.localSet)
	}
	if !c.ServerCommands {
		out = slices.DeleteFunc(out, wsconfig.IsCommandField)
	}
	return append(out, c.globalSet...)
}

// mergeManaged lays a workspace's server config over its local repo. The
// error says why the result cannot be used on this worker (the workspace
// is then reported unhealthy, with the reason).
func (c *Config) mergeManaged(ws string, eff *wsconfig.Effective) (*managedWS, error) {
	local := c.Repos[ws]
	if local == nil {
		return nil, fmt.Errorf("workspace %s has no repo in this worker's file", ws)
	}
	set := eff.Config
	// Command fields run on this host: without worker.server_commands they
	// stay the file's, whatever the server says.
	m := &managedWS{rev: eff.Rev, commands: map[string]string{}}
	if !c.ServerCommands {
		fields := set.Fields()
		for _, f := range wsconfig.CommandFields {
			if _, ok := fields[f]; ok {
				m.local = append(m.local, f)
			}
		}
		// Copies that are never nil: a list the file does not set is set to
		// nothing here, not left for the default.
		setup, checks, preflight := append([]string{}, local.Setup...), append([]string{}, local.Checks...), append([]string{}, local.Preflight...)
		set.Setup, set.Checks, set.Preflight = &setup, &checks, &preflight
	}
	full := wsconfig.Complete(set)
	r := *local
	r.DefaultBranch, r.BranchPrefix, r.BranchTemplate = *full.DefaultBranch, *full.BranchPrefix, *full.BranchTemplate
	r.Checkout = *full.Checkout
	r.Protected = slices.Clone(*full.ProtectedBranches)
	r.Setup, r.Checks, r.Preflight = slices.Clone(*full.Setup), slices.Clone(*full.Checks), slices.Clone(*full.Preflight)
	rounds := *full.CheckFixRounds
	r.CheckFixRounds = &rounds
	r.PreflightInterval = full.PreflightInterval.D()
	r.SetupTimeout, r.CheckTimeout, r.QuietTimeout = full.SetupTimeout.D(), full.CheckTimeout.D(), full.QuietTimeout.D()
	r.Push.Kind, r.Push.RemoteURL, r.Push.PullRequest = *full.Push.Kind, *full.Push.RemoteURL, *full.Push.PullRequest
	steps := *full.Push.Steps
	r.Push.Steps = &steps
	m.repo = &r
	m.planTimeout, m.implTimeout = full.PlanTimeout.D(), full.ImplTimeout.D()
	// The file's own deny list stays in force on top of the workspace's:
	// it is this host's operator saying what no run here may call, and a
	// deny list only ever narrows.
	m.mcpDeny, m.mcpServers = slices.Clone(*full.MCPDeny), slices.Clone(*full.MCPServers)
	for _, d := range c.mcpDenyFile {
		if !slices.Contains(m.mcpDeny, d) {
			m.mcpDeny = append(m.mcpDeny, d)
		}
	}
	m.commands["setup"], m.commands["checks"], m.commands["preflight"] = strings.Join(r.Setup, "\n"), strings.Join(r.Checks, "\n"), strings.Join(r.Preflight, "\n")

	// What the server cannot know about this host.
	if r.Checkout == checkoutBranch && !branchCheckoutSupported {
		return m, errors.New(`the server's config sets checkout = "branch", which needs a unix host`)
	}
	if r.Push.Kind != "none" {
		if r.Push.TokenEnv == "" {
			return m, fmt.Errorf("the server's config pushes (push.kind %s) and this worker's file names no push.token_env for the repo", r.Push.Kind)
		}
		if r.Push.RemoteURL == "" {
			return m, fmt.Errorf("the server's config sets push.kind %s without push.remote_url", r.Push.Kind)
		}
	}
	if r.Push.PullRequest != "none" {
		if _, err := newPRClient(r.Push.Kind, r.Push.RemoteURL, ""); err != nil {
			return m, fmt.Errorf("the server's config sets push.pull_request: %v", err)
		}
	}
	return m, nil
}

// repo is the workspace's repo as runs and idle work see it: the server's
// config over the local file for a managed workspace, the file's otherwise.
func (w *Worker) repo(ws string) *Repo {
	w.cfgMu.RLock()
	defer w.cfgMu.RUnlock()
	if m := w.managed[ws]; m != nil && m.repo != nil {
		return m.repo
	}
	return w.cfg.Repos[ws]
}

func (w *Worker) managedFor(ws string) *managedWS {
	w.cfgMu.RLock()
	defer w.cfgMu.RUnlock()
	return w.managed[ws]
}

// planTimeout / implTimeout / mcpDeny: per workspace when it is managed,
// the worker's own otherwise.
func (w *Worker) planTimeout(ws string) time.Duration {
	if m := w.managedFor(ws); m != nil {
		return m.planTimeout
	}
	return w.cfg.PlanTimeout
}

func (w *Worker) implTimeout(ws string) time.Duration {
	if m := w.managedFor(ws); m != nil {
		return m.implTimeout
	}
	return w.cfg.ImplTimeout
}

func (w *Worker) mcpDeny(ws string) []string {
	if m := w.managedFor(ws); m != nil {
		return m.mcpDeny
	}
	return w.cfg.MCPDeny
}

// planToolsFor / implToolsFor: the stage's tools with the MCP server
// prefixes the workspace's runs may use.
func (w *Worker) planToolsFor(ws string) []string {
	if m := w.managedFor(ws); m != nil {
		return append(slices.Clone(basePlanTools), m.mcpServers...)
	}
	return planTools
}

func (w *Worker) implToolsFor(ws string) []string {
	if m := w.managedFor(ws); m != nil {
		return append(slices.Clone(baseImplTools), m.mcpServers...)
	}
	return implTools
}

// configProblem is why a managed workspace's config cannot be used on this
// worker ("" = it can): reported as unhealthy with every claim, so the
// server hands this worker none of its runs.
func (w *Worker) configProblem(ws string) string {
	w.cfgMu.RLock()
	defer w.cfgMu.RUnlock()
	if why := w.configProblems[ws]; why != "" {
		return why
	}
	if w.managed[ws] == nil {
		if why := w.cfg.Repos[ws].pushProblem(); why != "" {
			return "config: " + why
		}
	}
	return ""
}

// pushProblem: a push block that names only the token's variable is
// complete only when the server holds the rest. For a workspace the server
// does not manage, the worker would never push.
func (r *Repo) pushProblem() string {
	if r == nil || !r.tokenOnly {
		return ""
	}
	return "push.token_env is set without push.kind, and the server does not manage this workspace: set push.kind and push.remote_url in this worker's file, or give the workspace a config on the server"
}

// applyConfig makes eff the workspace's config on this worker (nil or not
// managed: back to the file) and logs what changed.
func (w *Worker) applyConfig(ws string, eff *wsconfig.Effective) {
	w.cfgMu.Lock()
	defer w.cfgMu.Unlock()
	if w.managed == nil {
		w.managed, w.configProblems = map[string]*managedWS{}, map[string]string{}
	}
	prev := w.managed[ws]
	if eff == nil || !eff.Managed {
		if prev != nil {
			w.log.Info("config: the workspace is no longer managed by the server; using this worker's file", "workspace", ws)
		}
		delete(w.managed, ws)
		delete(w.configProblems, ws)
		return
	}
	if prev != nil && prev.rev == eff.Rev {
		return
	}
	m, err := w.cfg.mergeManaged(ws, eff)
	if err != nil {
		if w.configProblems[ws] != err.Error() {
			w.log.Error("config: the server's config for the workspace cannot be used here; not taking its work", "workspace", ws, "rev", eff.Rev, "err", err)
		}
		w.configProblems[ws] = "config: " + err.Error()
		if m == nil {
			return
		}
	} else {
		delete(w.configProblems, ws)
	}
	if prev == nil {
		w.log.Info("config: the workspace is managed by the server", "workspace", ws, "rev", eff.Rev, "fields", strings.Join(eff.Config.FieldNames(), ","))
		if ignored := w.cfg.ignoredFor(ws); len(ignored) > 0 {
			w.log.Warn("config: these fields of this worker's file are ignored for the workspace (the server's config applies)", "workspace", ws, "fields", strings.Join(ignored, ","))
		}
	} else {
		w.log.Info("config: applied a new revision", "workspace", ws, "rev", eff.Rev, "was", prev.rev)
	}
	if len(m.local) > 0 && (prev == nil || !slices.Equal(prev.local, m.local)) {
		w.log.Warn("config: the server sets commands for the workspace, and this worker keeps its own (set worker.server_commands = true to take them)", "workspace", ws, "fields", strings.Join(m.local, ","))
	}
	// Every command change this worker applies is logged: the commands run
	// on this host.
	if w.cfg.ServerCommands {
		for _, f := range wsconfig.CommandFields {
			was := ""
			if prev != nil {
				was = prev.commands[f]
			} else if local := w.cfg.Repos[ws]; local != nil {
				was = strings.Join(map[string][]string{"setup": local.Setup, "checks": local.Checks, "preflight": local.Preflight}[f], "\n")
			}
			if now := m.commands[f]; now != was {
				w.log.Warn("config: commands from the server changed", "workspace", ws, "field", f, "rev", eff.Rev, "commands", strings.ReplaceAll(now, "\n", " ; "))
			}
		}
	}
	w.managed[ws] = m
	// A preflight that changed runs again at once.
	delete(w.health, ws)
}

// refreshConfig fetches what applies to each of the worker's workspaces
// now. It reports whether every workspace answered: a server that cannot
// be reached leaves what the worker holds.
func (w *Worker) refreshConfig(ctx context.Context) bool {
	ok, _ := w.refreshConfigWhy(ctx)
	return ok
}

// refused reports an answer the server will keep giving (a 4xx: the key may
// not read the config), as opposed to one worth waiting out.
func refused(err error) bool { return err != nil && reClientError.MatchString(err.Error()) }

var reClientError = regexp.MustCompile(`: 4\d\d `)

// refreshConfigWhy is refreshConfig that also says whether a failure is one
// the server will keep giving.
func (w *Worker) refreshConfigWhy(ctx context.Context) (ok, final bool) {
	ok, final = true, true
	ids := slices.Clone(w.cfg.Workspaces)
	sort.Strings(ids)
	for _, ws := range ids {
		eff, err := w.client.EffectiveConfig(ctx, ws)
		if err != nil {
			w.log.Warn("config: the workspace's config could not be fetched; keeping what this worker holds", "workspace", ws, "err", err)
			ok = false
			if !refused(err) {
				final = false
			}
			continue
		}
		w.applyConfig(ws, eff)
	}
	return ok, final && !ok
}

// loadServerConfig fetches the workspaces' config before the first claim,
// waiting for a server that cannot be reached: without it the worker could
// claim nothing anyway.
func (w *Worker) loadServerConfig(ctx context.Context) {
	for attempt := 0; ; attempt++ {
		// The runner definitions first: they decide what the worker offers.
		if w.refreshFleet(ctx) {
			ok, final := w.refreshConfigWhy(ctx)
			if ok {
				return
			}
			if final {
				// The server answers and refuses: waiting changes nothing.
				// Runs are still safe (each fetches the revision its claim
				// names or is given back); idle work uses this file.
				w.log.Error("config: the server refuses this worker's key the workspaces' config (it needs workspaces read access); idle work uses this worker's file")
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		if attempt == 0 {
			w.log.Warn("config: waiting for the server before taking work")
		}
		wait := w.cfg.PollEvery
		if wait <= 0 || wait > 30*time.Second {
			wait = 30 * time.Second
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// maybeRefreshConfig refetches while idle when the claim answer says what
// the worker holds is out of date.
func (w *Worker) maybeRefreshConfig(ctx context.Context) {
	digest := w.client.ConfigDigest()
	if digest == w.configDigest && !w.configStale {
		return
	}
	if w.refreshConfig(ctx) {
		w.configDigest, w.configStale = digest, false
	}
}

// holdConfig makes the config the run's claim named the workspace's config
// for the run: "" is a workspace the server does not manage (the file
// applies), anything else is fetched unless already held. The run is not
// started under any other revision.
func (w *Worker) holdConfig(ctx context.Context, run *Run) error {
	if run.ConfigRev == "" {
		if w.managedFor(run.WorkspaceID) != nil {
			w.configStale = true
			w.applyConfig(run.WorkspaceID, nil)
		}
		return nil
	}
	if m := w.managedFor(run.WorkspaceID); m == nil || m.rev != run.ConfigRev {
		// What the run executes under may not be the newest (a run claimed
		// before a change): idle work refetches after it.
		w.configStale = true
		eff, err := w.client.ConfigAt(ctx, run.WorkspaceID, run.ConfigRev)
		if err != nil {
			return fmt.Errorf("the run's config revision %s could not be fetched: %w", run.ConfigRev, err)
		}
		if eff == nil || !eff.Managed || eff.Rev != run.ConfigRev {
			return fmt.Errorf("the server has no config revision %s for %s", run.ConfigRev, run.WorkspaceID)
		}
		w.applyConfig(run.WorkspaceID, eff)
	}
	if why := w.configProblem(run.WorkspaceID); why != "" {
		return errors.New(strings.TrimPrefix(why, "config: "))
	}
	if m := w.managedFor(run.WorkspaceID); m == nil || m.rev != run.ConfigRev {
		return fmt.Errorf("config revision %s was not applied", run.ConfigRev)
	}
	return nil
}

// CheckServerConfig asks the server what it holds for this worker — the
// runner definitions and each workspace's config — and reports what would
// stop the result from working here (problems) and what an operator should
// know about it (notes). It changes cfg the way a starting worker would. A
// server that cannot be reached is a note: it is not the config's fault.
func CheckServerConfig(ctx context.Context, cfg *Config, client *Client) (problems, notes []string) {
	f, err := client.FleetConfig(ctx)
	if err != nil {
		return nil, []string{"the server could not be asked for its runner definitions and workspace config: " + err.Error()}
	}
	notes = append(notes, cfg.ApplyRunnerDefs(f)...)
	if f != nil && len(f.Runners) > 0 {
		notes = append(notes, fmt.Sprintf("the server defines %d runner(s) (revision %s); this worker offers: %s", len(f.Runners), f.Rev, strings.Join(cfg.UsedRunners(), ", ")))
	}
	ids := slices.Clone(cfg.Workspaces)
	sort.Strings(ids)
	for _, ws := range ids {
		eff, err := client.EffectiveConfig(ctx, ws)
		if err != nil {
			notes = append(notes, fmt.Sprintf("workspace %s: its config on the server could not be read: %v", ws, err))
			continue
		}
		if eff == nil || !eff.Managed {
			if why := cfg.Repos[ws].pushProblem(); why != "" {
				problems = append(problems, fmt.Sprintf("workspace %s: %s", ws, why))
			}
			continue
		}
		m, err := cfg.mergeManaged(ws, eff)
		if err != nil {
			problems = append(problems, fmt.Sprintf("workspace %s: %v", ws, err))
			continue
		}
		line := fmt.Sprintf("workspace %s is managed by the server (revision %s)", ws, eff.Rev)
		if len(m.local) > 0 {
			line += "; " + strings.Join(m.local, ", ") + " stay this file's (worker.server_commands is off)"
		}
		if ignored := cfg.ignoredFor(ws); len(ignored) > 0 {
			line += "; ignored in this file: " + strings.Join(ignored, ", ")
		}
		notes = append(notes, line)
	}
	return problems, notes
}
