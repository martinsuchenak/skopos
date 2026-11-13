package wsconfig

import (
	"strings"
	"testing"
)

// A check reports every field's problem at once, under the field's own
// name, and says nothing about fields that are sound.
func TestFieldProblems(t *testing.T) {
	doc := `{
		"default_branch": "main",
		"branch_prefix": "has space/",
		"checkout": "copy",
		"check_fix_rounds": "two",
		"qa_timeout": "soon",
		"impl_timeout": "90",
		"plan_timeout": "5s",
		"checks": "go test ./...",
		"mcp_servers": ["router"],
		"push": {"kind": "gitlab", "remote_url": "git@host:repo.git", "pull_request": "draft"},
		"stages": {"deploy": {}, "qa": {"env": {"E2E_PASSWORD": "x"}}},
		"speed": "fast"
	}`
	by := map[string]string{}
	for _, p := range FieldProblems([]byte(doc)) {
		if _, dup := by[p.Field]; dup {
			t.Errorf("two problems for %s", p.Field)
		}
		by[p.Field] = p.Message
	}
	want := map[string]string{
		"branch_prefix":    "may hold letters",
		"checkout":         "not one of worktree, branch",
		"check_fix_rounds": "takes a whole number",
		"qa_timeout":       "is not a duration: write it like 90s, 10m or 1h30m",
		"plan_timeout":     "is not between",
		"impl_timeout":     "is not a duration",
		"checks":           "takes a list",
		"mcp_servers":      "is not an MCP tool prefix",
		"push.kind":        "not one of github, gitea, none",
		"push.remote_url":  "is not an http(s) URL",
		"stages.deploy":    "is not a stage",
		"stages.qa":        "E2E_PASSWORD",
		"speed":            "is not a field of the config",
	}
	for field, part := range want {
		if !strings.Contains(by[field], part) {
			t.Errorf("%s: %q, want it to say %q", field, by[field], part)
		}
	}
	for field, msg := range by {
		if _, ok := want[field]; !ok {
			t.Errorf("unexpected problem for %s: %s", field, msg)
		}
		if strings.HasPrefix(msg, field) || strings.Contains(msg, "invalid config") {
			t.Errorf("%s: the message repeats the field or the sentinel: %q", field, msg)
		}
	}
	if got := FieldProblems([]byte(`{"default_branch":"main","setup":["make deps"],"push":{"steps":false}}`)); len(got) != 0 {
		t.Fatalf("a sound config has no problems: %v", got)
	}
	if got := FieldProblems([]byte(`["not","an","object"]`)); len(got) != 1 || got[0].Field != "" {
		t.Fatalf("not an object: %v", got)
	}
}
