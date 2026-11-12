package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/policy"
)

func executorConfig(t *testing.T) *Config {
	t.Helper()
	c := &Config{
		Runners:    []string{"claude", "opencode-glm", "codex"},
		Workspaces: []string{"ws", "bare"},
		RunnerSpecs: map[string]*RunnerSpec{
			"opencode-glm": {Kind: "opencode"},
			"codex":        {Kind: "codex"},
		},
		Accounts: []*Account{
			{Name: "c", Runner: "claude"},
			{Name: "g", Runner: "opencode-glm", Model: "zai-coding-plan/glm-5.3"},
			{Name: "x", Runner: "codex"},
		},
		Profiles: map[string]map[string]*StageProfile{"php": {
			"implement": {Wrapper: []string{"bwrap", "--bind", "{dir}", "{dir}"}, Tools: []string{"Bash(php:*)"}},
			"review":    {Settings: `{"sandbox":{"enabled":true,"network":{"allowedDomains":[]}},"permissions":{"allow":["Bash(phpunit:*)"]}}`},
		}},
		Repos: map[string]*Repo{"ws": {Path: "/clones/ws", Profile: "php"}, "bare": {Path: "/clones/bare"}},
	}
	if err := c.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return c
}

// What contains a run, in order: the stage's wrapper, else a runner whose
// lockdown is a sandbox, else the host when the worker says it is a
// container, else the stage's sandbox settings for a runner that honours
// them, else local.
func TestExecutorForOrder(t *testing.T) {
	was := codexSandboxVerified
	codexSandboxVerified = true
	t.Cleanup(func() { codexSandboxVerified = was })
	c := executorConfig(t)
	wrapper := c.ExecutorFor("ws", "implement", "claude")
	if !strings.HasPrefix(wrapper, "wrapper:php:") || len(wrapper) != len("wrapper:php:")+12 {
		t.Fatalf("a wrapper is named after its profile: %q", wrapper)
	}
	for _, r := range []string{"opencode-glm", "codex"} {
		if got := c.ExecutorFor("ws", "implement", r); got != wrapper {
			t.Errorf("a wrapper goes around every runner (%s): %q", r, got)
		}
	}
	sandbox := c.ExecutorFor("ws", "review", "claude")
	if !strings.HasPrefix(sandbox, "sandbox:") {
		t.Fatalf("claude honours the stage's sandbox settings: %q", sandbox)
	}
	for _, stage := range []string{"second_opinion", "branch_review"} {
		if got := c.ExecutorFor("ws", stage, "claude"); got != sandbox {
			t.Errorf("%s runs with the review profile: %q", stage, got)
		}
	}
	// The review profile gives the stage tools, and its sandbox settings
	// contain only a runner that honours them: the worker does not offer
	// the stage for the others (a run would be refused before it starts).
	for _, r := range []string{"opencode-glm", "codex"} {
		for _, stage := range []string{"review", "branch_review"} {
			if got := c.ExecutorFor("ws", stage, r); got != policy.NoExecutor(policy.NoneUncontained) {
				t.Errorf("%s %s: a granted stage nothing contains: %q", stage, r, got)
			}
		}
	}
	if got := c.ExecutorFor("bare", "review", "opencode-glm"); got != "local" {
		t.Errorf("opencode without a profile: %q", got)
	}
	if got := c.ExecutorFor("bare", "review", "codex"); got != "runner:codex" {
		t.Errorf("codex's own lockdown is a sandbox: %q", got)
	}
	// A branch review runs the repo's setup and checks on the worker,
	// outside codex's sandbox: only a wrapper would go around those.
	if got := c.ExecutorFor("bare", "branch_review", "codex"); got != "local" {
		t.Errorf("codex on a stage whose checks the worker runs itself: %q", got)
	}
	if got := c.ExecutorFor("bare", "plan", "claude"); got != "local" {
		t.Errorf("no profile: %q", got)
	}
	if got := c.ExecutorFor("bare", "plan", "codex"); got != "runner:codex" {
		t.Errorf("codex without a profile: %q", got)
	}
	// Where the codex lockdown is not verified it is not reported.
	codexSandboxVerified = false
	if got := c.ExecutorFor("bare", "plan", "codex"); got != "local" {
		t.Errorf("codex on an OS its lockdown is not verified on: %q", got)
	}
	codexSandboxVerified = true
	// The host as a container: below what goes around the run itself,
	// above the sandbox settings (which leave the worker readable).
	c.ContainedBy = "knot"
	for stage, want := range map[string]string{"implement": wrapper, "review": "host:knot", "plan": "host:knot"} {
		if got := c.ExecutorFor("ws", stage, "claude"); got != want {
			t.Errorf("contained_by, %s: %q, want %q", stage, got, want)
		}
	}
	if got := c.ExecutorFor("bare", "plan", "codex"); got != "runner:codex" {
		t.Errorf("a runner sandbox ranks above the host: %q", got)
	}
	for _, e := range []string{wrapper, "runner:codex"} {
		if !policy.Isolated(e) || !policy.KnownExecutor(e) {
			t.Errorf("%q must count as isolated", e)
		}
	}
	if policy.Isolated(sandbox) || policy.Contained(sandbox) || !policy.KnownExecutor(sandbox) || !policy.ExecutorSatisfies("sandbox:*", sandbox) {
		t.Error("sandbox settings are neither isolated nor contained; a rule names them")
	}
	if policy.Isolated("host:knot") || !policy.Contained("host:knot") {
		t.Error("a host container is contained, not isolated")
	}
	// Names from the config are made safe inside the id.
	c.ContainedBy = ""
	c.Profiles["odd name:x"] = map[string]*StageProfile{"plan": {Wrapper: []string{"w"}, name: "odd name:x"}}
	c.Repos["bare"].Profile = "odd name:x"
	if got := c.ExecutorFor("bare", "plan", "claude"); !strings.HasPrefix(got, "wrapper:odd-name-x:") {
		t.Errorf("profile name in the id: %q", got)
	}
}

// The wrapper's id covers its argv only; what the profile grants is its own
// hash (env NAMES, never values), "" when it grants nothing.
func TestExecutorAndGrantsAreSeparate(t *testing.T) {
	mk := func(p *StageProfile) (*Config, *StageProfile) {
		c := &Config{Repos: map[string]*Repo{"ws": {Path: "/x", Stages: map[string]*StageProfile{"implement": p}}}}
		if err := c.Default(t.TempDir()); err != nil {
			t.Fatal(err)
		}
		return c, p
	}
	a, pa := mk(&StageProfile{Wrapper: []string{"w", "{dir}"}})
	b, pb := mk(&StageProfile{Wrapper: []string{"w", "{dir}"}, Tools: []string{"Bash(go:*)"}, Env: map[string]string{"KEY": "one"}})
	b2, pb2 := mk(&StageProfile{Wrapper: []string{"w", "{dir}"}, Tools: []string{"Bash(go:*)"}, Env: map[string]string{"KEY": "two"}})
	other, _ := mk(&StageProfile{Wrapper: []string{"w", "--net", "{dir}"}})
	ea, eb := a.ExecutorFor("ws", "implement", "claude"), b.ExecutorFor("ws", "implement", "claude")
	if ea != eb || !strings.HasPrefix(ea, "wrapper:repo:") {
		t.Fatalf("grants must not change the executor: %q %q", ea, eb)
	}
	if other.ExecutorFor("ws", "implement", "claude") == ea {
		t.Fatal("another argv is another executor")
	}
	if pa.Grants() != "" {
		t.Fatalf("no grants: %q", pa.Grants())
	}
	if pb.Grants() == "" || len(pb.Grants()) != 12 || pb.Grants() != pb2.Grants() {
		t.Fatalf("grants hash covers env names, not values: %q %q", pb.Grants(), pb2.Grants())
	}
	// Settings besides the sandbox block are grants too; the sandbox block
	// alone is not.
	only := &StageProfile{Settings: `{"sandbox":{"enabled":true}}`}
	dirs := &StageProfile{Settings: `{"sandbox":{"enabled":true},"permissions":{"additionalDirectories":["/srv/cache"]}}`}
	hooks := &StageProfile{Settings: `{"sandbox":{"enabled":true},"hooks":{"PreToolUse":[]}}`}
	if only.Grants() != "" || dirs.Grants() == "" || hooks.Grants() == "" || dirs.Grants() == hooks.Grants() {
		t.Fatalf("settings grants: %q %q %q", only.Grants(), dirs.Grants(), hooks.Grants())
	}
	_ = b2
	var none *StageProfile
	if none.Grants() != "" {
		t.Fatal("nil profile grants nothing")
	}
}

// A wrapper that only sets variables confines nothing: it declares the
// host, like worker.contained_by, and never counts as isolated.
func TestHostDeclarations(t *testing.T) {
	c := &Config{Repos: map[string]*Repo{"ws": {Path: "/x", Stages: map[string]*StageProfile{
		"implement": {Wrapper: []string{"env", "SKOPOS_CONTAINED_BY=knot"}, Tools: []string{"Bash(go:*)"}},
		"review":    {Wrapper: []string{"/usr/bin/env", "A=b"}},
		"qa":        {Wrapper: []string{"env", "-i", "bwrap"}},
	}}}}
	// env with nothing to run is the host, whatever its options; env that
	// runs a command is that command's wrapper.
	for _, tc := range []struct {
		wrapper []string
		host    string
	}{
		{[]string{"env"}, "env"},
		{[]string{"env", "-i", "A=b"}, "env"},
		{[]string{"env", "--", "A=b"}, "env"},
		{[]string{"env", "-u", "X", "SKOPOS_CONTAINED_BY=knot"}, "knot"},
		{[]string{"env", "-C", "/srv", "A=b"}, "env"},
		{[]string{"env", "-S", "bwrap --bind / /"}, "env"},
		{[]string{"env", "SKOPOS_CONTAINED_BY=a b:c"}, "a-b-c"},
		{[]string{"env", "A=b", "bwrap", "--bind", "/x", "/x"}, ""},
		{[]string{"env", "-u", "X", "docker", "run"}, ""},
		{[]string{"bwrap"}, ""},
	} {
		name, host := (&StageProfile{Wrapper: tc.wrapper}).hostWrapper()
		if host != (tc.host != "") || (host && name != tc.host) {
			t.Errorf("%v: host=%v name=%q, want %q", tc.wrapper, host, name, tc.host)
		}
	}
	if err := c.Default(t.TempDir()); err != nil {
		t.Fatalf("an env wrapper still lets a stage grant capabilities, as before: %v", err)
	}
	if got := c.ExecutorFor("ws", "implement", "claude"); got != "host:knot" {
		t.Errorf("env wrapper naming the host: %q", got)
	}
	if got := c.ExecutorFor("ws", "review", "claude"); got != "host:env" {
		t.Errorf("env wrapper without a name: %q", got)
	}
	if got := c.ExecutorFor("ws", "qa", "claude"); !strings.HasPrefix(got, "wrapper:repo:") {
		t.Errorf("env running a real wrapper is a wrapper: %q", got)
	}
	// contained_by lets a stage grant capabilities without any wrapper.
	checkConfigCases(t, []configCase{
		{"grants on a contained host", Config{ContainedBy: "knot", Profiles: map[string]map[string]*StageProfile{
			"php": {"implement": {Tools: []string{"Bash(php:*)"}}}}}, ""},
		{"grants on a bare host", Config{Profiles: map[string]map[string]*StageProfile{
			"php": {"implement": {Tools: []string{"Bash(php:*)"}}}}}, "worker.contained_by"},
		{"contained_by is a short name", Config{ContainedBy: "my knot space!"}, "contained_by"},
		{"an unknown placeholder", Config{Profiles: map[string]map[string]*StageProfile{
			"php": {"implement": {Wrapper: []string{"bwrap", "--bind", "{home}", "{home}"}}}}}, "{home}, which is not a placeholder"},
		{"braces that are not placeholders", Config{Profiles: map[string]map[string]*StageProfile{
			"php": {"implement": {Wrapper: []string{"docker", "run", "--label", `{"a":1}`, "--format", "{{.ID}}", "-v", "{dir}:{dir}"}}}}}, ""},
	})
}

// The executor facts a worker reports resolve, for every workspace, stage
// and runner, to exactly what a run there would record.
func TestExecutorFactsMatchWhatRuns(t *testing.T) {
	for _, containedBy := range []string{"", "knot"} {
		c := executorConfig(t)
		c.ContainedBy = containedBy
		facts := c.ExecutorFacts()
		if len(facts) != 2 {
			t.Fatalf("one entry per workspace: %+v", facts)
		}
		for _, ws := range c.Workspaces {
			if len(facts[ws]) == 0 || facts[ws][0].Stage != "" || facts[ws][0].Runner != "" {
				t.Fatalf("%s: the first fact is the default: %+v", ws, facts[ws])
			}
			for _, stage := range executorStages {
				for _, r := range c.Runners {
					want := c.ExecutorFor(ws, stage, r)
					if got := policy.ResolveExecutor(facts[ws], stage, r); got != want {
						t.Errorf("contained_by=%q %s %s %s: facts resolve to %q, a run records %q", containedBy, ws, stage, r, got, want)
					}
				}
			}
		}
		// A second opinion and a branch review run as a review.
		if got, want := policy.ResolveExecutor(facts["ws"], "second_opinion", "claude"), c.ExecutorFor("ws", "review", "claude"); got != want {
			t.Errorf("second opinion: %q, want %q", got, want)
		}
	}
}

// Placeholders: every one a wrapper uses gets its value; one without a
// value is an error, not an empty bind; an unused one needs none.
func TestExpandWrapper(t *testing.T) {
	vals := map[string]string{"{dir}": "/wt", "{gitdir}": "/clone/.git", "{login}": "/home/w/.claude", "{rundir}": "/state/runs/r1",
		"{cli}": "/opt/cli/bin", "{run}": "r1", "{stage}": "review", "{workspace}": "ws"}
	got, err := expandWrapper([]string{"bwrap", "--bind", "{dir}", "{dir}", "--bind", "{gitdir}", "{gitdir}", "--ro-bind", "{cli}", "{cli}",
		"--bind", "{login}", "{login}", "--bind", "{rundir}", "{rundir}", "--setenv", "RUN", "{stage}/{run}/{workspace}"}, vals)
	want := "bwrap --bind /wt /wt --bind /clone/.git /clone/.git --ro-bind /opt/cli/bin /opt/cli/bin --bind /home/w/.claude /home/w/.claude --bind /state/runs/r1 /state/runs/r1 --setenv RUN review/r1/ws"
	if err != nil || strings.Join(got, " ") != want {
		t.Fatalf("expanded: %v %v", got, err)
	}
	delete(vals, "{login}")
	if _, err := expandWrapper([]string{"bwrap", "--bind", "{login}", "{login}"}, vals); err == nil || !strings.Contains(err.Error(), "login_dir") {
		t.Fatalf("a placeholder without a value must fail the run: %v", err)
	}
	if got, err := expandWrapper([]string{"docker", "run", "-v", "{dir}:{dir}"}, vals); err != nil || got[3] != "/wt:/wt" {
		t.Fatalf("an unused placeholder needs no value: %v %v", got, err)
	}
}

// Where each kind keeps its login, and login_dir over all of them.
func TestLoginDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_DATA_HOME"} {
		t.Setenv(k, "")
	}
	c := &Config{RunnerSpecs: map[string]*RunnerSpec{
		"oc": {Kind: "opencode"}, "codex": {Kind: "codex"}, "agy": {Kind: "antigravity"}, "mine": {Kind: "command", Command: []string{"x", "{prompt}"}},
	}}
	for _, tc := range []struct {
		a    *Account
		want string
	}{
		{&Account{Runner: "claude"}, filepath.Join(home, ".claude")},
		{&Account{Runner: "claude", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/logins/c2"}}, "/logins/c2"},
		{&Account{Runner: "codex", Env: map[string]string{"CODEX_HOME": "/logins/codex"}}, "/logins/codex"},
		{&Account{Runner: "oc"}, filepath.Join(home, ".local", "share", "opencode")},
		{&Account{Runner: "oc", Env: map[string]string{"XDG_DATA_HOME": "/logins/oc"}}, "/logins/oc/opencode"},
		{&Account{Runner: "agy", Env: map[string]string{"HOME": "/logins/agy"}}, "/logins/agy/.gemini"},
		{&Account{Runner: "mine"}, ""},
		{&Account{Runner: "mine", LoginDir: "/logins/mine"}, "/logins/mine"},
		{&Account{Runner: "claude", LoginDir: "/logins/override"}, "/logins/override"},
	} {
		if got := c.loginDir(tc.a); got != tc.want {
			t.Errorf("%s %v: %q, want %q", tc.a.Runner, tc.a.Env, got, tc.want)
		}
	}
	checkConfigCases(t, []configCase{
		{"relative login_dir", Config{Accounts: []*Account{{Name: "a", LoginDir: "logins/a"}}}, "login_dir must be an absolute path"},
	})
}

// A run's agent gets the new placeholders filled from the run, a temp
// directory of its own under the run's directory, and the executor and
// grants it will report.
func TestAgentRunPlaceholdersAndTempDir(t *testing.T) {
	repo, _ := gitRepo(t)
	state := t.TempDir()
	bin := filepath.Join(t.TempDir(), "fakecli")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prof := &StageProfile{Wrapper: []string{"bwrap", "{dir}", "{gitdir}", "{login}", "{rundir}", "{cli}"}}
	cfg := &Config{StateDir: state, Bin: bin, Repos: map[string]*Repo{"ws": {Path: repo, Stages: map[string]*StageProfile{"review": prof}}}}
	w := &Worker{cfg: cfg, sessions: map[string]string{}, accounts: map[string]string{}, executors: map[string]string{}, grants: map[string]string{}}
	run := &Run{ID: "r1", WorkspaceID: "ws", Stage: "review"}
	acct := &Account{Name: "a", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/logins/a"}}
	ar := w.agentRun(repo, "p", "", []string{"Read"}, 0, acct, run, "review")
	gitDir, _ := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	cli, _ := filepath.EvalSymlinks(filepath.Dir(bin))
	gotGit, _ := filepath.EvalSymlinks(ar.Wrapper[2])
	if ar.StartErr != "" || len(ar.Wrapper) != 6 || ar.Wrapper[1] != repo || gotGit != gitDir || ar.Wrapper[3] != "/logins/a" ||
		ar.Wrapper[4] != filepath.Join(state, "runs", "r1") || ar.Wrapper[5] != cli {
		t.Fatalf("wrapper: %v (%s), want git %s cli %s", ar.Wrapper, ar.StartErr, gitDir, cli)
	}
	// A wrapper that binds {cli} starts the agent by its resolved path: the
	// shim directory on PATH is not inside it.
	realBin, _ := filepath.EvalSymlinks(bin)
	if name, argv := agentCommand(ar, []string{"-p"}); name != "bwrap" || argv[len(argv)-2] != realBin || ar.WrapBin != realBin {
		t.Fatalf("the wrapped command runs the resolved CLI: %s %v (%q)", name, argv, ar.WrapBin)
	}
	if ar.TempDir != filepath.Join(state, "runs", "r1", "tmp") {
		t.Fatalf("temp dir: %q", ar.TempDir)
	}
	if fi, err := os.Stat(ar.TempDir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the run's temp dir must exist, private: %v %v", fi, err)
	}
	if !strings.HasPrefix(w.executors["r1"], "wrapper:repo:") || w.grants["r1"] != "" || ar.HostContained {
		t.Fatalf("executor %q grants %q host %v", w.executors["r1"], w.grants["r1"], ar.HostContained)
	}
	// A wrapper that needs a login directory the account does not have
	// stops the run before it starts.
	cfg.RunnerSpecs = map[string]*RunnerSpec{"mine": {Kind: "command", Command: []string{bin, "{prompt}"}}}
	ar = w.agentRun(repo, "p", "", nil, 0, &Account{Name: "m", Runner: "mine"}, run, "review")
	if !strings.Contains(ar.StartErr, "{login}") {
		t.Fatalf("missing login dir: %q", ar.StartErr)
	}
	if res := w.runAgent(t.Context(), ar); res.Outcome != OutcomeError || !strings.Contains(res.Detail, "{login}") {
		t.Fatalf("the run must fail with the wrapper error: %+v", res)
	}
	// A run id that is not a plain name gets no directory.
	for _, id := range []string{"../x", "..", ".", "", "a/b"} {
		if got := cfg.runTempDir(id); got != "" {
			t.Fatalf("temp dir for run id %q: %q", id, got)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "tmp")); err == nil {
		t.Fatal("a bad run id must not create a directory in the state dir")
	}
	// On a contained host the fail-closed rule is met without a wrapper.
	cfg.ContainedBy = "knot"
	run.Stage = "plan"
	ar = w.agentRun(repo, "p", "", nil, 0, acct, run, "plan")
	if !ar.HostContained || w.executors["r1"] != "host:knot" {
		t.Fatalf("contained host: %v %q", ar.HostContained, w.executors["r1"])
	}
}

// The worker's own setup and check commands of a wrapped stage run behind
// the stage's wrapper — they execute the branch's code — and plainly
// otherwise.
func TestSetupAndChecksRunInsideTheWrapper(t *testing.T) {
	dir := t.TempDir()
	// A stand-in wrapper: records what it was asked to run, then runs it.
	wrapper := filepath.Join(t.TempDir(), "wrap")
	log := filepath.Join(t.TempDir(), "wrapped.log")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\necho \"$1 | $*\" >> "+log+"\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{StateDir: t.TempDir(), Repos: map[string]*Repo{
		"ws":   {Path: dir, Stages: map[string]*StageProfile{"implement": {Wrapper: []string{wrapper, "{dir}"}}}},
		"env":  {Path: dir, Stages: map[string]*StageProfile{"implement": {Wrapper: []string{"env", "SKOPOS_CONTAINED_BY=knot"}}}},
		"bare": {Path: dir},
	}}
	w := &Worker{cfg: cfg}
	acct := &Account{Name: "a"}
	ctx := withShellWrap(t.Context(), w.stageShellWrap(&Run{ID: "r1", WorkspaceID: "ws", Stage: "implement"}, acct, "implement"))
	checks := runChecks(ctx, dir, []string{"echo checked"}, nil, nil, runLimits{total: time.Minute})
	if len(checks) != 1 || !checks[0].Passed || strings.TrimSpace(checks[0].Output) != "checked" {
		t.Fatalf("the wrapped check still runs: %+v", checks)
	}
	if _, err := runSetup(ctx, dir, []string{"echo set up"}, dir, nil, nil, nil, runLimits{total: time.Minute}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(log)
	want := dir + " | " + dir + " sh -c echo checked\n" + dir + " | " + dir + " sh -c echo set up\n"
	if string(got) != want {
		t.Fatalf("setup and checks must go through the wrapper, placeholders filled:\n%s\nwant:\n%s", got, want)
	}
	// No real wrapper (none, or one that only declares the host): plain.
	for _, ws := range []string{"env", "bare"} {
		if fn := w.stageShellWrap(&Run{ID: "r1", WorkspaceID: ws, Stage: "implement"}, acct, "implement"); fn != nil {
			t.Errorf("%s: nothing to run setup and checks behind", ws)
		}
	}
	// A wrapper that cannot be prepared fails the command, it is not run bare.
	cfg.Repos["ws"].Stages["implement"].Wrapper = []string{wrapper, "{gitdir}"}
	ctx = withShellWrap(t.Context(), w.stageShellWrap(&Run{ID: "r1", WorkspaceID: "ws", Stage: "implement"}, acct, "implement"))
	if out, err := runShell(ctx, dir, nil, "echo bare", 0); err == nil || !strings.Contains(err.Error(), "{gitdir}") || len(out) != 0 {
		t.Fatalf("an unprepared wrapper must stop the command: %q %v", out, err)
	}
}

// Files the worker writes into a run's directory never follow a symlink a
// wrapped agent left there: the link is replaced.
func TestRunDirectoryWritesDoNotFollowSymlinks(t *testing.T) {
	state := t.TempDir()
	victim := filepath.Join(t.TempDir(), "skopos-config.toml")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{StateDir: state}
	runDir := cfg.runDir("r1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"implement.log", "mcp.json", "mcp.json.tmp", "tmp"} {
		if err := os.Symlink(victim, filepath.Join(runDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeRunLog(filepath.Join(runDir, "implement.log"), []byte("agent output"), nil)
	if err := writeFile0600(filepath.Join(runDir, "mcp.json"), []byte(`{"mcpServers":{}}`)); err != nil {
		t.Fatal(err)
	}
	tmp := cfg.runTempDir("r1")
	if b, _ := os.ReadFile(victim); string(b) != "keep me" {
		t.Fatalf("a symlink in the run directory was written through: %q", b)
	}
	for _, name := range []string{"implement.log", "mcp.json"} {
		fi, err := os.Lstat(filepath.Join(runDir, name))
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s must be the worker's own file: %v %v", name, fi, err)
		}
	}
	if fi, err := os.Lstat(tmp); err != nil || !fi.IsDir() || tmp != filepath.Join(runDir, "tmp") {
		t.Fatalf("the temp directory must be a real directory: %v %v", fi, err)
	}
}

// Before anything runs, the worker compares the executor the server
// matched the policy on with the one it would use; a difference stops the
// run.
func TestRefuseExecutor(t *testing.T) {
	c := executorConfig(t)
	w := &Worker{cfg: c}
	here := c.ExecutorFor("ws", "implement", "claude")
	facts := func(executor string) []byte { return []byte(`{"name":"claude","executor":"` + executor + `"}`) }
	for _, tc := range []struct {
		name string
		run  *Run
		want string
	}{
		{"the same", &Run{WorkspaceID: "ws", Stage: "implement", Runner: "claude", Facts: facts(here)}, ""},
		{"the claim matched a wrapper this worker does not have", &Run{WorkspaceID: "bare", Stage: "implement", Runner: "claude", Facts: facts(here)}, "this worker would run it in local"},
		{"the claim matched local, the worker would wrap", &Run{WorkspaceID: "ws", Stage: "implement", Runner: "claude", Facts: facts("local")}, "the server matched this run on executor local"},
		{"a server that records no executor", &Run{WorkspaceID: "ws", Stage: "implement", Runner: "claude", Facts: []byte(`{"name":"claude"}`)}, ""},
		{"no facts at all", &Run{WorkspaceID: "ws", Stage: "implement", Runner: "claude"}, ""},
	} {
		if got := w.refuseExecutor(tc.run); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// {login} comes from the environment the agent really gets: a variable from
// the worker's env file never reaches the CLI, so it names no login.
func TestLoginDirIgnoresEnvFileValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/from/the/env/file")
	c := &Config{}
	c.SetEnvFileKeys([]string{"CLAUDE_CONFIG_DIR"})
	if got := c.loginDir(&Account{Runner: "claude"}); got != filepath.Join(home, ".claude") {
		t.Fatalf("an env-file value must not name the login directory: %q", got)
	}
	if got := c.loginDir(&Account{Runner: "claude", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/logins/c"}}); got != "/logins/c" {
		t.Fatalf("the account's own env does: %q", got)
	}
}
