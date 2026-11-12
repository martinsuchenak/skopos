package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/policy"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// serverStages makes blocks the workspace's stage blocks the way an applied
// config does.
func serverStages(t *testing.T, c *Config, ws string, blocks map[string]*wsconfig.StageCaps) *managedWS {
	t.Helper()
	m, err := c.mergeManaged(ws, &wsconfig.Effective{Workspace: ws, Managed: true, Rev: "r1", Config: wsconfig.Config{Stages: blocks}})
	if err != nil {
		t.Fatal(err)
	}
	c.setStages(ws, m.stages)
	return m
}

// What a stage's agent is given comes from the server; what contains the
// run stays the worker's file's — its wrapper and its sandbox block.
func TestServerStagesReplaceGrantsAndKeepContainment(t *testing.T) {
	c := executorConfig(t)
	c.ServerCommands = true
	c.Vars = map[string]string{"ROOT": "/srv"}
	wrapperBefore, sandboxBefore := c.ExecutorFor("ws", "implement", "claude"), c.ExecutorFor("ws", "review", "claude")
	if got := strings.Join(c.fileGrants("ws"), ","); got != "stages.implement,stages.review" {
		t.Fatalf("what the file gives: %s", got)
	}
	m := serverStages(t, c, "ws", map[string]*wsconfig.StageCaps{
		"implement": {Tools: []string{"Bash(npm run:*)"}, Env: map[string]string{"CACHE": "${ROOT}/cache"}},
	})
	impl := c.StageProfile("ws", "implement")
	if !slices.Equal(impl.Tools, []string{"Bash(npm run:*)"}) || impl.Env["CACHE"] != "/srv/cache" {
		t.Fatalf("implement gets the server's block: %+v", impl)
	}
	if !slices.Equal(impl.Wrapper, []string{"bwrap", "--bind", "{dir}", "{dir}"}) || c.ExecutorFor("ws", "implement", "claude") != wrapperBefore {
		t.Fatalf("the file's wrapper stays: %v, %s (was %s)", impl.Wrapper, c.ExecutorFor("ws", "implement", "claude"), wrapperBefore)
	}
	// Review has no block on the server: the stage is given nothing, and
	// the file's sandbox still contains it.
	review := c.StageProfile("ws", "review")
	if review == nil || len(review.Tools) != 0 || review.grantsAllow() || !review.sandboxed() {
		t.Fatalf("review keeps only the sandbox: %+v", review)
	}
	if got := c.ExecutorFor("ws", "review", "claude"); got != sandboxBefore {
		t.Fatalf("the sandbox executor changed: %s (was %s)", got, sandboxBefore)
	}
	if ignored := strings.Join(c.ignoredFor("ws"), ","); !strings.Contains(ignored, "stages.implement") || !strings.Contains(ignored, "stages.review") {
		t.Fatalf("the file's grants are named as ignored: %s", ignored)
	}
	if !strings.Contains(m.commands["stages.implement"], "Bash(npm run:*)") {
		t.Fatalf("the block is in the change log: %v", m.commands)
	}
	// Back to the file once the server no longer holds the stages.
	c.setStages("ws", nil)
	if p := c.StageProfile("ws", "implement"); !slices.Equal(p.Tools, []string{"Bash(php:*)"}) {
		t.Fatalf("the file's profile applies again: %+v", p)
	}
}

// Without worker.server_commands a stage block from the server is not
// used: the file's profiles apply, and the run log is told.
func TestServerCommandsOffKeepsFileProfiles(t *testing.T) {
	c := executorConfig(t)
	m := serverStages(t, c, "ws", map[string]*wsconfig.StageCaps{"implement": {Tools: []string{"Bash(npm run:*)"}}})
	if m.stages != nil || c.stagesFromServer("ws") {
		t.Fatal("the server's blocks were taken without server_commands")
	}
	if !slices.Contains(m.local, "stages.implement") {
		t.Fatalf("the kept fields: %v", m.local)
	}
	if p := c.StageProfile("ws", "implement"); !slices.Equal(p.Tools, []string{"Bash(php:*)"}) {
		t.Fatalf("the file's profile: %+v", p)
	}
	if ignored := strings.Join(c.ignoredFor("ws"), ","); strings.Contains(ignored, "stages.") {
		t.Fatalf("nothing of the profile is ignored: %s", ignored)
	}
}

// A block that gives the stage something takes effect only where
// something contains the run. Elsewhere the worker does not offer the
// stage, and says why.
func TestServerGrantsNeedContainment(t *testing.T) {
	was := codexSandboxVerified
	codexSandboxVerified = true
	t.Cleanup(func() { codexSandboxVerified = was })
	c := executorConfig(t)
	c.ServerCommands = true
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{"plan": {Tools: []string{"Bash(make:*)"}}, "qa": {}})
	for _, r := range []string{"claude", "opencode-glm", "codex"} {
		if got := c.ExecutorFor("bare", "plan", r); got != policy.NoExecutor(policy.NoneUncontained) {
			t.Errorf("%s: a granted, uncontained stage must not be offered: %s", r, got)
		}
	}
	// Stages that are given nothing run as before.
	if got := c.ExecutorFor("bare", "qa", "claude"); got != policy.ExecutorLocal {
		t.Fatalf("an empty block: %s", got)
	}
	if got := c.ExecutorFor("bare", "implement", "claude"); got != policy.ExecutorLocal {
		t.Fatalf("no block: %s", got)
	}
	var problems []string
	for _, p := range c.ExecutorProblems() {
		if strings.HasPrefix(p, "bare:") {
			problems = append(problems, p)
		}
	}
	if len(problems) != 3 || !strings.Contains(problems[0], "the server's config gives the plan stage") || !strings.Contains(problems[0], "contained_by") {
		t.Fatalf("problems: %v", problems)
	}
	// The worker refuses such a run itself, whatever the claim said.
	w := &Worker{cfg: c}
	if why := w.refuseExecutor(&Run{WorkspaceID: "bare", Stage: "plan", Runner: "claude"}); !strings.Contains(why, "cannot run plan on claude") {
		t.Fatalf("refusal: %q", why)
	}
	// A host that is a container contains every stage.
	c.ContainedBy = "knot"
	if got := c.ExecutorFor("bare", "plan", "claude"); got != "host:knot" {
		t.Fatalf("contained host: %s", got)
	}
	if got := c.ExecutorProblems(); len(got) != 0 {
		t.Fatalf("problems on a contained host: %v", got)
	}
	// The same rule for a profile of the worker's own file: its review
	// stage gives tools and only its sandbox settings contain it, which
	// opencode does not honour.
	c.ContainedBy = ""
	if got := strings.Join(c.ExecutorProblems(), "\n"); !strings.Contains(got, "ws: this worker's file gives the review stage") {
		t.Fatalf("the file's own uncontained grant: %s", got)
	}
	// What the worker reports resolves to what a run records, the cases it
	// cannot run included.
	c.ContainedBy = ""
	facts := c.ExecutorFacts()
	for _, ws := range c.Workspaces {
		for _, stage := range executorStages {
			for _, r := range c.Runners {
				if got, want := policy.ResolveExecutor(facts[ws], stage, r), c.ExecutorFor(ws, stage, r); got != want {
					t.Errorf("%s %s %s: facts resolve to %q, a run records %q", ws, stage, r, got, want)
				}
			}
		}
	}
}

// Wrap rules pick a server-defined wrapper by the runner's facts, first
// match first. The executor is named after the wrapper, with the hash of
// its argv as defined. A wrapper of the worker's own file is never
// replaced.
func TestWrapRulesAttachServerWrappers(t *testing.T) {
	c := executorConfig(t)
	c.ServerCommands = true
	c.Vars = map[string]string{"TOOLS": "/opt/tools"}
	box := wsconfig.WrapperDef{Name: "opencode-bwrap", Argv: []string{"bwrap", "--bind", "{dir}", "{dir}", "--ro-bind", "${TOOLS}", "/tools"}}
	c.setWrappers([]wsconfig.WrapperDef{box, {Name: "needs-var", Argv: []string{"bwrap", "${NOT_DECLARED}"}}})
	glm := c.runnerFact("opencode-glm")
	if glm.Kind != "opencode" || glm.Provider == "" {
		t.Fatalf("facts: %+v", glm)
	}
	rules := []wsconfig.WrapRule{
		{Kind: "claude", Wrapper: wsconfig.NoWrapper},
		{Kind: "opencode", Provider: glm.Provider, Wrapper: "opencode-bwrap"},
		{Wrapper: "not-defined"},
	}
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{"review": {Wrap: rules}})
	if got := c.ExecutorFor("bare", "review", "opencode-glm"); got != box.Executor() {
		t.Fatalf("the wrapped runner: %s, want %s", got, box.Executor())
	}
	prof, problem := c.stageProfileFor("bare", "review", "opencode-glm")
	if problem != "" || !slices.Equal(prof.Wrapper, []string{"bwrap", "--bind", "{dir}", "{dir}", "--ro-bind", "/opt/tools", "/tools"}) {
		t.Fatalf("the wrapper on this host: %v (%s)", prof.Wrapper, problem)
	}
	// A second opinion and a branch review run as a review.
	for _, stage := range []string{"second_opinion", "branch_review"} {
		if got := c.ExecutorFor("bare", stage, "opencode-glm"); got != box.Executor() {
			t.Errorf("%s: %s", stage, got)
		}
	}
	// The exemption: claude runs unwrapped.
	if got := c.ExecutorFor("bare", "review", "claude"); got != policy.ExecutorLocal {
		t.Fatalf("claude: %s", got)
	}
	// What the worker reports resolves to the same, runner by runner.
	facts := c.ExecutorFacts()
	for _, ws := range c.Workspaces {
		for _, stage := range executorStages {
			for _, r := range c.Runners {
				if got, want := policy.ResolveExecutor(facts[ws], stage, r), c.ExecutorFor(ws, stage, r); got != want {
					t.Errorf("%s %s %s: facts resolve to %q, a run records %q", ws, stage, r, got, want)
				}
			}
		}
	}
	// A rule that names a wrapper the server does not define stops the run.
	if got := c.ExecutorFor("bare", "review", "codex"); got != policy.NoExecutor(policy.NoneWrapper) {
		t.Fatalf("an undefined wrapper: %s", got)
	}
	if _, problem := c.stageProfileFor("bare", "review", "codex"); !strings.Contains(problem, "not-defined") {
		t.Fatalf("problem: %q", problem)
	}
	// So does a wrapper that needs a variable this worker does not declare.
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{"review": {Wrap: []wsconfig.WrapRule{{Wrapper: "needs-var"}}}})
	if got := c.ExecutorFor("bare", "review", "claude"); got != policy.NoExecutor(policy.NoneWrapper) {
		t.Fatalf("a missing variable: %s", got)
	}
	if _, problem := c.stageProfileFor("bare", "review", "claude"); !strings.Contains(problem, "NOT_DECLARED") || !strings.Contains(problem, "worker.vars") {
		t.Fatalf("problem: %q", problem)
	}
	// The file's own wrapper wins over a rule from the server.
	fileWrapper := c.ExecutorFor("ws", "implement", "opencode-glm")
	serverStages(t, c, "ws", map[string]*wsconfig.StageCaps{"implement": {Wrap: []wsconfig.WrapRule{{Wrapper: "opencode-bwrap"}}}})
	if got := c.ExecutorFor("ws", "implement", "opencode-glm"); got != fileWrapper || !strings.HasPrefix(got, "wrapper:php:") {
		t.Fatalf("the file's wrapper was replaced: %s", got)
	}
	// A file wrapper that only sets variables is not a wrapper: the
	// server's applies.
	c.Repos["bare"].Stages = map[string]*StageProfile{"qa": {Wrapper: []string{"env", "SKOPOS_CONTAINED_BY=knot"}}}
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{"qa": {Wrap: []wsconfig.WrapRule{{Wrapper: "opencode-bwrap"}}}})
	if got := c.ExecutorFor("bare", "qa", "claude"); got != box.Executor() {
		t.Fatalf("an env-only file wrapper kept the server's out: %s", got)
	}
}

// A wrapper's check runs on the worker; where it fails, the worker does not
// offer what the wrapper is for. It runs again when the definition changes
// and when it is old, not on every claim.
func TestWrapperCheckDecidesWhetherItIsOffered(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	c := executorConfig(t)
	c.ServerCommands = true
	failing := wsconfig.WrapperDef{Name: "box", Argv: []string{"bwrap", "{dir}"}, Check: []string{"sh", "-c", "echo x >> " + count + "; echo no namespaces >&2; exit 3"}}
	c.setWrappers([]wsconfig.WrapperDef{failing, {Name: "unused", Argv: []string{"x"}, Check: []string{"sh", "-c", "echo x >> " + count + ".unused"}}})
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{"review": {Wrap: []wsconfig.WrapRule{{Kind: "opencode", Wrapper: "box"}}}})
	// Not checked yet: offered (the check has to say otherwise).
	if got := c.ExecutorFor("bare", "review", "opencode-glm"); got != failing.Executor() {
		t.Fatalf("before the check: %s", got)
	}
	checked := map[string]time.Time{}
	changed, notes := c.CheckWrappers(context.Background(), checked, false, os.Environ())
	if !changed || len(notes) != 1 || !strings.Contains(notes[0], "no namespaces") {
		t.Fatalf("a failing check: %v %v", changed, notes)
	}
	if got := c.ExecutorFor("bare", "review", "opencode-glm"); got != policy.NoExecutor(policy.NoneWrapper) {
		t.Fatalf("a wrapper that does not start is still offered: %s", got)
	}
	if got := c.ExecutorFor("bare", "review", "claude"); got != policy.ExecutorLocal {
		t.Fatalf("a runner the rule does not match: %s", got)
	}
	// A check that fails again with other words changes nothing the worker
	// reports.
	if changed, notes := c.CheckWrappers(context.Background(), checked, true, os.Environ()); changed || len(notes) != 0 {
		t.Fatalf("a check that still fails: %v %v", changed, notes)
	}
	// Not run again right away, and never for a wrapper no block names.
	if changed, _ := c.CheckWrappers(context.Background(), checked, false, os.Environ()); changed {
		t.Fatal("the check ran again and changed its mind")
	}
	if b, _ := os.ReadFile(count); strings.Count(string(b), "x") != 2 {
		t.Fatalf("the check ran %d times", strings.Count(string(b), "x"))
	}
	if _, err := os.Stat(count + ".unused"); err == nil {
		t.Fatal("a wrapper nothing names was checked")
	}
	// The same definition keeps its result across a refetch.
	c.setWrappers([]wsconfig.WrapperDef{failing})
	if got := c.ExecutorFor("bare", "review", "opencode-glm"); got != policy.NoExecutor(policy.NoneWrapper) {
		t.Fatalf("a refetch forgot the failed check: %s", got)
	}
	// A changed definition is checked at once.
	passing := failing
	passing.Check = []string{"true"}
	c.setWrappers([]wsconfig.WrapperDef{passing})
	changed, notes = c.CheckWrappers(context.Background(), checked, false, os.Environ())
	if !changed || len(notes) != 1 || !strings.Contains(notes[0], "starts on this worker again") {
		t.Fatalf("a passing check: %v %v", changed, notes)
	}
	if got := c.ExecutorFor("bare", "review", "opencode-glm"); got != passing.Executor() {
		t.Fatalf("after the check passes: %s", got)
	}
}

// ${NAME} in what the server supplies is filled in from worker.vars only:
// the worker's environment holds its credentials.
func TestVarsNeverComeFromTheEnvironment(t *testing.T) {
	t.Setenv("SKOPOS_TEST_SECRET", "the-worker-key")
	c := executorConfig(t)
	c.ServerCommands = true
	c.ContainedBy = "knot"
	_, err := c.mergeManaged("bare", &wsconfig.Effective{Workspace: "bare", Managed: true, Rev: "r1", Config: wsconfig.Config{
		Stages: map[string]*wsconfig.StageCaps{"plan": {Env: map[string]string{"LEAK": "${SKOPOS_TEST_SECRET}"}}}}})
	if err == nil || !strings.Contains(err.Error(), "SKOPOS_TEST_SECRET") || !strings.Contains(err.Error(), "worker.vars") {
		t.Fatalf("an undeclared variable was filled in: %v", err)
	}
	// What the server may not set is refused here too, whatever it saved.
	_, err = c.mergeManaged("bare", &wsconfig.Effective{Workspace: "bare", Managed: true, Rev: "r2", Config: wsconfig.Config{
		Stages: map[string]*wsconfig.StageCaps{"plan": {Env: map[string]string{"ANTHROPIC_BASE_URL": "http://elsewhere"}}}}})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_BASE_URL") {
		t.Fatalf("an endpoint variable from the server was accepted: %v", err)
	}
	c.setWrappers([]wsconfig.WrapperDef{{Name: "trick", Argv: []string{"env", "SKOPOS_CONTAINED_BY=knot"}}})
	if c.wrapperInvalid["trick"] == "" {
		t.Fatal("a wrapper that only sets variables was accepted from the server")
	}
}

// A definition the worker refuses stays refused whatever its check says: a
// passing check clears a failed check, never a refusal. (A server that
// sends what it should have refused itself — a wrapper that only sets
// variables would declare the host a container — gains nothing.)
func TestRefusedWrapperDefinitionStaysRefused(t *testing.T) {
	c := executorConfig(t)
	c.ServerCommands = true
	c.setWrappers([]wsconfig.WrapperDef{
		{Name: "trick", Argv: []string{"env", "SKOPOS_CONTAINED_BY=knot"}, Check: []string{"true"}},
		{Name: "newer", Argv: []string{"bwrap", "--bind", "{future}", "{dir}"}, Check: []string{"true"}},
		{Name: "novar", Argv: []string{"bwrap", "${NOPE}"}, Check: []string{"true"}},
	})
	for i, name := range []string{"trick", "newer", "novar"} {
		stage := []string{"plan", "review", "qa"}[i]
		serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{stage: {Tools: []string{"Bash(x:*)"}, Wrap: []wsconfig.WrapRule{{Wrapper: name}}}})
		for range 2 {
			changed, notes := c.CheckWrappers(context.Background(), map[string]time.Time{}, true, os.Environ())
			if changed || len(notes) != 0 {
				t.Fatalf("%s: a check ran for a refused definition: %v %v", name, changed, notes)
			}
			if got := c.ExecutorFor("bare", stage, "claude"); got != policy.NoExecutor(policy.NoneWrapper) {
				t.Fatalf("%s: a refused definition became usable: %s", name, got)
			}
		}
		if _, problem := c.stageProfileFor("bare", stage, "claude"); !strings.Contains(problem, "does not accept") {
			t.Fatalf("%s: problem %q", name, problem)
		}
		if _, held := c.wrappers[name]; held {
			t.Fatalf("%s: a refused definition is kept", name)
		}
	}
}

// What the worker's file says about containment survives a managed
// workspace whole: its sandbox block replaces the server's, and what its
// settings deny stays denied, next to what the server allows.
func TestFileSandboxAndDenyRulesStay(t *testing.T) {
	c := executorConfig(t)
	c.ServerCommands = true
	c.Repos["bare"].Stages = map[string]*StageProfile{"review": {
		Settings: `{"sandbox":{"enabled":true},"permissions":{"deny":["Read(/secrets/**)"],"allow":["Bash(file:*)"]}}`, Tools: []string{"Bash(file:*)"}}}
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{"review": {
		Settings: `{"permissions":{"allow":["Edit(//**)"],"deny":["Bash(rm:*)"],"additionalDirectories":["/opt"]},"sandbox":{"enabled":false}}`}})
	p := c.StageProfile("bare", "review")
	var got struct {
		Sandbox     struct{ Enabled bool }
		Permissions struct{ Allow, Deny, AdditionalDirectories []string }
	}
	if err := json.Unmarshal([]byte(p.Settings), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Sandbox.Enabled {
		t.Fatalf("the server turned the file's sandbox off: %s", p.Settings)
	}
	if !slices.Equal(got.Permissions.Deny, []string{"Bash(rm:*)", "Read(/secrets/**)"}) {
		t.Fatalf("the file's deny rules must stay in force: %s", p.Settings)
	}
	if !slices.Equal(got.Permissions.Allow, []string{"Edit(//**)"}) || len(p.Tools) != 0 {
		t.Fatalf("what the stage is given is the server's alone: %s %v", p.Settings, p.Tools)
	}
	// The file's deny rules are not named as ignored; its allow rule is.
	if got := strings.Join(c.fileGrants("bare"), ","); got != "stages.review" {
		t.Fatalf("ignored: %s", got)
	}
	c.Repos["bare"].Stages["review"] = &StageProfile{Settings: `{"sandbox":{"enabled":true},"permissions":{"deny":["Read(/secrets/**)"]}}`}
	if got := c.fileGrants("bare"); len(got) != 0 {
		t.Fatalf("a profile that only contains and denies loses nothing: %v", got)
	}
	// With no stage block at all the deny rules are still there.
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{})
	if p := c.StageProfile("bare", "review"); p == nil || !strings.Contains(p.Settings, "Read(/secrets/**)") || !p.sandboxed() {
		t.Fatalf("no block from the server: %+v", p)
	}
}

// Stage blocks this worker cannot use stop the workspace here: the file's
// profiles do not take their place, the worker reports that it cannot run
// there, and a revision it can use puts things right.
func TestUnusableStageBlocksStopTheWorkspace(t *testing.T) {
	c := executorConfig(t)
	c.ServerCommands = true
	c.ContainedBy = "knot"
	box := wsconfig.WrapperDef{Name: "box", Argv: []string{"bwrap", "{dir}"}}
	good := &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{
		Stages: map[string]*wsconfig.StageCaps{"review": {Wrap: []wsconfig.WrapRule{{Wrapper: "box"}}}}}}
	bad := &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r2", Config: wsconfig.Config{
		Stages: map[string]*wsconfig.StageCaps{"review": {Env: map[string]string{"X": "${NOPE}"}, Wrap: []wsconfig.WrapRule{{Wrapper: "box"}}}}}}
	s := &configServer{effective: map[string]*wsconfig.Effective{"ws": good}, revs: map[string]*wsconfig.Effective{"r1": good, "r2": bad},
		fleet: &wsconfig.Fleet{Rev: "f1", Wrappers: []wsconfig.WrapperDef{box}}}
	w, _ := managedWorker(t, c, s)
	if !w.refreshFleet(context.Background()) || !w.refreshConfig(context.Background()) {
		t.Fatal("fetch")
	}
	if got := c.ExecutorFor("ws", "review", "claude"); got != box.Executor() {
		t.Fatalf("the good revision: %s", got)
	}
	w.applyConfig("ws", bad)
	for _, stage := range []string{"review", "implement", "plan"} {
		if got := c.ExecutorFor("ws", stage, "claude"); got != policy.NoExecutor(policy.NoneConfig) {
			t.Fatalf("%s under blocks the worker cannot use: %s", stage, got)
		}
	}
	// Not the file's profile: its implement stage has a wrapper and tools.
	if p, problem := c.stageProfileFor("ws", "implement", "claude"); p != nil || !strings.Contains(problem, "NOPE") {
		t.Fatalf("the file's profile stepped in: %+v %q", p, problem)
	}
	if why := w.configProblem("ws"); !strings.Contains(why, "NOPE") {
		t.Fatalf("the workspace is reported unhealthy: %q", why)
	}
	if why := w.refuseExecutor(&Run{WorkspaceID: "ws", Stage: "review", Runner: "claude"}); !strings.Contains(why, "NOPE") {
		t.Fatalf("a run is refused: %q", why)
	}
	if got := strings.Join(c.ExecutorProblems(), "\n"); !strings.Contains(got, "takes none of its runs") {
		t.Fatalf("said in the problems: %s", got)
	}
	w.applyConfig("ws", &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r3", Config: good.Config})
	if got := c.ExecutorFor("ws", "review", "claude"); got != box.Executor() || w.configProblem("ws") != "" {
		t.Fatalf("a usable revision puts it right: %s %q", got, w.configProblem("ws"))
	}
}

// A variable of the account is never replaced by what the server gives a
// stage; a profile of the worker's own file still may.
func TestAccountEnvWinsOverServerStageEnv(t *testing.T) {
	repo, _ := gitRepo(t)
	cfg := &Config{StateDir: t.TempDir(), ServerCommands: true, ContainedBy: "knot", Repos: map[string]*Repo{
		"ws":   {Path: repo},
		"file": {Path: repo, Stages: map[string]*StageProfile{"review": {Env: map[string]string{"SHARED": "from the file", "ONLY": "1"}}}},
	}}
	serverStages(t, cfg, "ws", map[string]*wsconfig.StageCaps{"review": {Env: map[string]string{"SHARED": "from the server", "ONLY": "1"}}})
	w := &Worker{cfg: cfg, sessions: map[string]string{}, accounts: map[string]string{}, executors: map[string]string{}, grants: map[string]string{}}
	acct := &Account{Name: "a", Env: map[string]string{"SHARED": "the account's"}}
	ar := w.agentRun(repo, "p", "", nil, 0, acct, &Run{ID: "r1", WorkspaceID: "ws", Stage: "review"}, "review")
	if ar.Env["SHARED"] != "the account's" || ar.Env["ONLY"] != "1" {
		t.Fatalf("server stage env over the account's: %v", ar.Env)
	}
	ar = w.agentRun(repo, "p", "", nil, 0, acct, &Run{ID: "r2", WorkspaceID: "file", Stage: "review"}, "review")
	if ar.Env["SHARED"] != "from the file" {
		t.Fatalf("the file's own profile: %v", ar.Env)
	}
}

// A run of a stage the server gives something gets its tools and env, and
// starts behind the wrapper its block attaches; a wrapper that cannot be
// used stops the run before it starts.
func TestAgentRunWithServerStage(t *testing.T) {
	repo, _ := gitRepo(t)
	state := t.TempDir()
	bin := filepath.Join(t.TempDir(), "fakecli")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{StateDir: state, Bin: bin, ServerCommands: true, Repos: map[string]*Repo{"ws": {Path: repo}}}
	cfg.setWrappers([]wsconfig.WrapperDef{{Name: "box", Argv: []string{"bwrap", "--bind", "{dir}", "{dir}", "{rundir}"}}})
	serverStages(t, cfg, "ws", map[string]*wsconfig.StageCaps{"review": {
		Tools: []string{"Bash(phpstan:*)"}, Env: map[string]string{"APP_ENV": "test"},
		Wrap: []wsconfig.WrapRule{{Kind: "claude", Wrapper: "box"}},
	}})
	w := &Worker{cfg: cfg, sessions: map[string]string{}, accounts: map[string]string{}, executors: map[string]string{}, grants: map[string]string{}}
	run := &Run{ID: "r1", WorkspaceID: "ws", Stage: "review"}
	ar := w.agentRun(repo, "p", "", []string{"Read"}, 0, &Account{Name: "a", Env: map[string]string{"X": "1"}}, run, "review")
	if ar.StartErr != "" || !slices.Contains(ar.AllowedTools, "Bash(phpstan:*)") || ar.Env["APP_ENV"] != "test" || ar.Env["X"] != "1" || !ar.Granted {
		t.Fatalf("the run: %+v", ar)
	}
	if len(ar.Wrapper) != 5 || ar.Wrapper[2] != repo || ar.Wrapper[4] != filepath.Join(state, "runs", "r1") {
		t.Fatalf("the wrapper for this run: %v", ar.Wrapper)
	}
	if got := w.executors["r1"]; !strings.HasPrefix(got, "wrapper:box:") || w.grants["r1"] == "" {
		t.Fatalf("recorded executor %q grants %q", got, w.grants["r1"])
	}
	if cmds := w.stageCommands("ws", "review", nil); !slices.Contains(cmds, "phpstan") {
		t.Fatalf("the prompt's commands: %v", cmds)
	}
	// The worker's setup and check commands of the stage start behind it too.
	wrap := w.stageShellWrap(run, &Account{Name: "a"}, "review")
	if wrap == nil {
		t.Fatal("no wrapper for the stage's own commands")
	}
	if argv, err := wrap(repo); err != nil || argv[0] != "bwrap" || argv[2] != repo {
		t.Fatalf("shell wrapper: %v %v", argv, err)
	}
	// A wrapper that cannot be used: nothing starts, nothing runs bare.
	cfg.wrapperBad["box"] = "its check failed"
	ar = w.agentRun(repo, "p", "", nil, 0, &Account{Name: "a"}, run, "review")
	if !strings.Contains(ar.StartErr, "its check failed") {
		t.Fatalf("start error: %q", ar.StartErr)
	}
	if res := w.runAgent(t.Context(), ar); res.Outcome != OutcomeError {
		t.Fatalf("the run started: %+v", res)
	}
	if _, err := w.stageShellWrap(run, &Account{Name: "a"}, "review")(repo); err == nil {
		t.Fatal("the stage's own commands would run bare")
	}
}

// Applying a config changes what the worker says contains its runs, and
// giving the workspace back to the file undoes it.
func TestApplyConfigUpdatesStagesAndFacts(t *testing.T) {
	c := executorConfig(t)
	c.ServerCommands = true
	c.ContainedBy = "knot"
	box := wsconfig.WrapperDef{Name: "box", Argv: []string{"bwrap", "{dir}"}}
	eff := &wsconfig.Effective{Workspace: "bare", Managed: true, Rev: "r1", Config: wsconfig.Config{
		Stages: map[string]*wsconfig.StageCaps{"review": {Tools: []string{"Bash(x:*)"}, Wrap: []wsconfig.WrapRule{{Kind: "opencode", Wrapper: "box"}}}}}}
	s := &configServer{effective: map[string]*wsconfig.Effective{"bare": eff}, revs: map[string]*wsconfig.Effective{"r1": eff},
		fleet: &wsconfig.Fleet{Rev: "f1", Wrappers: []wsconfig.WrapperDef{box}}}
	w, logs := managedWorker(t, c, s)
	if !w.refreshFleet(context.Background()) || !w.refreshConfig(context.Background()) {
		t.Fatal("the server's config was not fetched")
	}
	_ = w.runnerFacts()
	if got := c.ExecutorFor("bare", "review", "opencode-glm"); got != box.Executor() {
		t.Fatalf("after applying: %s", got)
	}
	if got := c.ExecutorFor("bare", "review", "claude"); got != "host:knot" {
		t.Fatalf("claude on a contained host: %s", got)
	}
	// The run's executor is compared with the claim's after its config is
	// held: a claim that matched the old state is given back.
	run := &Run{ID: "r1", WorkspaceID: "bare", Stage: "review", Runner: "opencode-glm", ConfigRev: "r1", Facts: []byte(`{"name":"opencode-glm","executor":"host:knot"}`)}
	if why := w.refuseExecutor(run); !strings.Contains(why, "the server matched this run on executor host:knot") {
		t.Fatalf("a stale claim: %q", why)
	}
	// No longer managed: the file applies again, and the facts follow.
	s.mu.Lock()
	s.effective["bare"] = &wsconfig.Effective{Workspace: "bare"}
	s.mu.Unlock()
	if !w.refreshConfig(context.Background()) {
		t.Fatal("refresh")
	}
	if c.stagesFromServer("bare") || c.ExecutorFor("bare", "review", "opencode-glm") != "host:knot" {
		t.Fatalf("the workspace still takes its stages from the server: %s", c.ExecutorFor("bare", "review", "opencode-glm"))
	}
	if w.factsSet {
		t.Fatal("the executors the worker reports were not worked out again")
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("errors in the log:\n%s", logs.String())
	}
}

// A run whose claim matched another executor than this worker would run it
// in is not started, and is given back for a retry: the next claim reports
// what the worker would really run it in.
func TestExecutorMismatchGivesTheRunBack(t *testing.T) {
	var mu sync.Mutex
	var report map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/report") {
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&report)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	c := executorConfig(t)
	c.StateDir = t.TempDir()
	client := NewClient(srv.URL, "k")
	client.leases["r1"] = "lease-1"
	w := New(c, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.execute(t.Context(), &Run{ID: "r1", ItemID: "i1", WorkspaceID: "bare", Stage: "plan", Runner: "claude", Attempt: 1,
		Facts: []byte(`{"name":"claude","executor":"wrapper:box:0123456789ab"}`)})
	mu.Lock()
	defer mu.Unlock()
	if report == nil {
		t.Fatal("nothing was reported")
	}
	reason, _ := report["reason"].(string)
	if report["retryable"] != true || !strings.Contains(reason, "the server matched this run on executor wrapper:box:0123456789ab") || report["status"] == "succeeded" {
		t.Fatalf("the report: %v", report)
	}
}
