package worker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func codexMCPFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A run's MCP file becomes codex settings: http servers only, header values
// in variables and never in the arguments, only the tools the stage allows.
func TestCodexMCPSettings(t *testing.T) {
	file := codexMCPFile(t, `{"mcpServers": {
		"skopos":  {"type": "http", "url": "http://127.0.0.1:8080/mcp", "headers": {"Authorization": "Bearer skr_run-token-123456"}},
		"router":  {"type": "http", "url": "https://router.example.com/mcp", "headers": {"X-Api-Key": "router-key-abcdef", "X-Team": "shop"}},
		"browser": {"command": "npx", "args": ["browser-mcp"]},
		"unused":  {"type": "http", "url": "https://unused.example.com/mcp"}
	}}`)
	allowed := []string{"Read", "Bash(git diff:*)", "mcp__skopos__code_search", "mcp__skopos__blackboard_read", "mcp__router", "mcp__browser"}
	denied := []string{"Bash(git push:*)", "mcp__router__jobs__create"}
	plan, err := codexMCP(file, allowed, denied, []string{"skr_run-token-123456", "router-key-abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Names, []string{"router", "skopos"}) {
		t.Fatalf("servers given: %v", plan.Names)
	}
	args := strings.Join(plan.Args, "\n")
	for _, want := range []string{
		`mcp_servers.skopos.url="http://127.0.0.1:8080/mcp"`,
		`mcp_servers.skopos.enabled_tools=["blackboard_read","code_search"]`,
		`mcp_servers.skopos.default_tools_approval_mode="approve"`,
		`mcp_servers.router.url="https://router.example.com/mcp"`,
		`mcp_servers.router.disabled_tools=["jobs__create"]`,
		`mcp_servers.router.default_tools_approval_mode="approve"`,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %s in:\n%s", want, args)
		}
	}
	// The whole router server is allowed: no list of names for it.
	if strings.Contains(args, "mcp_servers.router.enabled_tools") || strings.Contains(args, "mcp_servers.skopos.disabled_tools") {
		t.Errorf("tool lists: %s", args)
	}
	// No header value is in an argument; each is in a variable the header
	// setting names.
	for _, secret := range []string{"skr_run-token-123456", "router-key-abcdef", "shop"} {
		if strings.Contains(args, secret) {
			t.Errorf("a header value is on the command line: %s", secret)
		}
		found := false
		for name, v := range plan.Env {
			if strings.Contains(v, secret) {
				found = strings.Contains(args, `"`+name+`"`)
			}
		}
		if !found {
			t.Errorf("header value %q is not handed over in a named variable: %v", secret, plan.Env)
		}
	}
	if len(plan.Env) != 3 {
		t.Fatalf("one variable per header: %v", plan.Env)
	}
	// The command server is left out and said so; a server the stage allows
	// nothing of is simply not configured.
	if len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "browser") || !strings.Contains(plan.Notes[0], "command") {
		t.Fatalf("notes: %v", plan.Notes)
	}
	if strings.Contains(args, "unused") || strings.Contains(args, "browser") {
		t.Errorf("a server the run does not get is configured: %s", args)
	}
	// The variables are kept out of the agent's shell with the account's.
	if ex := codexShellExclude(plan.Env); !strings.Contains(ex, `"SKOPOS_MCP_SECRET_0"`) || !strings.Contains(ex, `"SKOPOS_MCP_SECRET_2"`) {
		t.Fatalf("shell exclude: %s", ex)
	}
}

// What cannot be said to codex safely leaves the server out.
func TestCodexMCPLeavesOut(t *testing.T) {
	cases := []struct {
		name, server string
		allowed      []string
		denied       []string
		secrets      []string
		wantNote     string
	}{
		{"a secret in the address", `{"url": "https://x.example.com/mcp?key=sekret-value-1"}`, []string{"mcp__s"}, nil, []string{"sekret-value-1"}, "carries a secret"},
		{"an escaped secret in the address", `{"url": "https://x.example.com/mcp?key=a%2Fb+secret%3D1"}`, []string{"mcp__s"}, nil, []string{"a/b secret=1"}, "carries a secret"},
		{"user info in the address", `{"url": "https://user:pw@x.example.com/mcp"}`, []string{"mcp__s"}, nil, nil, "user info"},
		{"not http", `{"url": "ftp://x.example.com/mcp"}`, []string{"mcp__s"}, nil, nil, "not an http"},
		{"a deny pattern over a whole server", `{"url": "https://x.example.com/mcp"}`, []string{"mcp__s"}, []string{"mcp__s__write_*"}, nil, "pattern"},
	}
	for _, c := range cases {
		plan, err := codexMCP(codexMCPFile(t, `{"mcpServers": {"s": `+c.server+`}}`), c.allowed, c.denied, c.secrets)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(plan.Args) != 0 || len(plan.Names) != 0 || len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], c.wantNote) {
			t.Errorf("%s: args %v notes %v", c.name, plan.Args, plan.Notes)
		}
		for _, n := range plan.Notes {
			for _, s := range c.secrets {
				if strings.Contains(n, s) {
					t.Errorf("%s: the note repeats a secret: %s", c.name, n)
				}
			}
		}
	}
}

// The tool lists: names only, the deny list wins, and an entry belongs to
// the longest server name it starts with.
func TestCodexMCPTools(t *testing.T) {
	servers := []string{"a", "a__b", "router"}
	// A deny pattern is applied to a list of names.
	all, tools, whole, deny, problem := codexMCPTools("router", servers,
		[]string{"mcp__router__read_one", "mcp__router__read_two", "mcp__router__write_one", "mcp__router__read_*"},
		[]string{"mcp__router__write_*", "mcp__router__read_two"})
	if all || whole || problem != "" || len(deny) != 0 || !slices.Equal(tools, []string{"read_one"}) {
		t.Fatalf("names with denies: all=%v tools=%v whole=%v deny=%v problem=%q", all, tools, whole, deny, problem)
	}
	// mcp__<server>__* is the whole server, in either list.
	if all, _, _, _, _ := codexMCPTools("router", servers, []string{"mcp__router__*"}, nil); !all {
		t.Fatal("a * tool allows the whole server")
	}
	if _, _, whole, _, _ := codexMCPTools("router", servers, []string{"mcp__router"}, []string{"mcp__router__*"}); !whole {
		t.Fatal("a * tool in the deny list denies the whole server")
	}
	// mcp__a__b__x is server a__b's tool x, not server a's tool b__x.
	if _, tools, _, _, _ := codexMCPTools("a", servers, []string{"mcp__a__b__x", "mcp__a__y"}, nil); !slices.Equal(tools, []string{"y"}) {
		t.Fatalf("server a: %v", tools)
	}
	if _, tools, _, _, _ := codexMCPTools("a__b", servers, []string{"mcp__a__b__x", "mcp__a__y"}, nil); !slices.Equal(tools, []string{"x"}) {
		t.Fatalf("server a__b: %v", tools)
	}
	// A whole-server deny leaves the run without it, with no note.
	plan, err := codexMCP(codexMCPFile(t, `{"mcpServers": {"router": {"url": "https://r.example.com/mcp"}}}`), []string{"mcp__router"}, []string{"mcp__router"}, nil)
	if err != nil || len(plan.Args) != 0 || len(plan.Notes) != 0 {
		t.Fatalf("a denied server: %v %v %v", plan.Args, plan.Notes, err)
	}
	// A broken file is an error, as it is for the other runners.
	if _, err := codexMCP(codexMCPFile(t, `{`), nil, nil, nil); err == nil {
		t.Fatal("an unreadable MCP file must fail the run")
	}
}
