package routes

import (
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/runs"
)

// Item templates: held for a workspace (its approvers) or a group (root),
// the workspace's own used before its group's; an item started from one
// takes the fields the request leaves empty, with the placeholders filled
// in, and records which template it came from. A template that approves
// plans automatically needs an approver to start an item from it.
func TestIntegrationItemTemplates(t *testing.T) {
	const ws = "github.com/globex/shop"
	_, baseURL := workerStackWith(t, func(*runs.Service) {})
	root := workerClient{base: baseURL, key: "root-secret"}
	wsPath := "/api/workspaces/" + strings.ReplaceAll(ws, "/", "%2F")
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"github.com/globex/web"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
	plain := workerClient{base: baseURL, key: mint(t, root, `{"name":"agent","workspaces":["`+ws+`"]}`)}
	group := mustJSON(t, root.req("POST", "/api/groups", `{"name":"globex","members":["`+ws+`","github.com/globex/web"]}`), 201).(map[string]any)
	groupID := group["id"].(string)

	// The group's template, root only.
	deps := `{"title":"Update the dependencies of {workspace} ({month})","brief":"Bump every dependency.\n\nFix what breaks.","tags":["Deps"],"needs":["stock"],"agents":"solo","auto_approve_plan":true}`
	if r := approver.req("PUT", "/api/groups/"+groupID+"/templates/deps", deps); r.code != 403 {
		t.Fatalf("a group's template is root's: %d %s", r.code, r.body)
	}
	saved := mustJSON(t, root.req("PUT", "/api/groups/"+groupID+"/templates/deps", deps), 200).(map[string]any)
	if saved["agents"] != "solo" || saved["tags"].([]any)[0] != "deps" || saved["auto_approve_plan"] != true {
		t.Fatalf("the template as saved: %v", saved)
	}
	// Saving the same again adds no revision.
	again := mustJSON(t, root.req("PUT", "/api/groups/"+groupID+"/templates/deps", deps), 200).(map[string]any)
	if again["rev"] != saved["rev"] {
		t.Fatal("an unchanged save adds no revision")
	}
	if r := root.req("PUT", "/api/groups/"+groupID+"/templates/Bad Name", deps); r.code != 400 && r.code != 404 {
		t.Fatalf("a bad name: %d", r.code)
	}
	if r := root.req("PUT", "/api/groups/"+groupID+"/templates/x", `{"title":"","brief":"b"}`); r.code != 400 {
		t.Fatalf("a template without a title: %d", r.code)
	}
	if r := root.req("PUT", "/api/groups/"+groupID+"/templates/x", `{"title":"t","colour":"red"}`); r.code != 400 {
		t.Fatalf("an unknown field: %d", r.code)
	}

	// The workspace sees it, from the group; a plain key reads it but may
	// not write one.
	list := mustJSON(t, plain.req("GET", wsPath+"/templates", ""), 200).(map[string]any)["templates"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["source"] != "group:globex" {
		t.Fatalf("the workspace's templates: %v", list)
	}
	if r := plain.req("PUT", wsPath+"/templates/tidy", `{"title":"Tidy"}`); r.code != 403 {
		t.Fatalf("a plain key writes no template: %d", r.code)
	}

	// Started from it: the template approves plans automatically, so a
	// plain key may not, and an approver may.
	if r := plain.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","author_agent_id":"a","template":"deps"}`); r.code != 403 {
		t.Fatalf("a plain key starting from a template that approves plans: %d %s", r.code, r.body)
	}
	item := mustJSON(t, approver.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","author_agent_id":"alex","template":"deps","extra":"The router stays on 4."}`), 201).(map[string]any)
	month := time.Now().UTC().Format("2006-01")
	if item["title"] != "Update the dependencies of shop ("+month+")" {
		t.Fatalf("the title with its placeholders filled in: %v", item["title"])
	}
	if item["template"] != "deps" || item["template_rev"] != saved["rev"] || item["agents"] != "solo" || item["auto_approve_plan"] != true ||
		strings.Join(toStrings(item["needs"]), ",") != "stock" || strings.Join(toStrings(item["tags"]), ",") != "deps" {
		t.Fatalf("the item takes the template's fields: %v", item)
	}
	full := mustJSON(t, approver.req("GET", "/api/inbox/"+item["id"].(string), ""), 200).(map[string]any)
	if full["content"] != "Bump every dependency.\n\nFix what breaks.\n\nThe router stays on 4." {
		t.Fatalf("the brief with the extra text: %q", full["content"])
	}

	// The workspace's own template of the name is used before the group's;
	// what the request gives is used before the template's.
	mustDo(t, approver.req("PUT", wsPath+"/templates/deps", `{"title":"Shop dependencies","brief":"Only the shop's."}`), 200)
	own := mustJSON(t, plain.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","author_agent_id":"a","template":"deps","title":"By hand"}`), 201).(map[string]any)
	if own["title"] != "By hand" || own["auto_approve_plan"] != nil || own["template"] != "deps" {
		t.Fatalf("the workspace's own template, the request's title: %v", own)
	}
	if r := plain.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","author_agent_id":"a","template":"nope"}`); r.code != 400 || !strings.Contains(r.body, `no template \"nope\"`) {
		t.Fatalf("an unknown template: %d %s", r.code, r.body)
	}

	// A second group may not hold a template of the same name for a shared
	// workspace.
	other := mustJSON(t, root.req("POST", "/api/groups", `{"name":"web","members":["github.com/globex/web"]}`), 201).(map[string]any)
	if r := root.req("PUT", "/api/groups/"+other["id"].(string)+"/templates/deps", deps); r.code != 409 {
		t.Fatalf("two groups holding deps for one workspace: %d %s", r.code, r.body)
	}

	// Removed: the workspace falls back to the group's again.
	mustDo(t, approver.req("DELETE", wsPath+"/templates/deps", ""), 204)
	if got := mustJSON(t, plain.req("GET", wsPath+"/templates/deps", ""), 200).(map[string]any); got["source"] != "group:globex" {
		t.Fatalf("after the workspace's is removed: %v", got)
	}
}

// Schedules over the API: an approver holds one for a workspace, a plain
// key may read it but not hold or run one, a run by hand starts an item
// from the template and queues it, and the firings say what each did.
func TestIntegrationSchedules(t *testing.T) {
	const ws = "github.com/globex/shop"
	_, baseURL := workerStackWith(t, func(*runs.Service) {})
	root := workerClient{base: baseURL, key: "root-secret"}
	wsPath := "/api/workspaces/" + strings.ReplaceAll(ws, "/", "%2F")
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"github.com/globex/web"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
	plain := workerClient{base: baseURL, key: mint(t, root, `{"name":"agent","workspaces":["`+ws+`"]}`)}
	other := workerClient{base: baseURL, key: mint(t, root, `{"name":"web","workspaces":["github.com/globex/web"],"approver":true}`)}
	mustDo(t, approver.req("PUT", wsPath+"/templates/deps", `{"title":"Update the dependencies ({month})","brief":"Bump them."}`), 200)

	body := `{"name":"deps","owner_kind":"workspace","owner_id":"` + ws + `","template":"deps","rhythm":"monthly","timezone":"Europe/London"}`
	if r := plain.req("POST", "/api/schedules", body); r.code != 403 {
		t.Fatalf("a plain key holds no schedule: %d %s", r.code, r.body)
	}
	if r := approver.req("POST", "/api/schedules", strings.Replace(body, "monthly", "now and then", 1)); r.code != 400 {
		t.Fatalf("a rhythm that does not parse: %d %s", r.code, r.body)
	}
	sc := mustJSON(t, approver.req("POST", "/api/schedules", body), 201).(map[string]any)
	id := sc["id"].(string)
	if sc["enabled"] != true || sc["next"] == nil || sc["created_by"] != "key alex" {
		t.Fatalf("the schedule: %v", sc)
	}
	list := mustJSON(t, plain.req("GET", "/api/schedules", ""), 200).(map[string]any)["schedules"].([]any)
	if len(list) != 1 {
		t.Fatalf("a key with the workspace reads it: %v", list)
	}
	if got := mustJSON(t, other.req("GET", "/api/schedules", ""), 200).(map[string]any)["schedules"].([]any); len(got) != 0 {
		t.Fatalf("another workspace's key does not see it: %v", got)
	}
	mustDo(t, other.req("GET", "/api/schedules/"+id, ""), 404)
	if r := plain.req("POST", "/api/schedules/"+id+"/run", ""); r.code != 403 {
		t.Fatalf("a plain key does not run it: %d", r.code)
	}
	fired := mustJSON(t, approver.req("POST", "/api/schedules/"+id+"/run", ""), 200).(map[string]any)["firings"].([]any)
	if len(fired) != 1 || fired[0].(map[string]any)["outcome"] != "created" {
		t.Fatalf("run by hand: %v", fired)
	}
	item := mustJSON(t, approver.req("GET", "/api/inbox/"+fired[0].(map[string]any)["item_id"].(string), ""), 200).(map[string]any)
	if item["status"] != "queued" || item["template"] != "deps" || !strings.HasPrefix(item["title"].(string), "Update the dependencies (") || item["author_agent_id"] != "schedule:deps" {
		t.Fatalf("the item it started: %v", item)
	}
	// Run again: the item is still open, so none is added.
	again := mustJSON(t, approver.req("POST", "/api/schedules/"+id+"/run", ""), 200).(map[string]any)["firings"].([]any)
	if len(again) != 1 || again[0].(map[string]any)["outcome"] != "skipped" {
		t.Fatalf("a second run while the item is open: %v", again)
	}
	if fs := mustJSON(t, plain.req("GET", "/api/schedules/"+id+"/firings", ""), 200).(map[string]any)["firings"].([]any); len(fs) != 2 {
		t.Fatalf("firings: %v", fs)
	}
	paused := mustJSON(t, approver.req("PUT", "/api/schedules/"+id, `{"enabled":false,"rhythm":"0 7 * * mon"}`), 200).(map[string]any)
	if paused["enabled"] != false || paused["rhythm"] != "0 7 * * mon" {
		t.Fatalf("paused and moved: %v", paused)
	}
	if r := approver.req("PUT", "/api/schedules/"+id, `{"owner_id":"elsewhere"}`); r.code != 400 {
		t.Fatalf("a schedule keeps its owner: %d", r.code)
	}
	mustDo(t, approver.req("DELETE", "/api/schedules/"+id, ""), 204)
	mustDo(t, approver.req("GET", "/api/schedules/"+id, ""), 404)
}
