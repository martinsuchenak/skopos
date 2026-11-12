package routes

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/policy"
	wk "github.com/martinsuchenak/skopos/internal/worker"
)

// knotWorker is a worker config the way a Knot worker's file reads after
// phase 2d: the host is a container, commands come from the server, and
// the file has no profiles.
func knotWorker(t *testing.T, ws string, vars map[string]string) *wk.Config {
	t.Helper()
	cfg := &wk.Config{ID: "worker-w1", StateDir: t.TempDir(), ServerCommands: true, ContainedBy: "knot", Vars: vars,
		Workspaces:  []string{ws},
		Repos:       map[string]*wk.Repo{ws: {Path: t.TempDir()}},
		RunnerSpecs: map[string]*wk.RunnerSpec{"opencode-glm": {Kind: "opencode"}},
		Accounts:    []*wk.Account{{Name: "c", Runner: "claude"}, {Name: "g", Runner: "opencode-glm", Model: "zai-coding-plan/glm-5.3"}},
	}
	if err := cfg.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Stage capabilities and wrappers on the server, end to end through the
// real routes and the worker's own client: a fleet admin defines a wrapper,
// a workspace's admin gives its stages tools and env and attaches the
// wrapper by rule; the worker takes them, fills in its own variables, and
// reports what contains each stage; a worker where the wrapper's check
// fails is handed no run the wrapper is for, and one where it passes gets
// the run with the wrapper's executor recorded on it.
func TestIntegrationStageCapsAndWrappers(t *testing.T) {
	sqlDB, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const ws = "github.com/fortix/freedom3"
	wsPath := "/api/workspaces/" + url.PathEscape(ws)
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`","name":"freedom3"}`), 201)
	mustDo(t, root.req("POST", "/api/groups", `{"name":"fortix","members":["`+ws+`"]}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"martin","workspaces":["`+ws+`"],"approver":true}`)}
	ops := workerClient{base: baseURL, key: mint(t, root, `{"name":"ops","groups":["fortix"],"admin":true}`)}
	workerKey := mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`)
	wc := wk.NewClient(baseURL, workerKey)
	wc.SetServerCommands(true)
	ctx := t.Context()

	// Wrappers: the fleet's, defined by a fleet admin.
	failing := `{"argv":["bwrap","--bind","{dir}","{dir}","--ro-bind","${TOOLS}","/tools"],"check":["false"]}`
	mustDo(t, ops.req("PUT", "/api/fleet/wrappers/opencode-bwrap", failing), 403, "a group admin does not define wrappers")
	mustDo(t, root.req("PUT", "/api/fleet/wrappers/opencode-bwrap", `{"argv":["env","SKOPOS_CONTAINED_BY=knot"]}`), 400, "a wrapper that only sets variables")
	mustDo(t, root.req("PUT", "/api/fleet/wrappers/opencode-bwrap", `{"arg":["bwrap"]}`), 400, "a misspelt field")
	mustDo(t, root.req("PUT", "/api/fleet/wrappers/opencode-bwrap", `{"argv":["bwrap","{home}"]}`), 400, "an unknown placeholder")

	// A stage block cannot name a wrapper before it exists.
	stages := `{"config":{"stages":{
		"plan":{"tools":["Bash(mago:*)"],"env":{"CACHE":"${CACHE_DIR}/ff3"},"wrap":[{"kind":"opencode","wrapper":"opencode-bwrap"}]},
		"implement":{"tools":["Bash(./cli_tools/dothis:*)"],"env":{"CODECEPTION_ENV":"local"}}}}}`
	mustDo(t, ops.req("PUT", wsPath+"/config", stages), 400, "an undefined wrapper")
	first := mustJSON(t, root.req("PUT", "/api/fleet/wrappers/opencode-bwrap", failing), 200).(map[string]any)
	executor, _ := first["executor"].(string)
	if !strings.HasPrefix(executor, "wrapper:opencode-bwrap:") {
		t.Fatalf("the saved wrapper names its executor: %v", first)
	}
	// What a stage block may not carry.
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"config":{"stages":{"plan":{"env":{"ANTHROPIC_BASE_URL":"http://elsewhere"}}}}}`), 400, "an endpoint variable")
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"config":{"stages":{"plan":{"settings":"{\"hooks\":{}}"}}}}`), 400, "settings beyond permissions and sandbox")
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"config":{"stages":{"deploy":{}}}}`), 400, "an unknown stage")
	mustDo(t, approver.req("PUT", wsPath+"/config", stages), 403, "an approver is not an admin")
	mustDo(t, ops.req("PUT", wsPath+"/config", stages), 200)
	mustDo(t, approver.req("PUT", wsPath+"/pipeline", `{"stages":{"plan":{"members":[{"runner":"opencode-glm"}]}}}`), 200)

	// A worker that does not declare the variable the block's env names
	// cannot use the config at all.
	cfg := knotWorker(t, ws, nil)
	problems, _ := wk.CheckServerConfig(ctx, cfg, wc)
	if len(problems) != 1 || !strings.Contains(problems[0], "CACHE_DIR") || !strings.Contains(problems[0], "worker.vars") {
		t.Fatalf("a variable the worker does not declare: %v", problems)
	}

	// With it declared, the worker takes the blocks. The wrapper's check
	// fails here, so it does not offer the GLM plan — and nothing else
	// changes: Claude still plans on the contained host.
	cfg = knotWorker(t, ws, map[string]string{"CACHE_DIR": "/var/cache", "TOOLS": "/opt/tools"})
	problems, notes := wk.CheckServerConfig(ctx, cfg, wc)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if all := strings.Join(notes, "\n"); !strings.Contains(all, "wrapper opencode-bwrap cannot be used on this worker") || !strings.Contains(all, "takes no plan run of it for opencode-glm") {
		t.Fatalf("the check's result is said: %s", all)
	}
	if got := cfg.ExecutorFor(ws, "plan", "opencode-glm"); got != policy.NoExecutor(policy.NoneWrapper) {
		t.Fatalf("a wrapper that does not start: %s", got)
	}
	if got := cfg.ExecutorFor(ws, "plan", "claude"); got != "host:knot" {
		t.Fatalf("claude: %s", got)
	}
	if p := cfg.StageProfile(ws, "plan"); p == nil || p.Env["CACHE"] != "/var/cache/ff3" || p.Tools[0] != "Bash(mago:*)" {
		t.Fatalf("the plan stage on this worker: %+v", p)
	}

	// The claim: the plan is for opencode-glm, which this worker cannot
	// run there.
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"wrapped plan","author_agent_id":"martin"}`), 201).(map[string]any)
	mustDo(t, approver.req("POST", "/api/inbox/"+item["id"].(string)+"/queue", ""), 200)
	runners := []string{"claude", "opencode-glm"}
	wc.SetExecutors(cfg.ExecutorFacts())
	if run, err := wc.Claim(ctx, "worker-w1", runners, []string{ws}, nil, nil, cfg.RunnerFacts()); err != nil || run != nil {
		t.Fatalf("a worker whose wrapper does not start was handed the run: %+v %v", run, err)
	}

	// The wrapper is fixed: its check passes, the worker offers it, and
	// the run records the wrapper's executor.
	passing := strings.Replace(failing, `["false"]`, `["true"]`, 1)
	second := mustJSON(t, root.req("PUT", "/api/fleet/wrappers/opencode-bwrap", passing), 200).(map[string]any)
	if second["executor"] != executor {
		t.Fatalf("the executor follows the argv, not the check: %v, was %s", second["executor"], executor)
	}
	cfg = knotWorker(t, ws, map[string]string{"CACHE_DIR": "/var/cache", "TOOLS": "/opt/tools"})
	if problems, notes := wk.CheckServerConfig(ctx, cfg, wc); len(problems) != 0 || !strings.Contains(strings.Join(notes, "\n"), "wrapper opencode-bwrap starts on this worker") {
		t.Fatalf("after the fix: %v %v", problems, notes)
	}
	if got := cfg.ExecutorFor(ws, "plan", "opencode-glm"); got != executor {
		t.Fatalf("the worker's executor: %s, want %s", got, executor)
	}
	wc.SetExecutors(cfg.ExecutorFacts())
	run, err := wc.Claim(ctx, "worker-w1", runners, []string{ws}, nil, nil, cfg.RunnerFacts())
	if err != nil || run == nil || run.Runner != "opencode-glm" || run.ConfigRev == "" {
		t.Fatalf("the claim: %+v %v", run, err)
	}
	var claimed struct {
		Executor string `json:"executor"`
	}
	if err := json.Unmarshal(run.Facts, &claimed); err != nil || claimed.Executor != executor {
		t.Fatalf("the run records what contains it: %s %v", run.Facts, err)
	}
	// The config the run was claimed under carries the stage blocks.
	at, err := wc.ConfigAt(ctx, ws, run.ConfigRev)
	if err != nil || at.Config.Stages["implement"].Env["CODECEPTION_ENV"] != "local" {
		t.Fatalf("the run's config revision: %+v %v", at, err)
	}

	// A wrapper a stage block names is not deleted by accident.
	mustDo(t, root.req("DELETE", "/api/fleet/wrappers/opencode-bwrap", ""), 409, "a wrapper in use")
	listed := root.req("GET", "/api/fleet/wrappers", "")
	if mustDo(t, listed, 200); !strings.Contains(listed.body, `"used_by":["workspace:`+ws+`"]`) {
		t.Fatalf("who names the wrapper: %s", listed.body)
	}

	// Everything in one document, and back with nothing to do.
	exported := root.req("GET", "/api/config/export", "")
	mustDo(t, exported, 200)
	for _, want := range []string{`"opencode-bwrap"`, `"stages"`, `"CODECEPTION_ENV"`} {
		if !strings.Contains(exported.body, want) {
			t.Fatalf("the export lacks %s: %s", want, exported.body)
		}
	}
	plan := mustJSON(t, root.req("POST", "/api/config/apply", `{"plan":true,"document":`+exported.body+`}`), 200).(map[string]any)
	if changes, _ := plan["changes"].([]any); len(changes) != 0 {
		t.Fatalf("export then apply changes nothing: %v", plan)
	}

	// The trail: the wrapper's argv and the stage blocks, in full.
	var actions, notes2 string
	if err := sqlDB.QueryRow(`SELECT group_concat(action, ' '), group_concat(COALESCE(notes, ''), ' | ') FROM (SELECT * FROM audit_log WHERE entity_type = 'wrapper' AND entity_id = 'opencode-bwrap' ORDER BY id)`).Scan(&actions, &notes2); err != nil {
		t.Fatal(err)
	}
	if actions != "wrapper.create wrapper.update" || !strings.Contains(notes2, `"--ro-bind"`) || !strings.Contains(notes2, executor) {
		t.Fatalf("wrapper audit: %s / %s", actions, notes2)
	}
	var cfgNotes string
	if err := sqlDB.QueryRow(`SELECT group_concat(COALESCE(notes, ''), ' | ') FROM audit_log WHERE entity_type = 'workspace_config' AND entity_id = ?`, "workspace:"+ws).Scan(&cfgNotes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfgNotes, "stages.implement = ") || !strings.Contains(cfgNotes, "Bash(./cli_tools/dothis:*)") {
		t.Fatalf("stage blocks are recorded with their values: %s", cfgNotes)
	}
	mustDo(t, root.req("DELETE", "/api/fleet/wrappers/opencode-bwrap?force=1", ""), 204)
}
