package policy

import (
	"errors"
	"reflect"
	"testing"
)

func layer(source, rev string, rules ...Rule) Layer {
	return Layer{Source: source, Rev: rev, Policy: Policy{Allow: rules}}
}

var (
	claude    = Facts{Name: "claude", Kind: "claude", Model: "claude-opus-4-5", Provider: "anthropic", Endpoint: "api.anthropic.com"}
	glm       = Facts{Name: "opencode-glm", Kind: "opencode", Model: "zai-coding-plan/glm-5.3", Provider: "zai", Endpoint: "opencode:zai-coding-plan"}
	localLLM  = Facts{Name: "opencode-local", Kind: "opencode", Model: "ollama/qwen3", Provider: "local", Endpoint: "localhost"}
	codex     = Facts{Name: "codex", Kind: "codex", Model: "gpt-5.2", Provider: "openai", Endpoint: "api.openai.com", Stages: []string{"plan", "review", "compare", "second_opinion"}}
	viaRouter = Facts{Name: "opencode-qwen", Kind: "opencode", Model: "router/qwen3-coder", Provider: "unknown:llm.lan", Endpoint: "llm.lan"}
)

// The design's example: the globex group allows Anthropic, a local model,
// and OpenAI for second opinions only — never GLM, whatever CLI runs it.
func TestRuleMatching(t *testing.T) {
	globex := &Effective{Layers: []Layer{layer("group globex", "r1",
		Rule{Provider: "anthropic"},
		Rule{Provider: "openai", Stages: []string{"second_opinion"}},
		Rule{Provider: "local"},
	)}}
	cases := []struct {
		name  string
		facts Facts
		stage string
		want  bool
	}{
		{"claude implements", claude, "implement", true},
		{"claude reviews", claude, "review", true},
		{"opencode with a local model", localLLM, "review", true},
		{"opencode with GLM", glm, "review", false},
		{"codex second opinion", codex, "second_opinion", true},
		{"codex review", codex, "review", false},
		{"a router nobody declared", viaRouter, "review", false},
	}
	for _, c := range cases {
		if got := globex.Allows(c.facts, c.stage); got.Allowed != c.want {
			t.Errorf("%s: allowed = %v, want %v (%+v)", c.name, got.Allowed, c.want, got)
		}
	}
	// The decision names the rule that allowed it, and the layer that
	// refused.
	if d := globex.Allows(codex, "second_opinion"); !reflect.DeepEqual(d.Rules, []int{1}) {
		t.Fatalf("matched rule: %+v", d)
	}
	if d := globex.Allows(glm, "review"); d.Refused != "group globex" {
		t.Fatalf("refusing layer: %+v", d)
	}
}

// Model, kind and runner fields, and what an unset provider means.
func TestRuleFields(t *testing.T) {
	for _, c := range []struct {
		rule  Rule
		facts Facts
		want  bool
	}{
		{Rule{Model: "claude-opus-*"}, claude, true},
		{Rule{Model: "claude-sonnet-*"}, claude, false},
		{Rule{Model: "*glm*"}, glm, true}, // * crosses the provider/model slash
		{Rule{Model: "glm-*"}, glm, false},
		{Rule{Kind: "opencode"}, glm, true},
		{Rule{Kind: "opencode"}, claude, false},
		{Rule{Runner: "opencode-*"}, glm, true},
		{Rule{Runner: "CLAUDE"}, claude, true}, // case does not matter
		{Rule{Provider: "router", Model: "router/qwen*"}, Facts{Provider: "router", Model: "router/qwen3-coder"}, true},
		{Rule{Provider: "router", Model: "router/qwen*"}, Facts{Provider: "router", Model: "router/gpt-5"}, false},
		// No provider set: any provider, an unknown one included.
		{Rule{Kind: "opencode"}, viaRouter, true},
		{Rule{Provider: "unknown:llm.lan"}, viaRouter, true},
		{Rule{Provider: "unknown:*"}, viaRouter, true},
		{Rule{Provider: "anthropic"}, viaRouter, false},
		{Rule{}, glm, true}, // "any"
	} {
		if got := c.rule.Matches(c.facts, "review"); got != c.want {
			t.Errorf("%s vs %+v = %v, want %v", c.rule, c.facts, got, c.want)
		}
	}
}

// Every layer must allow the runner: a ceiling narrows whatever is below
// it, and a workspace narrows its group.
func TestLayersIntersect(t *testing.T) {
	e := &Effective{Layers: []Layer{
		layer("fleet ceiling", "c1", Rule{Provider: "anthropic"}, Rule{Provider: "zai"}),
		layer("group personal", "g1", Rule{Provider: "anthropic"}, Rule{Provider: "zai"}, Rule{Provider: "openai"}),
		layer("workspace", "w1", Rule{Provider: "anthropic"}),
	}}
	if !e.Allows(claude, "plan").Allowed {
		t.Fatal("claude passes every layer")
	}
	if d := e.Allows(glm, "plan"); d.Allowed || d.Refused != "workspace" {
		t.Fatalf("the workspace narrows its group: %+v", d)
	}
	if d := e.Allows(codex, "plan"); d.Allowed || d.Refused != "fleet ceiling" {
		t.Fatalf("the ceiling refuses what a group would allow: %+v", d)
	}
	if e.AllowAll() || e.Rev() == "" || len(e.Revs()) != 3 {
		t.Fatal("a policy with layers is not allow-all and has a revision")
	}
	other := &Effective{Layers: []Layer{e.Layers[0], e.Layers[1], layer("workspace", "w2", Rule{Provider: "anthropic"})}}
	if other.Rev() == e.Rev() {
		t.Fatal("a changed layer changes the effective revision")
	}

	var none *Effective
	if !none.AllowAll() || !none.Allows(glm, "implement").Allowed || none.Rev() != "" {
		t.Fatal("no policy allows everything")
	}
	if !(&Effective{}).Allows(viaRouter, "review").Allowed {
		t.Fatal("no layers allows everything")
	}
	// A layer with no rules allows nothing.
	if (&Effective{Layers: []Layer{layer("workspace", "w")}}).Allows(claude, "plan").Allowed {
		t.Fatal("an empty rule list allows nothing")
	}
}

func TestNormalizeAndParse(t *testing.T) {
	p, err := Normalize(Policy{Allow: []Rule{{Provider: " Anthropic ", Kind: "Claude", Stages: []string{"review", "plan", "review", ""}}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Rule{Provider: "anthropic", Kind: "claude", Stages: []string{"plan", "review"}}); !reflect.DeepEqual(p.Allow[0], want) {
		t.Fatalf("normalized: %+v", p.Allow[0])
	}
	for name, bad := range map[string]Policy{
		"unknown stage":  {Allow: []Rule{{Stages: []string{"deploy"}}}},
		"unknown kind":   {Allow: []Rule{{Kind: "gemini"}}},
		"space in value": {Allow: []Rule{{Provider: "open ai"}}},
		"too many rules": {Allow: make([]Rule, 65)},
	} {
		if _, err := Normalize(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}

	r, err := ParseRule("provider=openai, stages=second_opinion|review ,model=gpt-*")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Rule{Provider: "openai", Model: "gpt-*", Stages: []string{"second_opinion", "review"}}); !reflect.DeepEqual(r, want) {
		t.Fatalf("parsed: %+v", r)
	}
	if any, err := ParseRule("any"); err != nil || !reflect.DeepEqual(any, Rule{}) {
		t.Fatalf("any: %+v %v", any, err)
	}
	for _, bad := range []string{"", "provider", "provider=", "vendor=x", "provider=anthropic,provider=zai"} {
		if _, err := ParseRule(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: want ErrInvalid, got %v", bad, err)
		}
	}
	// A rule prints the way it is written.
	n, _ := Normalize(Policy{Allow: []Rule{r}})
	if back, err := ParseRule(n.Allow[0].String()); err != nil || !reflect.DeepEqual(mustNorm(t, back), n.Allow[0]) {
		t.Fatalf("round trip: %q → %+v %v", n.Allow[0].String(), back, err)
	}
	if (Rule{}).String() != "any" {
		t.Fatal("an empty rule prints as any")
	}
}

func mustNorm(t *testing.T, r Rule) Rule {
	t.Helper()
	p, err := Normalize(Policy{Allow: []Rule{r}})
	if err != nil {
		t.Fatal(err)
	}
	return p.Allow[0]
}

func TestFactsCanRun(t *testing.T) {
	if !claude.CanRun("implement") || codex.CanRun("implement") || !codex.CanRun("review") {
		t.Fatal("a runner with no stage list runs everything; a read-only one only its stages")
	}
}

func TestProviderForEndpoint(t *testing.T) {
	for endpoint, want := range map[string]string{
		"https://api.anthropic.com":                       "anthropic",
		"api.anthropic.com":                               "anthropic",
		"https://api.openai.com/v1":                       "openai",
		"https://chatgpt.com/backend-api":                 "openai",
		"https://api.z.ai/api/coding/paas/v4":             "zai",
		"https://open.bigmodel.cn/api":                    "zai",
		"https://aiplatform.googleapis.com":               "google",
		"https://bedrock-runtime.us-east-1.amazonaws.com": "aws",
		"https://bedrock.eu-west-1.amazonaws.com":         "aws",
		"https://my-resource.openai.azure.com":            "microsoft",
		"https://my-project.services.ai.azure.com":        "microsoft",
		// A cloud's customer hosts are anyone's, not the cloud's AI service.
		"https://ec2-1-2-3-4.compute.amazonaws.com":                       "unknown:ec2-1-2-3-4.compute.amazonaws.com",
		"https://abc123.execute-api.us-east-1.amazonaws.com":              "unknown:abc123.execute-api.us-east-1.amazonaws.com",
		"https://my-proxy.westeurope.cloudapp.azure.com":                  "unknown:my-proxy.westeurope.cloudapp.azure.com",
		"https://notbedrock-runtime.us-east-1.amazonaws.com.evil.example": "unknown:notbedrock-runtime.us-east-1.amazonaws.com.evil.example",
		"http://localhost:11434/v1":                                       "local",
		"http://127.0.0.1:1234":                                           "local",
		"http://[::1]:8080":                                               "local",
		// A private-network host is not assumed local.
		"http://192.168.1.20:8080/v1": "unknown:192.168.1.20",
		"https://llm.lan/v1":          "unknown:llm.lan",
		// A look-alike domain is not the company.
		"https://anthropic.com.evil.example": "unknown:anthropic.com.evil.example",
		"https://notanthropic.com":           "unknown:notanthropic.com",
		"":                                   "unknown:none",
	} {
		if got := ProviderForEndpoint(endpoint); got != want {
			t.Errorf("%q → %q, want %q", endpoint, got, want)
		}
	}
}

func TestProviderForOpencode(t *testing.T) {
	for _, c := range []struct{ model, baseURL, provider, endpoint string }{
		{"zai-coding-plan/glm-5.3", "", "zai", "opencode:zai-coding-plan"},
		{"anthropic/claude-opus-4-5", "", "anthropic", "opencode:anthropic"},
		{"amazon-bedrock/anthropic.claude", "", "aws", "opencode:amazon-bedrock"},
		// A provider whose endpoint the operator chooses needs its baseURL.
		{"ollama/qwen3", "", "unknown:ollama", "opencode:ollama"},
		{"ollama/qwen3", "http://localhost:11434/v1", "local", "localhost"},
		{"router/qwen3-coder", "https://llm.lan/v1", "unknown:llm.lan", "llm.lan"},
		// A baseURL wins over the id: zai's name pointed elsewhere is elsewhere.
		{"zai/glm-5.3", "https://proxy.example/v1", "unknown:proxy.example", "proxy.example"},
		{"", "", "unknown:none", ""},
	} {
		p, e := ProviderForOpencode(c.model, c.baseURL)
		if p != c.provider || e != c.endpoint {
			t.Errorf("%q %q → %q %q, want %q %q", c.model, c.baseURL, p, e, c.provider, c.endpoint)
		}
	}
}

func TestContradicts(t *testing.T) {
	for _, c := range []struct {
		declared, derived string
		want              bool
	}{
		{"local", "zai", true},
		{"anthropic", "local", true},
		{"local", "local", false},
		{"Local", "LOCAL", false},
		{"router", "unknown:llm.lan", false}, // declaring what cannot be derived
		{"local", "unknown:10.0.0.5", false},
		{"", "zai", false},
	} {
		if got := Contradicts(c.declared, c.derived); got != c.want {
			t.Errorf("Contradicts(%q, %q) = %v, want %v", c.declared, c.derived, got, c.want)
		}
	}
}

// The two containment properties: a host that is itself a container is
// "contained" but not "isolated" — inside it a run still sees every other
// clone — while a wrapper and a runner whose own lockdown is a sandbox are
// both. Claude Code's sandbox settings are neither: a rule names them.
func TestExecutorRequirements(t *testing.T) {
	for _, c := range []struct {
		executor            string
		isolated, contained bool
	}{
		{"local", false, false},
		{"", false, false},
		{"host:knot", false, true},
		{"wrapper:opencode-bwrap:3f2a9c1d77aa", true, true},
		{"sandbox:9c1d77aa3f2a", false, false}, // matched by name only (sandbox:*)
		{"runner:codex", true, true},
		{"wrapperish", false, false},
	} {
		if got := ExecutorSatisfies(ExecutorIsolated, c.executor); got != c.isolated {
			t.Errorf("%q isolated = %v, want %v", c.executor, got, c.isolated)
		}
		if got := ExecutorSatisfies(ExecutorContained, c.executor); got != c.contained {
			t.Errorf("%q contained = %v, want %v", c.executor, got, c.contained)
		}
		if !ExecutorSatisfies("", c.executor) {
			t.Errorf("no requirement accepts %q", c.executor)
		}
	}
	// Naming an executor: a glob on its id; never an unreported one.
	for _, c := range []struct {
		req, executor string
		want          bool
	}{
		{"wrapper:opencode-bwrap:*", "wrapper:opencode-bwrap:3f2a9c1d77aa", true},
		{"wrapper:opencode-bwrap:*", "wrapper:other:3f2a9c1d77aa", false},
		{"wrapper:*", "sandbox:9c1d77aa3f2a", false},
		{"host:knot", "host:knot", true},
		{"host:*", "host:knot", true},
		{"runner:codex", "runner:codex", true},
		{"local", "local", true},
		{"host:*", "", false},
	} {
		if got := ExecutorSatisfies(c.req, c.executor); got != c.want {
			t.Errorf("%q vs %q = %v, want %v", c.req, c.executor, got, c.want)
		}
	}
}

// The design's example: GLM only where something confines that run. A
// worker whose whole host is a container does not pass; a wrapped run
// does; a rule without the field is unaffected; a worker that reports no
// executor never passes a rule that asks for one.
func TestExecutorRule(t *testing.T) {
	personal := &Effective{Layers: []Layer{layer("group personal", "r1",
		Rule{Provider: "anthropic"},
		Rule{Provider: "zai", Executor: "isolated"},
		Rule{Provider: "openai", Executor: "contained"},
	)}}
	with := func(f Facts, executor string) Facts { f.Executor = executor; return f }
	for _, c := range []struct {
		name  string
		facts Facts
		want  bool
	}{
		{"claude, local", with(claude, "local"), true},
		{"claude, nothing reported", claude, true},
		{"GLM, local", with(glm, "local"), false},
		{"GLM, nothing reported", glm, false},
		{"GLM on a host that is a container", with(glm, "host:knot"), false},
		{"GLM under a wrapper", with(glm, "wrapper:opencode-bwrap:3f2a9c1d77aa"), true},
		{"codex in its own sandbox", with(codex, "runner:codex"), true},
		{"codex on a contained host", with(codex, "host:knot"), true},
		{"codex, local", with(codex, "local"), false},
	} {
		if got := personal.Allows(c.facts, "review").Allowed; got != c.want {
			t.Errorf("%s: allowed = %v, want %v", c.name, got, c.want)
		}
	}

	p, err := Normalize(Policy{Allow: []Rule{{Provider: "zai", Executor: " Isolated "}}})
	if err != nil || p.Allow[0].Executor != "isolated" {
		t.Fatalf("normalized: %+v %v", p, err)
	}
	if _, err := Normalize(Policy{Allow: []Rule{{Executor: "jail"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown executor requirement: %v", err)
	}
	r, err := ParseRule("provider=zai,executor=wrapper:opencode-bwrap:*")
	if err != nil || r.Executor != "wrapper:opencode-bwrap:*" || r.String() != "provider=zai,executor=wrapper:opencode-bwrap:*" {
		t.Fatalf("parse and print: %+v %q %v", r, r.String(), err)
	}
}

// The most specific executor fact wins: stage and runner, then stage, then
// runner, then the workspace's default.
func TestResolveExecutor(t *testing.T) {
	facts := []ExecutorFact{
		{Executor: "host:knot"},
		{Runner: "codex", Executor: "runner:codex"},
		{Stage: "implement", Executor: "wrapper:ff3:aaaa"},
		{Stage: "review", Runner: "claude", Executor: "sandbox:bbbb"},
	}
	for _, c := range []struct{ stage, runner, want string }{
		{"plan", "claude", "host:knot"},
		{"plan", "codex", "runner:codex"},
		{"implement", "claude", "wrapper:ff3:aaaa"},
		{"implement", "codex", "wrapper:ff3:aaaa"}, // a stage's wrapper wraps every runner
		{"review", "claude", "sandbox:bbbb"},
		{"review", "codex", "runner:codex"},
		{"review", "opencode-glm", "host:knot"},
	} {
		if got := ResolveExecutor(facts, c.stage, c.runner); got != c.want {
			t.Errorf("%s on %s = %q, want %q", c.runner, c.stage, got, c.want)
		}
	}
	if got := ResolveExecutor(nil, "plan", "claude"); got != "" {
		t.Fatalf("nothing reported: %q", got)
	}
}

// An executor a worker cannot offer (none:…) is a known id that meets no
// requirement, not even the empty one, and a runner with it cannot run.
func TestUnavailableExecutor(t *testing.T) {
	for _, why := range []string{NoneWrapper, NoneUncontained} {
		e := NoExecutor(why)
		if !KnownExecutor(e) || !Unavailable(e) || Isolated(e) || Contained(e) {
			t.Errorf("%s: known %v unavailable %v", e, KnownExecutor(e), Unavailable(e))
		}
		for _, req := range []string{"", ExecutorIsolated, ExecutorContained, "none:*", "*"} {
			if ExecutorSatisfies(req, e) {
				t.Errorf("%s satisfies %q", e, req)
			}
		}
		if (Facts{Name: "claude", Executor: e}).CanRun("plan") {
			t.Errorf("%s: the runner can still run", e)
		}
		if UnavailableReason(e) == "" {
			t.Errorf("%s has no reason", e)
		}
	}
	if Unavailable("local") || Unavailable("") || KnownExecutor("none:") {
		t.Fatal("what counts as unavailable")
	}
	if !(Facts{Name: "claude", Executor: "local"}).CanRun("plan") {
		t.Fatal("a local executor can run")
	}
	if !RoutesRequests("https_proxy") || !RoutesRequests("ANTHROPIC_BASE_URL") || RoutesRequests("NODE_ENV") {
		t.Fatal("which variables route requests")
	}
}
