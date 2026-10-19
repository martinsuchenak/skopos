package routes

import (
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/runs"
)

// Second opinion over REST, end to end, on an item blocked by its review
// group (the newest review member passed, an older one failed):
//   - approver-only; one at a time; pinned to the implemented head;
//   - claiming it doesn't move the item; the result is appended under the
//     runner and head from the run record and the status never changes;
//   - the same runner on the same head needs force;
//   - with no runner named, the workspace default from pipeline settings.
func TestIntegrationSecondOpinionRoundTrip(t *testing.T) {
	const ws = "github.com/fortix/freedom3"
	_, baseURL := workerStackWith(t, func(r *runs.Service) {
		r.SetPrefs(map[string]*runs.WorkspacePrefs{ws: {
			Stages: map[string]string{},
			Groups: map[string][]runs.Member{"review": {{Runner: "claude", Focus: "security"}, {Runner: "glm", Focus: "tests"}}},
		}})
	})
	root := workerClient{base: baseURL, key: "root-secret"}
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"martin","workspaces":["`+ws+`"],"approver":true}`)}
	plain := workerClient{base: baseURL, key: mint(t, root, `{"name":"agent","workspaces":["`+ws+`"]}`)}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: leases}

	// Plan, approve, implement (head abc1234def), review group: security
	// fails, tests (the newest member) passes -> blocked.
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"so item","author_agent_id":"martin"}`), 201).(map[string]any)
	itemID := item["id"].(string)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/queue", ""), 200)
	run := claim(t, worker, `{"worker_id":"w1","runners":["claude"],"workspaces":["`+ws+`"]}`)
	plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"p","author_agent_id":"worker-1"}`), 201).(map[string]any)
	planID := plan["id"].(string)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"step"}`), 201)
	mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
	revID := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"d2ce73d"}`), 201).(map[string]any)["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+run["id"].(string)+"/report", `{"worker_id":"w1","status":"succeeded","summary":"plan ready","branch":"agent/so"}`, leases[run["id"].(string)]), 200)
	// Asking before the review exists is refused.
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{"runner":"codex"}`), 400, "before review")
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+revID+`"}`), 200)
	run = claim(t, worker, `{"worker_id":"w1","runners":["claude"],"workspaces":["`+ws+`"]}`)
	mustDo(t, worker.reqH("POST", "/api/runs/"+run["id"].(string)+"/report", `{"worker_id":"w1","status":"succeeded","summary":"done","head_sha":"abc1234def","branch":"agent/so"}`, leases[run["id"].(string)]), 200)
	sec := claim(t, worker, `{"worker_id":"w-claude","runners":["claude"],"workspaces":["`+ws+`"]}`)
	tst := claim(t, worker, `{"worker_id":"w-glm","runners":["glm"],"workspaces":["`+ws+`"]}`)
	mustDo(t, worker.reqH("POST", "/api/runs/"+sec["id"].(string)+"/report", `{"worker_id":"w-claude","status":"failed","reason":"review failed: injection","summary":"injection","findings":[{"severity":"blocking","text":"Query built by concatenation"}]}`, leases[sec["id"].(string)]), 200)
	mustDo(t, worker.reqH("POST", "/api/runs/"+tst["id"].(string)+"/report", `{"worker_id":"w-glm","status":"succeeded","summary":"tests ok"}`, leases[tst["id"].(string)]), 200)
	status := func() string {
		return mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)["status"].(string)
	}
	if status() != "blocked" {
		t.Fatalf("setup: item should be blocked by its review group, is %s", status())
	}

	// Who and when.
	mustDo(t, plain.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{"runner":"codex"}`), 403, "plain key")
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{}`), 400, "no runner, no default")
	req := mustJSON(t, approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{"runner":"codex","via":"dashboard"}`), 202).(map[string]any)
	if req["runner"] != "codex" || req["head_sha"] != "abc1234def" {
		t.Fatalf("queued: %v", req)
	}
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{"runner":"glm"}`), 409, "one at a time")

	// Only a codex worker gets it; claiming doesn't move the item.
	mustDo(t, worker.req("POST", "/api/runs/claim", `{"worker_id":"w-claude","runners":["claude"],"workspaces":["`+ws+`"]}`), 204, "pinned to codex")
	so := claim(t, worker, `{"worker_id":"w-codex","runners":["codex"],"workspaces":["`+ws+`"]}`)
	if so["stage"] != "second_opinion" || so["head_sha"] != "abc1234def" || so["runner"] != "codex" {
		t.Fatalf("second-opinion run: %v", so)
	}
	if status() != "blocked" {
		t.Fatalf("claiming must not move the item: %s", status())
	}
	soID := so["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+soID+"/report", `{"worker_id":"w-codex","status":"succeeded","verdict":"pass","summary":"The query is parameterised further down; the security finding is a false positive.","findings":[{"severity":"nit","text":"name the bind variable"}]}`, leases[soID]), 200)
	got := mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
	content, _ := got["content"].(string)
	if got["status"] != "blocked" || !strings.Contains(content, "## Second opinion — codex @ abc1234") ||
		!strings.Contains(content, "**Verdict:** pass (advisory") || !strings.Contains(content, "### Nits") {
		t.Fatalf("after the second opinion: %v\n%s", got["status"], content)
	}

	// The same runner on the same head needs force; the default runner
	// comes from pipeline settings.
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{"runner":"codex"}`), 409, "same runner, same head")
	mustDo(t, approver.req("POST", "/api/workspaces/"+escapeWS(ws)+"/pipeline", ""), 405, "method guard (sanity)")
	mustDo(t, approver.req("PUT", "/api/workspaces/"+escapeWS(ws)+"/pipeline", `{"second_opinion_runner":"opus"}`), 200)
	req = mustJSON(t, approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{}`), 202).(map[string]any)
	if req["runner"] != "opus" {
		t.Fatalf("default runner from pipeline settings: %v", req)
	}
	opus := claim(t, worker, `{"worker_id":"w-opus","runners":["opus"],"workspaces":["`+ws+`"]}`)
	mustDo(t, worker.reqH("POST", "/api/runs/"+opus["id"].(string)+"/report", `{"worker_id":"w-opus","status":"failed","reason":"usage limit reached"}`, leases[opus["id"].(string)]), 200)
	content = mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)["content"].(string)
	if !strings.Contains(content, "## Second opinion — opus @ abc1234") || !strings.Contains(content, "did not finish: usage limit reached") {
		t.Fatalf("a failed run says why:\n%s", content)
	}
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{"runner":"codex","force":true}`), 202, "force")
	if status() != "blocked" {
		t.Fatalf("second opinions never move the item: %s", status())
	}
}
