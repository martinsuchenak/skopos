package worker

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// A codex run's MCP servers.
//
// Codex has no MCP config file a run could be handed: the lockdown ignores
// CODEX_HOME/config.toml, so every server becomes `-c mcp_servers.<name>.*`
// settings. What that means, each point verified live against codex-cli
// 0.163.0 (TestLiveCodexLockdown):
//
//   - http servers only. Codex starts a command server itself, outside the
//     sandbox it keeps the agent's shell in, with the worker's privileges:
//     such a server is left out of a codex run.
//   - A header's value is never on the command line (`ps` shows it): it is
//     put in a variable of the codex process, named in env_http_headers,
//     and that variable is kept out of the agent's shell like the account's
//     own (codexShellExclude).
//   - A server's address IS on the command line: one that carries user
//     info or one of the run's secrets is left out.
//   - Tools: codex is told exactly which of a server's tools exist for the
//     run (enabled_tools, or everything but disabled_tools), from the
//     stage's allow list and the deny list. With approvals off — the
//     lockdown's approval_policy = "never" — codex refuses every MCP call
//     unless the server's tools are approved in its settings, so every
//     tool the run is given is approved there. A deny pattern that cannot
//     be turned into a list of names leaves the server out.
//   - --strict-config does not check the keys under mcp_servers.<name> (a
//     made-up one is accepted), so a renamed key would be ignored silently:
//     the live test checks behaviour — a tool outside the list is not
//     offered, a listed one runs.
//   - The agent's shell still has no network: the model reaches a server
//     only through its tools.

// codexMCPPlan is what a codex run is started with for its MCP servers.
type codexMCPPlan struct {
	Args  []string          // -c pairs for the command line (no secret in them)
	Env   map[string]string // variables of the codex process holding header values
	Names []string          // the servers the run gets
	Notes []string          // servers left out, and why
}

// codexMCPServerName: a name usable as a bare key on the command line.
var codexMCPServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// codexMCPStartup bounds a server's handshake: codex waits for every server
// before the first turn.
const codexMCPStartup = 20

// codexMCP reads a run's MCP config (Claude's mcpServers shape) and turns
// it into codex settings. allowed is the stage's tool list, denied the deny
// rules of the run's settings, secrets the run's secret values (an address
// holding one is not put on the command line).
func codexMCP(file string, allowed, denied, secrets []string) (codexMCPPlan, error) {
	plan := codexMCPPlan{Env: map[string]string{}}
	raw, err := os.ReadFile(file)
	if err != nil {
		return plan, fmt.Errorf("mcp_config: %v", err)
	}
	var in struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Command string            `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return plan, fmt.Errorf("mcp_config %s: %v", file, err)
	}
	names := make([]string, 0, len(in.MCPServers))
	for name := range in.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	quote := func(v any) string { b, _ := json.Marshal(v); return string(b) } // a JSON string or list is a TOML one
	for _, name := range names {
		s := in.MCPServers[name]
		all, tools, deniedWhole, denyNames, problem := codexMCPTools(name, names, allowed, denied)
		if deniedWhole || (!all && len(tools) == 0 && problem == "") {
			continue // the stage gives the run none of its tools
		}
		left := func(why string) {
			plan.Notes = append(plan.Notes, fmt.Sprintf("mcp server %s: %s: left out of the codex run", name, why))
		}
		switch {
		case problem != "":
			left(problem)
			continue
		case !codexMCPServerName.MatchString(name):
			left("codex cannot be given a server of that name")
			continue
		case s.Command != "":
			left("it is a command, which codex would start outside its sandbox")
			continue
		}
		u, err := url.Parse(s.URL)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			left("its address is not an http(s) URL")
			continue
		case u.User != nil:
			left("its address carries user info, and codex takes an address on its command line")
			continue
		case holdsSecret(s.URL, secrets):
			left("its address carries a secret, and codex takes an address on its command line")
			continue
		}
		key := "mcp_servers." + name + "."
		plan.Args = append(plan.Args, "-c", key+"url="+quote(s.URL),
			"-c", key+"startup_timeout_sec="+fmt.Sprint(codexMCPStartup),
			"-c", key+`default_tools_approval_mode="approve"`)
		if len(s.Headers) > 0 {
			headers := make([]string, 0, len(s.Headers))
			for h := range s.Headers {
				headers = append(headers, h)
			}
			sort.Strings(headers)
			pairs := make([]string, len(headers))
			for i, h := range headers {
				env := fmt.Sprintf("SKOPOS_MCP_SECRET_%d", len(plan.Env))
				plan.Env[env] = s.Headers[h]
				pairs[i] = quote(h) + "=" + quote(env)
			}
			plan.Args = append(plan.Args, "-c", key+"env_http_headers={"+strings.Join(pairs, ", ")+"}")
		}
		if all {
			if len(denyNames) > 0 {
				plan.Args = append(plan.Args, "-c", key+"disabled_tools="+quote(denyNames))
			}
		} else {
			plan.Args = append(plan.Args, "-c", key+"enabled_tools="+quote(tools))
		}
		plan.Names = append(plan.Names, name)
	}
	return plan, nil
}

// codexMCPTools reads what the stage's allow list and the deny rules say
// about one server's tools, in Claude's names (mcp__<server>, or
// mcp__<server>__<tool>; a tool may be `*`). all: every tool of the server
// is allowed, less denyNames. Otherwise tools are the allowed ones, with
// the denied ones already taken out. deniedWhole: the deny list names the
// whole server. problem: the lists cannot be expressed to codex.
func codexMCPTools(name string, servers, allowed, denied []string) (all bool, tools []string, deniedWhole bool, denyNames []string, problem string) {
	var denyGlobs []string
	for _, d := range denied {
		tool, whole, ok := codexMCPEntry(d, name, servers)
		switch {
		case !ok:
		case whole || tool == "*":
			deniedWhole = true
		case strings.ContainsAny(tool, "*?["):
			denyGlobs = append(denyGlobs, tool)
		default:
			denyNames = append(denyNames, tool)
		}
	}
	for _, a := range allowed {
		tool, whole, ok := codexMCPEntry(a, name, servers)
		switch {
		case !ok:
		case whole || tool == "*":
			all = true
		case strings.ContainsAny(tool, "*?["):
			// Codex takes names, not patterns: nothing is allowed by it.
		default:
			tools = append(tools, tool)
		}
	}
	if all {
		if len(denyGlobs) > 0 {
			return false, nil, false, nil, fmt.Sprintf("the deny list has a pattern for it (mcp__%s__%s) that cannot be applied to all of a server's tools — allow its tools by name", name, denyGlobs[0])
		}
		sort.Strings(denyNames)
		return true, nil, deniedWhole, slices.Compact(denyNames), ""
	}
	tools = slices.DeleteFunc(tools, func(t string) bool {
		if slices.Contains(denyNames, t) {
			return true
		}
		for _, g := range denyGlobs {
			if ok, _ := path.Match(g, t); ok {
				return true
			}
		}
		return false
	})
	sort.Strings(tools)
	return false, slices.Compact(tools), deniedWhole, nil, ""
}

// codexMCPEntry reads a tool-list entry as one about the named server:
// whole for mcp__<server>, else the tool part of mcp__<server>__<tool>. An
// entry belongs to the longest server name it starts with (a tool name may
// itself hold "__").
func codexMCPEntry(entry, name string, servers []string) (tool string, whole, ok bool) {
	rest, isMCP := strings.CutPrefix(strings.TrimSpace(entry), "mcp__")
	if !isMCP {
		return "", false, false
	}
	owner := ""
	for _, s := range servers {
		if (rest == s || strings.HasPrefix(rest, s+"__")) && len(s) > len(owner) {
			owner = s
		}
	}
	if owner != name {
		return "", false, false
	}
	if rest == name {
		return "", true, true
	}
	return rest[len(name)+2:], false, true
}

// holdsSecret reports whether text carries one of the secret values, as
// written or URL-escaped. Short values are not looked for: they would match
// by chance (the redactor leaves them out for the same reason).
func holdsSecret(text string, secrets []string) bool {
	for _, s := range secrets {
		if len(s) < 8 {
			continue
		}
		if strings.Contains(text, s) || strings.Contains(text, url.QueryEscape(s)) || strings.Contains(text, url.PathEscape(s)) {
			return true
		}
	}
	return false
}
