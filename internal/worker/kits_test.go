package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// kitHome makes a home folder for the test with one credential folder in
// it, and forgets what earlier tests resolved.
func kitHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	kitCache.mu.Lock()
	kitCache.parts = nil
	kitCache.mu.Unlock()
	return home
}

type sandboxView struct {
	Sandbox struct {
		Enabled                   bool  `json:"enabled"`
		FailIfUnavailable         *bool `json:"failIfUnavailable"`
		AllowUnsandboxedCommands  *bool `json:"allowUnsandboxedCommands"`
		EnableWeakerNestedSandbox bool  `json:"enableWeakerNestedSandbox"`
		Filesystem                struct {
			AllowWrite []string `json:"allowWrite"`
			DenyRead   []string `json:"denyRead"`
		} `json:"filesystem"`
		Network struct {
			StrictAllowlist *bool    `json:"strictAllowlist"`
			AllowedDomains  []string `json:"allowedDomains"`
		} `json:"network"`
	} `json:"sandbox"`
}

// A stage block that names kits gets their tools and Claude Code's sandbox
// with their hosts and cache folders, the host's credential folders denied,
// and no way out of the sandbox; its own tools and sandbox settings stay.
func TestKitsBecomeToolsAndSandbox(t *testing.T) {
	home := kitHome(t)
	c := executorConfig(t)
	c.ServerCommands = true
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{
		"implement": {
			Kits:     []string{"bun", "github"},
			Tools:    []string{"Bash(./tests/run.sh:*)"},
			Settings: `{"sandbox":{"network":{"allowedDomains":["example.org"]}}}`,
		},
	})
	p := c.StageProfile("bare", "implement")
	for _, want := range []string{"Bash(./tests/run.sh:*)", "Bash(bun:*)", "Bash(bunx:*)", "Bash(git ls-remote:*)"} {
		if !slices.Contains(p.Tools, want) {
			t.Fatalf("tools %v lack %s", p.Tools, want)
		}
	}
	var s sandboxView
	if err := json.Unmarshal([]byte(p.Settings), &s); err != nil {
		t.Fatal(err)
	}
	sb := s.Sandbox
	if !sb.Enabled || sb.FailIfUnavailable == nil || !*sb.FailIfUnavailable || sb.AllowUnsandboxedCommands == nil || *sb.AllowUnsandboxedCommands ||
		sb.Network.StrictAllowlist == nil || !*sb.Network.StrictAllowlist || sb.EnableWeakerNestedSandbox {
		t.Fatalf("sandbox switches: %s", p.Settings)
	}
	for _, d := range []string{"example.org", "registry.npmjs.org", "github.com"} {
		if !slices.Contains(sb.Network.AllowedDomains, d) {
			t.Fatalf("hosts %v lack %s", sb.Network.AllowedDomains, d)
		}
	}
	if !slices.Contains(sb.Filesystem.AllowWrite, filepath.Join(home, ".bun/install/cache")) {
		t.Fatalf("bun's cache is not writable: %v", sb.Filesystem.AllowWrite)
	}
	if !slices.Contains(sb.Filesystem.DenyRead, filepath.Join(home, ".ssh")) || slices.Contains(sb.Filesystem.DenyRead, filepath.Join(home, ".aws")) {
		t.Fatalf("credential folders denied: %v (only the ones that exist)", sb.Filesystem.DenyRead)
	}
	if got := c.ExecutorFor("bare", "implement", "claude"); !strings.HasPrefix(got, "sandbox:") {
		t.Fatalf("a claude run of the stage is sandboxed: %s", got)
	}
	if got := c.ExecutorFor("bare", "implement", "opencode-glm"); got != "none:uncontained" {
		t.Fatalf("a runner that ignores the sandbox takes none of it: %s", got)
	}
	// The nested mode is this host's choice.
	c.SandboxNested = true
	if err := json.Unmarshal([]byte(c.StageProfile("bare", "implement").Settings), &s); err != nil || !s.Sandbox.EnableWeakerNestedSandbox {
		t.Fatalf("sandbox_nested sets the nested mode: %v", err)
	}
	if c.StageProfile("bare", "plan") != nil {
		t.Fatal("a stage without a block gets nothing")
	}
}

func TestBashProgram(t *testing.T) {
	for in, want := range map[string]string{
		"Bash(go:*)": "go", "Bash(git ls-remote:*)": "git", "Bash(./tests/run.sh:*)": "",
		"Bash(vendor/bin/*:*)": "", "Bash(npm run test)": "npm", "Read(/opt/**)": "", "Bash(FOO=1 go:*)": "",
	} {
		if got := bashProgram(in); got != want {
			t.Errorf("bashProgram(%q) = %q, want %q", in, got, want)
		}
	}
}

// The stage check, before any claim: a command a sandboxed stage is given
// that is not on the PATH, a cache folder (created), and a push token the
// remote refuses — each holds back the stage it concerns, with the reason,
// and leaves the workspace's other stages alone.
func TestStageCheck(t *testing.T) {
	home := kitHome(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec "+mustLook(t, "git")+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	// A remote that refuses the token for a push.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "Permission to acme/shop.git denied to someone.\n")
	}))
	t.Cleanup(refused.Close)

	clone := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.email=a@b", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command(mustLook(t, "git"), args...)
		cmd.Dir = clone
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	c := executorConfig(t)
	c.ServerCommands = true
	c.Repos["bare"].Path = clone
	c.Repos["bare"].Push.Kind, c.Repos["bare"].Push.RemoteURL, c.Repos["bare"].Push.TokenEnv = "github", refused.URL+"/acme/shop.git", "SHOP_PUSH_TOKEN"
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{"implement": {Kits: []string{"bun"}}})
	w := &Worker{cfg: c, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	reason := func(stage string) string {
		for _, f := range c.ExecutorFacts()["bare"] {
			if f.Executor == "none:check" && (f.Stage == stage || f.Stage == "") {
				return f.Reason
			}
		}
		return ""
	}

	t.Setenv("SHOP_PUSH_TOKEN", "")
	if !w.checkStages(ctx, "bare") {
		t.Fatal("the first failing check is a change")
	}
	if got := c.ExecutorFor("bare", "implement", "claude"); got != "none:check" {
		t.Fatalf("implement is held back: %s", got)
	}
	if got := c.ExecutorFor("bare", "plan", "claude"); got == "none:check" {
		t.Fatal("plan, which needs nothing here, is not")
	}
	why := reason("implement")
	for _, want := range []string{"not on the PATH runs get: bun", "SHOP_PUSH_TOKEN is empty"} {
		if !strings.Contains(why, want) {
			t.Fatalf("reason %q lacks %q", why, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(home, ".bun/install/cache")); err != nil || !fi.IsDir() {
		t.Fatalf("the kit's cache folder is created: %v", err)
	}
	if w.checkStages(ctx, "bare") {
		t.Fatal("not due again: nothing changed")
	}

	// The token set: the remote refuses it, and the token never shows.
	t.Setenv("SHOP_PUSH_TOKEN", "tok-1234567890")
	w.cfg.Repos["bare"].Push.TokenEnv = "SHOP_PUSH_TOKEN"
	w.AddSecrets("tok-1234567890")
	w.checkStages(ctx, "bare")
	if why := reason("implement"); !strings.Contains(why, "refuses the push token (SHOP_PUSH_TOKEN)") || strings.Contains(why, "tok-1234567890") {
		t.Fatalf("a refused token: %q", why)
	}

	// Fixed (bun on the PATH, a workspace that does not push): the check
	// runs again because what it depends on changed, and passes.
	if err := os.WriteFile(filepath.Join(bin, "bun"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.Repos["bare"].Push.Kind = "none"
	w.checkStages(ctx, "bare")
	if runtime.GOOS == "linux" {
		// On Linux the sandbox's own runtime is checked too.
		if why := reason("implement"); strings.Contains(why, "not on the PATH") || strings.Contains(why, "push token") {
			t.Fatalf("fixed, only the sandbox may remain: %q", why)
		}
		return
	}
	if got := c.ExecutorFor("bare", "implement", "claude"); got == "none:check" {
		t.Fatalf("fixed, the stage runs again: %q", reason("implement"))
	}
}

// On a host that is itself a container (contained_by: a knot space), kits
// give their tools and no sandbox: the space is the boundary. The stage
// check still looks for the commands.
func TestKitsOnAContainedHost(t *testing.T) {
	kitHome(t)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	c := executorConfig(t)
	c.ServerCommands = true
	c.ContainedBy = "knot"
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{"implement": {Kits: []string{"bun"}}})
	p := c.StageProfile("bare", "implement")
	if p == nil || !slices.Contains(p.Tools, "Bash(bun:*)") || p.sandboxed() {
		t.Fatalf("tools without the sandbox: %+v", p)
	}
	if got := c.ExecutorFor("bare", "implement", "claude"); got != "host:knot" {
		t.Fatalf("executor: %s", got)
	}
	w := &Worker{cfg: c, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w.checkStages(context.Background(), "bare")
	why := c.checkReason("bare", "implement")
	if !strings.Contains(why, "bun") || strings.Contains(why, "Claude Code's sandbox") {
		t.Fatalf("the check looks for the commands only: %q", why)
	}
	if got := c.ExecutorFor("bare", "implement", "claude"); got != "none:check" {
		t.Fatalf("held back: %s", got)
	}
}

// Kits are for Claude Code's sandbox: a runner that ignores it gets none of
// them (and runs as before), and a sandbox the worker's own file sets is
// not widened by them.
func TestKitsLeaveOtherRunnersAndTheFilesSandbox(t *testing.T) {
	kitHome(t)
	c := executorConfig(t)
	c.ServerCommands = true
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{"implement": {Kits: []string{"bun"}}})
	if got := c.ExecutorFor("bare", "implement", "opencode-glm"); got != "local" {
		t.Fatalf("opencode runs without the kits: %s", got)
	}
	if p, _ := c.stageProfileFor("bare", "implement", "opencode-glm"); p != nil {
		t.Fatalf("opencode is given nothing: %+v", p)
	}
	if p, _ := c.stageProfileFor("bare", "implement", "claude"); p == nil || !p.sandboxed() {
		t.Fatalf("claude gets the sandbox: %+v", p)
	}

	// "ws" has the worker file's own sandbox on review: the kits add their
	// tools, and the file's sandbox stays as it is.
	c2 := executorConfig(t)
	c2.ServerCommands = true
	before := c2.StageProfile("ws", "review").sandboxSettings()
	serverStages(t, c2, "ws", map[string]*wsconfig.StageCaps{"review": {Kits: []string{"bun"}}})
	p := c2.StageProfile("ws", "review")
	if !slices.Contains(p.Tools, "Bash(bun:*)") || p.sandboxSettings() != before {
		t.Fatalf("the file's sandbox is kept: %s (was %s), tools %v", p.sandboxSettings(), before, p.Tools)
	}
}

// The worker's own secret files are denied to a kit's sandboxed commands,
// wherever they are.
func TestKitsDenyTheWorkersOwnFiles(t *testing.T) {
	kitHome(t)
	dir := t.TempDir()
	envFile := filepath.Join(dir, "skopos-worker.env")
	if err := os.WriteFile(envFile, []byte("X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := executorConfig(t)
	c.ServerCommands = true
	c.SetSecretFiles(envFile, filepath.Join(dir, "missing.toml"))
	serverStages(t, c, "bare", map[string]*wsconfig.StageCaps{"implement": {Kits: []string{"bun"}}})
	var s sandboxView
	if err := json.Unmarshal([]byte(c.StageProfile("bare", "implement").Settings), &s); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Sandbox.Filesystem.DenyRead, envFile) || slices.Contains(s.Sandbox.Filesystem.DenyRead, filepath.Join(dir, "missing.toml")) {
		t.Fatalf("denied: %v", s.Sandbox.Filesystem.DenyRead)
	}
}

func mustLook(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found", name)
	}
	return p
}

// A workspace's kits reach every stage; a stage block's own kits add to
// them, and its other fields stay.
func TestWorkspaceKitsReachEveryStage(t *testing.T) {
	kitHome(t)
	c := executorConfig(t)
	c.ServerCommands = true
	kits := []string{"bun", "task"}
	m, err := c.mergeManaged("bare", &wsconfig.Effective{Workspace: "bare", Managed: true, Rev: "r1", Config: wsconfig.Config{
		Kits:   &kits,
		Stages: map[string]*wsconfig.StageCaps{"qa": {Kits: []string{"github"}, Tools: []string{"Bash(./qa.sh:*)"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.setStages("bare", m.stages)
	for _, stage := range wsconfig.StageNames {
		p := c.StageProfile("bare", stage)
		if p == nil || !slices.Contains(p.Tools, "Bash(bun:*)") || !slices.Contains(p.Tools, "Bash(task:*)") || !p.sandboxed() {
			t.Fatalf("%s lacks the workspace's kits: %+v", stage, p)
		}
	}
	qa := c.StageProfile("bare", "qa")
	if !slices.Contains(qa.Tools, "Bash(git ls-remote:*)") || !slices.Contains(qa.Tools, "Bash(./qa.sh:*)") {
		t.Fatalf("qa keeps its own block: %v", qa.Tools)
	}
	if m.commands["kits"] != "bun,task" {
		t.Fatalf("the kits are logged as a command field: %q", m.commands["kits"])
	}
	// Without server_commands the file's profiles apply: no kits.
	c2 := executorConfig(t)
	m2, err := c2.mergeManaged("bare", &wsconfig.Effective{Workspace: "bare", Managed: true, Rev: "r1", Config: wsconfig.Config{Kits: &kits}})
	if err != nil || len(m2.stages) != 0 || !slices.Contains(m2.local, "kits") {
		t.Fatalf("kits stay off without server_commands: %+v, %v", m2, err)
	}
}

// A run may reach the server's other tools through the lean toolset's
// search tools: the server checks each execute_tool target against the
// run's own tools.
func TestSkoposToolsReachTheLeanToolset(t *testing.T) {
	for _, phase := range []string{"plan", "implement", "review"} {
		got := skoposTools(phase)
		for _, want := range []string{"mcp__skopos__tool_search", "mcp__skopos__execute_tool"} {
			if !slices.Contains(got, want) {
				t.Fatalf("%s: %v lacks %s", phase, got, want)
			}
		}
	}
	if !slices.Contains(skoposTools("implement"), "mcp__skopos__plan_update_item") {
		t.Fatal("implement marks its plan's steps")
	}
}
