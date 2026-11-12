package wsconfig

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/policy"
)

func stages(m map[string]*StageCaps) Config { return Config{Stages: m} }

func TestNormalizeStage(t *testing.T) {
	bad := map[string]Config{
		"unknown stage":        stages(map[string]*StageCaps{"deploy": {Tools: []string{"Bash(x:*)"}}}),
		"proxy variable":       stages(map[string]*StageCaps{"implement": {Env: map[string]string{"https_proxy": "http://x"}}}),
		"endpoint variable":    stages(map[string]*StageCaps{"implement": {Env: map[string]string{"ANTHROPIC_BASE_URL": "http://x"}}}),
		"login variable":       stages(map[string]*StageCaps{"review": {Env: map[string]string{"CLAUDE_CONFIG_DIR": "/x"}}}),
		"not a variable name":  stages(map[string]*StageCaps{"review": {Env: map[string]string{"A B": "1"}}}),
		"bad reference":        stages(map[string]*StageCaps{"review": {Env: map[string]string{"A": "${not a name}"}}}),
		"settings not json":    stages(map[string]*StageCaps{"plan": {Settings: "{"}}),
		"settings hooks":       stages(map[string]*StageCaps{"plan": {Settings: `{"hooks":{"x":[]}}`}}),
		"settings env":         stages(map[string]*StageCaps{"plan": {Settings: `{"env":{"ANTHROPIC_BASE_URL":"http://x"}}`}}),
		"permissions other":    stages(map[string]*StageCaps{"plan": {Settings: `{"permissions":{"defaultMode":"bypassPermissions"}}`}}),
		"permissions not list": stages(map[string]*StageCaps{"plan": {Settings: `{"permissions":{"allow":"Bash"}}`}}),
		"rule without wrapper": stages(map[string]*StageCaps{"qa": {Wrap: []WrapRule{{Kind: "opencode"}}}}),
		"rule bad wrapper":     stages(map[string]*StageCaps{"qa": {Wrap: []WrapRule{{Wrapper: "Not A Name"}}}}),
		"multi-line tool":      stages(map[string]*StageCaps{"qa": {Tools: []string{"Bash(a:*)\nBash(*)"}}}),
	}
	for name, c := range bad {
		if _, err := Normalize(c); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	norm, err := Normalize(stages(map[string]*StageCaps{
		" Implement ": {Tools: []string{" Bash(npm run:*) ", "", "Bash(npm run:*)"}, Env: map[string]string{"NODE_ENV": "test", "CACHE": "${CACHE_DIR}/npm"},
			Settings: ` {"permissions": {"allow": ["Read(/opt/**)"]}, "sandbox": {"enabled": true}} `,
			Wrap:     []WrapRule{{Kind: " OpenCode ", Wrapper: "opencode-bwrap"}, {Wrapper: NoWrapper}}},
		"review": nil,
	}))
	if err != nil {
		t.Fatal(err)
	}
	impl := norm.Stages["implement"]
	if impl == nil || len(impl.Tools) != 1 || impl.Tools[0] != "Bash(npm run:*)" {
		t.Fatalf("tools: %+v", impl)
	}
	if impl.Settings != `{"permissions":{"allow":["Read(/opt/**)"]},"sandbox":{"enabled":true}}` {
		t.Fatalf("settings: %s", impl.Settings)
	}
	if impl.Wrap[0].Kind != "opencode" {
		t.Fatalf("rule: %+v", impl.Wrap[0])
	}
	// A nil block is a block: the stage is set to nothing.
	if norm.Stages["review"] == nil {
		t.Fatal("a nil block was dropped")
	}
	if got := norm.FieldNames(); strings.Join(got, ",") != "stages.implement,stages.review" {
		t.Fatalf("fields: %v", got)
	}
	if !IsCommandField("stages.implement") || IsCommandField("mcp_deny") {
		t.Fatal("stage blocks are command fields")
	}
	if !impl.Grants() || (&StageCaps{Settings: `{"sandbox":{"enabled":true}}`}).Grants() || (&StageCaps{Wrap: []WrapRule{{Wrapper: "w"}}}).Grants() {
		t.Fatal("what counts as a grant")
	}
}

func TestStageFieldsRoundTrip(t *testing.T) {
	c := stages(map[string]*StageCaps{"implement": {Tools: []string{"Bash(a:*)"}}, "qa": {}})
	c.DefaultBranch = new("main")
	back, err := FromFields(c.Fields())
	if err != nil {
		t.Fatal(err)
	}
	if string(Canonical(back)) != string(Canonical(c)) {
		t.Fatalf("round trip:\n%s\n%s", Canonical(back), Canonical(c))
	}
	if string(c.Fields()["stages.qa"]) != "{}" {
		t.Fatalf("an empty block is %s", c.Fields()["stages.qa"])
	}
	// A block replaces the one under it whole.
	base := stages(map[string]*StageCaps{"implement": {Tools: []string{"Bash(a:*)"}, Env: map[string]string{"A": "1"}}, "review": {Tools: []string{"Bash(r:*)"}}})
	over := Overlay(base, stages(map[string]*StageCaps{"implement": {Tools: []string{"Bash(b:*)"}}}))
	if got := over.Stages["implement"]; len(got.Tools) != 1 || got.Tools[0] != "Bash(b:*)" || len(got.Env) != 0 {
		t.Fatalf("a block was merged: %+v", got)
	}
	if over.Stages["review"] == nil {
		t.Fatal("a stage only the base sets was lost")
	}
}

func TestWorkspaceStageBlockReplacesTheGroups(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := root()
	if _, err := s.Set(ctx, KindGroup, "globex", stages(map[string]*StageCaps{
		"implement": {Tools: []string{"Bash(group:*)"}, Env: map[string]string{"G": "1"}},
		"review":    {Tools: []string{"Bash(review:*)"}},
	}), "cli", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, KindWorkspace, "fx/api", stages(map[string]*StageCaps{"implement": {Tools: []string{"Bash(ws:*)"}}}), "cli", ""); err != nil {
		t.Fatal(err)
	}
	eff, err := s.Effective(ctx, "fx/api")
	if err != nil {
		t.Fatal(err)
	}
	impl, review := eff.Config.Stages["implement"], eff.Config.Stages["review"]
	if impl == nil || len(impl.Tools) != 1 || impl.Tools[0] != "Bash(ws:*)" || len(impl.Env) != 0 {
		t.Fatalf("implement is not the workspace's block alone: %+v", impl)
	}
	if review == nil || review.Tools[0] != "Bash(review:*)" {
		t.Fatalf("review should be the group's: %+v", review)
	}
	if eff.Sources["stages.implement"] != "workspace" || eff.Sources["stages.review"] != "group:globex" {
		t.Fatalf("sources: %v", eff.Sources)
	}
	// An empty block undoes the group's for that stage.
	if _, err := s.Set(ctx, KindWorkspace, "fx/api", stages(map[string]*StageCaps{"review": {}}), "cli", ""); err != nil {
		t.Fatal(err)
	}
	eff, _ = s.Effective(ctx, "fx/api")
	if r := eff.Config.Stages["review"]; r == nil || len(r.Tools) != 0 {
		t.Fatalf("review should be set to nothing: %+v", r)
	}
	// Two groups setting the same stage for a shared workspace conflict.
	_, err = s.Set(ctx, KindGroup, "web-team", stages(map[string]*StageCaps{"implement": {Tools: []string{"Bash(x:*)"}}}), "cli", "")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second group setting stages.implement: %v", err)
	}
}

func TestStageBlocksAreAuditedInFull(t *testing.T) {
	s, _, sink := fixture(t)
	if _, err := s.Set(root(), KindWorkspace, "fx/api", stages(map[string]*StageCaps{"implement": {Tools: []string{"Bash(make deploy:*)"}}}), "cli", ""); err != nil {
		t.Fatal(err)
	}
	if e := sink.last(); e.Action != "config.update" || !strings.Contains(e.Notes, "stages.implement = ") || !strings.Contains(e.Notes, "Bash(make deploy:*)") {
		t.Fatalf("audit: %+v", e)
	}
}

func TestNormalizeWrapper(t *testing.T) {
	bad := map[string]WrapperDef{
		"no argv":             {Name: "w"},
		"bad name":            {Name: "Bad Name", Argv: []string{"bwrap"}},
		"reserved name":       {Name: NoWrapper, Argv: []string{"bwrap"}},
		"env only":            {Name: "w", Argv: []string{"env", "SKOPOS_CONTAINED_BY=knot"}},
		"env options only":    {Name: "w", Argv: []string{"/usr/bin/env", "-i", "A=b"}},
		"unknown placeholder": {Name: "w", Argv: []string{"bwrap", "--bind", "{home}", "{home}"}},
		"bad reference":       {Name: "w", Argv: []string{"bwrap", "${bad name}"}},
		"empty command":       {Name: "w", Argv: []string{" ", "x"}},
		"check placeholder":   {Name: "w", Argv: []string{"bwrap"}, Check: []string{"bwrap", "{dir}"}},
	}
	for name, d := range bad {
		if _, err := NormalizeWrapper(d); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	d, err := NormalizeWrapper(WrapperDef{Name: "opencode-bwrap", Argv: []string{"env", "A=b", "bwrap", "--bind", "{dir}", "{dir}", "--ro-bind", "${TOOLS}", "/tools"}, Check: []string{"bwrap", "--ro-bind", "/", "/", "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.Executor(), "wrapper:opencode-bwrap:") || !policy.Isolated(d.Executor()) {
		t.Fatalf("executor %s", d.Executor())
	}
	other := d
	other.Argv = append([]string{}, d.Argv...)
	other.Argv[2] = "bwrap2"
	if other.Executor() == d.Executor() {
		t.Fatal("the executor id does not follow the argv")
	}
}

func TestExpandAndWrapRules(t *testing.T) {
	if v, err := Expand("${A}/x/${B}", map[string]string{"A": "/srv", "B": "c"}); err != nil || v != "/srv/x/c" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := Expand("${A}/${MISSING}", map[string]string{"A": "/srv"}); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("a missing variable: %v", err)
	}
	if v, err := Expand("plain $HOME", nil); err != nil || v != "plain $HOME" {
		t.Fatalf("only ${NAME} is a reference: %q %v", v, err)
	}
	block := &StageCaps{Wrap: []WrapRule{
		{Kind: "claude", Wrapper: NoWrapper},
		{Kind: "opencode", Provider: "zai", Wrapper: "glm-box"},
		{Kind: "opencode", Wrapper: "opencode-bwrap"},
		{Wrapper: "catch-all"},
	}}
	cases := map[string]policy.Facts{
		"":               {Name: "claude", Kind: "claude", Provider: "anthropic"},
		"glm-box":        {Name: "opencode-glm", Kind: "opencode", Provider: "zai"},
		"opencode-bwrap": {Name: "opencode-qwen", Kind: "opencode", Provider: "local"},
		"catch-all":      {Name: "codex", Kind: "codex", Provider: "openai"},
	}
	for want, f := range cases {
		if got := block.WrapperFor(f); got != want {
			t.Errorf("%s: wrapper %q, want %q", f.Name, got, want)
		}
	}
	if got := strings.Join(block.Wrappers(), ","); got != "catch-all,glm-box,opencode-bwrap" {
		t.Fatalf("wrappers named: %s", got)
	}
}

func TestWrapperDefinitions(t *testing.T) {
	s, _, sink := fixture(t)
	def := WrapperDef{Name: "opencode-bwrap", Argv: []string{"bwrap", "--bind", "{dir}", "{dir}"}, Check: []string{"bwrap", "--version"}}
	// Fleet admins only.
	if _, _, err := s.SetWrapper(key(true, false, "fx/api"), def, "cli"); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a workspace admin defined a wrapper: %v", err)
	}
	// A stage block cannot name a wrapper that is not defined.
	use := stages(map[string]*StageCaps{"review": {Wrap: []WrapRule{{Kind: "opencode", Wrapper: "opencode-bwrap"}}}})
	if _, err := s.Set(root(), KindWorkspace, "fx/api", use, "cli", ""); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("an undefined wrapper was accepted: %v", err)
	}
	before, _ := s.Fleet(root())
	v, changed, err := s.SetWrapper(key(true, true), def, "cli")
	if err != nil || !changed {
		t.Fatal(err, changed)
	}
	if e := sink.last(); e.EntityType != WrapperAuditEntity || e.EntityID != "opencode-bwrap" || e.Action != "wrapper.create" || !strings.Contains(e.Notes, `"--bind"`) || !strings.Contains(e.Notes, v.Executor) {
		t.Fatalf("audit: %+v", e)
	}
	if _, changed, _ := s.SetWrapper(root(), def, "cli"); changed {
		t.Fatal("the same definition was a change")
	}
	after, err := s.Fleet(root())
	if err != nil || len(after.Wrappers) != 1 || after.Rev == before.Rev || after.Rev == "" {
		t.Fatalf("the fleet config: %+v (was %q) %v", after, before.Rev, err)
	}
	if action, err := s.PlanWrapper(root(), WrapperDef{Name: "opencode-bwrap", Argv: []string{"bwrap", "--unshare-all"}}); err != nil || action != "update" {
		t.Fatalf("plan: %q %v", action, err)
	}
	if _, err := s.Set(root(), KindWorkspace, "fx/api", use, "cli", ""); err != nil {
		t.Fatal(err)
	}
	users, err := s.WrapperUsers(root())
	if err != nil || strings.Join(users["opencode-bwrap"], ",") != "workspace:fx/api" {
		t.Fatalf("users: %v %v", users, err)
	}
	// A wrapper stage blocks name is not deleted by accident.
	if _, err := s.DeleteWrapper(root(), "opencode-bwrap", "cli", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting a wrapper in use: %v", err)
	}
	if _, err := s.DeleteWrapper(root(), "opencode-bwrap", "cli", true); err != nil {
		t.Fatal(err)
	}
	if e := sink.last(); e.Action != "wrapper.delete" || !strings.Contains(e.Notes, "workspace:fx/api") {
		t.Fatalf("audit: %+v", e)
	}
	if _, err := s.DeleteWrapper(root(), "opencode-bwrap", "cli", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice: %v", err)
	}
	// A document that states the full set is checked against that set.
	ctx := WithWrapperSet(root(), []string{"other"})
	other := stages(map[string]*StageCaps{"review": {Wrap: []WrapRule{{Wrapper: "other"}}}})
	if _, err := s.Plan(ctx, KindWorkspace, "fx/api", &other); err != nil {
		t.Fatalf("a wrapper the document defines: %v", err)
	}
	if _, err := s.Plan(root(), KindWorkspace, "fx/api", &other); !errors.Is(err, ErrInvalid) {
		t.Fatalf("without the document's set: %v", err)
	}
}

func TestEnvOnlyWrapper(t *testing.T) {
	cases := []struct {
		argv []string
		by   string
		ok   bool
	}{
		{[]string{"env", "SKOPOS_CONTAINED_BY=knot"}, "knot", true},
		{[]string{"/usr/bin/env", "-i", "A=b"}, "", true},
		{[]string{"env", "-u", "X", "A=b"}, "", true},
		{[]string{"env", "-S", "bwrap x"}, "", true},
		{[]string{"env", "A=b", "bwrap"}, "", false},
		{[]string{"bwrap"}, "", false},
		{nil, "", false},
	}
	for _, c := range cases {
		by, ok := EnvOnlyWrapper(c.argv)
		if by != c.by || ok != c.ok {
			t.Errorf("%v: %q %v, want %q %v", c.argv, by, ok, c.by, c.ok)
		}
	}
	if got, _ := json.Marshal(WrapperDef{Name: "w", Argv: []string{"x"}}); string(got) != `{"name":"w","argv":["x"]}` {
		t.Fatalf("stored form: %s", got)
	}
}

// A stage's env is for the repository's own tools: what would change the
// agent's model, login, provider, credentials or what its processes load is
// refused — in the block, and in a wrapper's leading `env K=V`.
func TestStageEnvRefusals(t *testing.T) {
	refused := []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_MODEL", "anthropic_model",
		"ANTHROPIC_CUSTOM_HEADERS", "AWS_PROFILE", "AWS_BEARER_TOKEN_BEDROCK", "OPENCODE_CONFIG_DIR", "CODEX_HOME", "GOOGLE_APPLICATION_CREDENTIALS",
		"NODE_OPTIONS", "NODE_EXTRA_CA_CERTS", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "SSH_AUTH_SOCK", "GIT_ASKPASS", "GIT_SSH_COMMAND",
		"GIT_CONFIG_GLOBAL", "NO_PROXY", "no_proxy", "HTTPS_PROXY", "HOME", "home", "APP_KEY", "DB_PASSWORD", "MY_SECRET", "BASH_ENV",
	}
	for _, name := range refused {
		if StageEnvRefused(name) == "" {
			t.Errorf("%s is accepted in a stage's env", name)
		}
		if _, err := NormalizeWrapper(WrapperDef{Name: "w", Argv: []string{"env", "-i", name + "=x", "bwrap", "{dir}"}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s is accepted in a wrapper's env prefix (%v)", name, err)
		}
	}
	for _, name := range []string{"CODECEPTION_ENV", "NODE_ENV", "APP_ENV", "PATH", "CACHE_DIR", "ENV", "LANG", "COMPOSER_HOME", "TZ"} {
		if why := StageEnvRefused(name); why != "" {
			t.Errorf("%s is refused: %s", name, why)
		}
	}
	if _, err := NormalizeWrapper(WrapperDef{Name: "w", Argv: []string{"env", "-u", "HOME", "LANG=C", "bwrap", "--setenv", "X", "1", "{dir}"}}); err != nil {
		t.Fatalf("an ordinary env prefix: %v", err)
	}
}

// Settings read as a JSON object or as a string holding one, are written
// as an object, and come out the same either way.
func TestSettingsForms(t *testing.T) {
	var asObject, asString Config
	if err := json.Unmarshal([]byte(`{"stages":{"review":{"settings":{"permissions":{"allow":["Bash(mago:*)"]}}}}}`), &asObject); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"stages":{"review":{"settings":"{ \"permissions\": {\"allow\": [\"Bash(mago:*)\"]} }"}}}`), &asString); err != nil {
		t.Fatal(err)
	}
	a, err := Normalize(asObject)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Normalize(asString)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"stages":{"review":{"settings":{"permissions":{"allow":["Bash(mago:*)"]}}}}}`
	if string(Canonical(a)) != want || string(Canonical(b)) != want {
		t.Fatalf("stored form:\n%s\n%s", Canonical(a), Canonical(b))
	}
	// Through the flattened fields and back.
	back, err := FromFields(a.Fields())
	if err != nil || string(Canonical(back)) != want {
		t.Fatalf("round trip: %s %v", Canonical(back), err)
	}
	if err := json.Unmarshal([]byte(`{"stages":{"review":{"settings":["x"]}}}`), &asObject); err == nil {
		t.Fatal("settings that are neither an object nor a string were read")
	}
	if _, err := Normalize(stages(map[string]*StageCaps{"review": {Settings: `"just text"`}})); !errors.Is(err, ErrInvalid) {
		t.Fatalf("settings that are not an object: %v", err)
	}
}
