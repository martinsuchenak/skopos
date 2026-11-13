package routes

import (
	"strings"
	"testing"
)

// Checking a config before saving it: every field's problem at once, what
// only the whole config shows, the fields a save would change — and
// nothing is written. It needs what the save needs.
func TestIntegrationConfigCheck(t *testing.T) {
	_, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const ws = "github.com/globex/api"
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	plain := workerClient{base: baseURL, key: mint(t, root, `{"name":"agent","workspaces":["`+ws+`"]}`)}
	path := "/api/workspaces/" + escapeWS(ws) + "/config"
	problems := func(body string) map[string]string {
		t.Helper()
		out := map[string]string{}
		res := mustJSON(t, root.req("POST", path+"/check", body), 200).(map[string]any)
		for _, p := range res["problems"].([]any) {
			m := p.(map[string]any)
			out[m["field"].(string)] = m["message"].(string)
		}
		return out
	}

	mustDo(t, plain.req("POST", path+"/check", `{"config":{"default_branch":"main"}}`), 403)
	mustDo(t, plain.req("POST", path+"/check", `{"config":{"checkout":"copy"}}`), 403)
	mustDo(t, root.req("POST", path+"/check", `{}`), 400)

	got := problems(`{"config":{"checkout":"copy","qa_timeout":"soon","push":{"kind":"gitlab"},"checks":["go test ./..."]}}`)
	if len(got) != 3 || !strings.Contains(got["checkout"], "worktree, branch") || !strings.Contains(got["qa_timeout"], "duration") || !strings.Contains(got["push.kind"], "github, gitea, none") {
		t.Fatalf("every field's problem at once: %v", got)
	}
	// Sound fields that do not hold together: a push needs somewhere to go.
	whole := problems(`{"config":{"push":{"kind":"github"}}}`)
	if len(whole) != 1 {
		t.Fatalf("a push without a remote is one problem of the whole: %v", whole)
	}
	for field, msg := range whole {
		if !strings.HasPrefix(field, "push") || !strings.Contains(msg, "remote_url") {
			t.Fatalf("the problem names the push: %q %q", field, msg)
		}
	}
	// A sound config: no problems, the fields it would change, nothing saved.
	res := mustJSON(t, root.req("POST", path+"/check", `{"config":{"default_branch":"trunk","qa_timeout":"45m","checks":["go test ./..."]}}`), 200).(map[string]any)
	if len(res["problems"].([]any)) != 0 || len(res["changed"].([]any)) != 3 {
		t.Fatalf("a sound config: %v", res)
	}
	view := mustJSON(t, root.req("GET", path, ""), 200).(map[string]any)
	if view["config"] != nil {
		t.Fatalf("a check writes nothing: %v", view["config"])
	}
	// The read carries the defaults a form shows for fields nothing sets.
	if d, _ := view["defaults"].(map[string]any); d["default_branch"] != "main" || d["qa_timeout"] != "30m0s" || d["checkout"] != "worktree" {
		t.Fatalf("defaults: %v", view["defaults"])
	}
}
