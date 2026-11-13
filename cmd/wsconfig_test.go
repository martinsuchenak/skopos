package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// --set and --unset change single fields of a config; an unknown field or a
// value of the wrong type is refused, never dropped.
func TestEditConfig(t *testing.T) {
	c, err := editConfig(wsconfig.Config{}, []string{"default_branch=develop", `checks=["go vet ./...", "go test ./..."]`, "push.kind=github", "check_fix_rounds=3", "plan_timeout=20m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if *c.DefaultBranch != "develop" || len(*c.Checks) != 2 || *c.Push.Kind != "github" || *c.CheckFixRounds != 3 || c.PlanTimeout.D().Minutes() != 20 {
		t.Fatalf("set: %s", wsconfig.Canonical(c))
	}
	c, err = editConfig(c, []string{"setup=[]"}, []string{"checks", "push.kind"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Checks != nil || c.Push != nil || c.Setup == nil || len(*c.Setup) != 0 || *c.DefaultBranch != "develop" {
		t.Fatalf("unset removes the field; an empty list stays set: %s", wsconfig.Canonical(c))
	}
	if _, err := editConfig(c, []string{"chekcs=[]"}, nil); err == nil || !strings.Contains(err.Error(), "not a config field") {
		t.Fatalf("an unknown field: %v", err)
	}
	if _, err := editConfig(c, []string{"checks=make test"}, nil); err == nil {
		t.Fatal("a list field given plain text must be refused")
	}
	if _, err := editConfig(c, []string{"noequals"}, nil); err == nil {
		t.Fatal("field=value is required")
	}
}

// A config round-trips through the TOML `show --toml` prints and
// `set --file` reads.
func TestConfigTOMLRoundTrip(t *testing.T) {
	text := `default_branch = "develop"
setup = ["npm ci"]
checks = []
check_fix_rounds = 0
quiet_timeout = "5m0s"

[push]
kind = "github"
remote_url = "https://github.com/a/b.git"
steps = false
`
	var c wsconfig.Config
	if _, err := toml.Decode(text, &c); err != nil {
		t.Fatal(err)
	}
	norm, err := wsconfig.Normalize(c)
	if err != nil {
		t.Fatal(err)
	}
	if norm.Checks == nil || len(*norm.Checks) != 0 || *norm.CheckFixRounds != 0 || *norm.Push.Steps || norm.QuietTimeout.D().Minutes() != 5 {
		t.Fatalf("decoded: %s", wsconfig.Canonical(norm))
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(norm); err != nil {
		t.Fatal(err)
	}
	var back wsconfig.Config
	if _, err := toml.Decode(buf.String(), &back); err != nil {
		t.Fatal(err)
	}
	if string(wsconfig.Canonical(back)) != string(wsconfig.Canonical(norm)) {
		t.Fatalf("round trip:\n%s\n%s\n--- toml ---\n%s", wsconfig.Canonical(back), wsconfig.Canonical(norm), buf.String())
	}
}

const importWorkerA = `[worker]
id = "worker-a"
plan_timeout = "15m"
mcp_servers = ["mcp__router"]
mcp_config = "/etc/mcp.json"

[worker.runner_config.glm]
kind = "opencode"

[worker.repos."fabrikam/api"]
path = "/clones/api"
default_branch = "develop"
setup = ["npm ci"]
checks = ["npm test"]
setup_env = { NPM_TOKEN = "${NPM_TOKEN}" }
[worker.repos."fabrikam/api".push]
kind = "github"
remote_url = "https://github.com/fabrikam/api.git"
token_env = "API_PUSH_TOKEN"
pull_request = "draft"

[worker.repos."fabrikam/web"]
path = "/clones/web"
checks = ["make test"]

[[worker.accounts]]
name = "c1"
runner = "claude"
model = "opus"
effort = "high"

[[worker.accounts]]
name = "g1"
runner = "glm"
model = "zai-coding-plan/glm-5.3"

[[worker.accounts]]
name = "pool"
kind = "claude"
`

// The import proposes, per workspace, the fields a worker's file sets —
// never the machine's own (path, setup_env, the token variable) — and a
// runner definition per runner its accounts name. Two files that disagree
// are a conflict that is left out, not merged.
func TestProposeImport(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.toml")
	if err := os.WriteFile(a, []byte(importWorkerA), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := proposeImport([]string{a})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.conflicts) != 0 || len(p.workspaces) != 2 {
		t.Fatalf("proposal: %+v", p)
	}
	api := p.workspaces["fabrikam/api"]
	got := strings.Join(api.FieldNames(), ",")
	if got != "checks,default_branch,mcp_servers,plan_timeout,push.kind,push.pull_request,push.remote_url,setup" {
		t.Fatalf("the fields the file sets, and nothing of the machine's own: %s", got)
	}
	if raw := string(wsconfig.Canonical(api)); strings.Contains(raw, "/clones") || strings.Contains(raw, "TOKEN") {
		t.Fatalf("paths and token variables stay in the worker's file: %s", raw)
	}
	if web := p.workspaces["fabrikam/web"]; web.Push != nil || web.DefaultBranch != nil || (*web.Checks)[0] != "make test" {
		t.Fatalf("a field the file leaves out is left out: %s", wsconfig.Canonical(web))
	}
	claude, glm := p.runners["claude"], p.runners["glm"]
	if claude.Kind != "claude" || claude.Model != "opus" || claude.Effort != "high" || glm.Kind != "opencode" || glm.Model != "zai-coding-plan/glm-5.3" {
		t.Fatalf("runner definitions from the accounts: %+v %+v", claude, glm)
	}
	if len(p.runners) != 2 {
		t.Fatalf("an account that names a kind defines no runner: %+v", p.runners)
	}

	// A second worker that disagrees about one workspace and one runner.
	b := filepath.Join(dir, "b.toml")
	other := strings.Replace(importWorkerA, `checks = ["npm test"]`, `checks = ["npm run test:ci"]`, 1)
	other = strings.Replace(other, `model = "opus"`, `model = "sonnet"`, 1)
	if err := os.WriteFile(b, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = proposeImport([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.conflicts) != 2 || !strings.Contains(strings.Join(p.conflicts, "\n"), "workspace fabrikam/api") || !strings.Contains(strings.Join(p.conflicts, "\n"), "differ in checks") ||
		!strings.Contains(strings.Join(p.conflicts, "\n"), "runner claude") {
		t.Fatalf("conflicts: %v", p.conflicts)
	}
	if _, ok := p.workspaces["fabrikam/api"]; ok {
		t.Fatal("a workspace the files disagree about is not proposed")
	}
	if _, ok := p.runners["claude"]; ok {
		t.Fatal("a runner the files disagree about is not proposed")
	}
	if _, ok := p.workspaces["fabrikam/web"]; !ok || p.runners["glm"].Kind != "opencode" {
		t.Fatal("what the files agree about is proposed")
	}
}

// The commit recorded with an apply is the file's checkout's, marked when
// the file differs from it.
func TestFileCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	if got := fileCommit(filepath.Join(dir, "fleet.toml")); got != "" {
		t.Fatalf("outside a checkout: %q", got)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	file := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(file, []byte("# fleet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("add", "fleet.toml")
	run("commit", "-q", "-m", "fleet")
	clean := fileCommit(file)
	if len(clean) != 40 {
		t.Fatalf("a committed file: %q", clean)
	}
	if err := os.WriteFile(file, []byte("# fleet, edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := fileCommit(file); got != clean+"+modified" {
		t.Fatalf("an edited file is marked: %q", got)
	}
}

// A stage's block is one field: --set takes it as JSON, --unset drops it,
// and it round-trips through the TOML `show --toml` prints.
func TestEditConfigStageBlocks(t *testing.T) {
	c, err := editConfig(wsconfig.Config{}, []string{
		`stages.implement={"tools":["Bash(npm run:*)"],"env":{"NODE_ENV":"test"}}`,
		`stages.review={"wrap":[{"kind":"opencode","wrapper":"opencode-bwrap"}]}`,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Stages["implement"].Env["NODE_ENV"] != "test" || c.Stages["review"].Wrap[0].Wrapper != "opencode-bwrap" {
		t.Fatalf("set: %s", wsconfig.Canonical(c))
	}
	if _, err := editConfig(c, []string{"stages.implement=npm"}, nil); err == nil {
		t.Fatal("a stage block given plain text must be refused")
	}
	if _, err := editConfig(c, []string{`stages.deploy={}`}, nil); err == nil || !strings.Contains(err.Error(), "not a config field") {
		t.Fatalf("an unknown stage: %v", err)
	}
	norm, err := wsconfig.Normalize(c)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(norm); err != nil {
		t.Fatal(err)
	}
	var back wsconfig.Config
	if md, err := toml.Decode(buf.String(), &back); err != nil || len(md.Undecoded()) > 0 {
		t.Fatalf("decode: %v %v\n%s", err, md.Undecoded(), buf.String())
	}
	if string(wsconfig.Canonical(back)) != string(wsconfig.Canonical(norm)) {
		t.Fatalf("round trip:\n%s\n%s\n--- toml ---\n%s", wsconfig.Canonical(back), wsconfig.Canonical(norm), buf.String())
	}
	c, err = editConfig(c, nil, []string{"stages.review"})
	if err != nil || c.Stages["review"] != nil || c.Stages["implement"] == nil {
		t.Fatalf("unset: %s %v", wsconfig.Canonical(c), err)
	}
}

// The import carries what a worker file's profile gives each stage — tools,
// env, settings — and leaves what contains the stage (a wrapper, the
// sandbox block) in the file.
func TestProposeImportStageBlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.toml")
	text := `[worker]
id = "worker-knot"

[worker.profiles.shop.implement]
wrapper = ["env", "SKOPOS_CONTAINED_BY=knot"]
tools = ["Bash(./cli_tools/dothis:*)", "Bash(mago:*)"]
env = { CODECEPTION_ENV = "local", CACHE = "${CACHE_DIR}/shop" }

[worker.profiles.shop.review]
settings = '{"sandbox":{"enabled":true},"permissions":{"allow":["Bash(mago:*)"]}}'

[worker.profiles.shop.plan]
settings = '{"sandbox":{"enabled":true}}'

[worker.repos."globex/shop"]
path = "/srv/clones/shop"
profile = "shop"

[worker.repos."globex/shop".stages.qa]
tools = ["Bash(npm run:*)"]
wrapper = ["bwrap", "{dir}"]

[[worker.accounts]]
name = "c1"
runner = "claude"
`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	prop, err := proposeImport([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := prop.workspaces["globex/shop"]
	if !ok {
		t.Fatalf("the workspace was not proposed: %+v", prop)
	}
	impl, review, qa := c.Stages["implement"], c.Stages["review"], c.Stages["qa"]
	if impl == nil || len(impl.Tools) != 2 || impl.Env["CODECEPTION_ENV"] != "local" || impl.Env["CACHE"] != "${CACHE_DIR}/shop" {
		t.Fatalf("implement: %+v", impl)
	}
	// The sandbox block stays the worker's; the allow rule moves.
	if review == nil || review.Settings != `{"permissions":{"allow":["Bash(mago:*)"]}}` {
		t.Fatalf("review: %+v", review)
	}
	// The repo's own stage block wins over its profile's, and its wrapper
	// stays in the file.
	if qa == nil || len(qa.Tools) != 1 || len(qa.Wrap) != 0 {
		t.Fatalf("qa: %+v", qa)
	}
	// A stage whose profile only contains it gives nothing to import.
	if _, ok := c.Stages["plan"]; ok {
		t.Fatalf("plan has nothing to import: %+v", c.Stages["plan"])
	}
}

// A misspelt key inside a stage's block is refused, not dropped: dropped,
// it would save an empty block or a wrap rule that matches every runner.
func TestEditConfigRefusesUnknownKeysInAStageBlock(t *testing.T) {
	for _, set := range []string{
		`stages.implement={"tool":["Bash(npm run:*)"]}`,
		`stages.review={"wrap":[{"knd":"opencode","wrapper":"box"}]}`,
		`stages.review={"wrap":[{"knd":"claude","wrapper":"none"},{"wrapper":"box"}]}`,
		`stages.plan={"env":{"A":"1"},"enviroment":{}}`,
	} {
		if _, err := editConfig(wsconfig.Config{}, []string{set}, nil); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s: %v", set, err)
		}
	}
	// Settings as an object, the way one writes them.
	c, err := editConfig(wsconfig.Config{}, []string{`stages.review={"settings":{"permissions":{"allow":["Bash(mago:*)"]}}}`}, nil)
	if err != nil || c.Stages["review"].Settings != `{"permissions":{"allow":["Bash(mago:*)"]}}` {
		t.Fatalf("settings as an object: %+v %v", c.Stages["review"], err)
	}
}
