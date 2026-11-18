package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// opencode 2 (the rewrite released as 2.x) changed what the runner relies
// on, verified live against 2.3.2 (TestLiveOpencodeLockdown):
//   - `run` lost --dir and --variant: the folder is the process's working
//     directory and $PWD, the effort is the model's #variant;
//   - by default every client talks to ONE shared background server per
//     user, which runs the tools with the environment it was started with:
//     --standalone gives the run a private server of its own (the run's
//     environment, inside a wrapper with it), never the shared one;
//   - OPENCODE_PERMISSION is not read. Permissions are an ordered list
//     (`permissions`, the last match wins) in a config file — the run's
//     own config directory holds it — and every agent appends its own rules
//     after them (build allows questions), so the hard denies are also
//     `policies` (`tool.use`), which overrule agent rules and saved
//     approvals and never grant anything;
//   - the action names are shell and subagent (bash and task before);
//   - MCP servers sit under mcp.servers; skills, plugins and MCP servers
//     from anywhere but the run's config are dropped by policy (mcp.use,
//     skill.use, plugin.use, which need 2.2.3).
//
// The event stream (--format json) kept its shape.

// ocVersion is an opencode CLI's version; ok is false when it could not be
// read (a v1 build, or something else, takes the v1 path).
type ocVersion struct {
	major, minor, patch int
	ok                  bool
}

func (v ocVersion) v2() bool { return v.ok && v.major >= 2 }

func (v ocVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

// atLeast compares with major.minor.patch.
func (v ocVersion) atLeast(major, minor, patch int) bool {
	if v.major != major {
		return v.major > major
	}
	if v.minor != minor {
		return v.minor > minor
	}
	return v.patch >= patch
}

var reOCVersion = regexp.MustCompile(`\bv?(\d+)\.(\d+)\.(\d+)\b`)

func parseOCVersion(out string) ocVersion {
	m := reOCVersion.FindStringSubmatch(out)
	if m == nil {
		return ocVersion{}
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return ocVersion{a, b, c, true}
}

// ocVersions caches a binary's version by its path, size and time: the
// CLI is large and starts slowly, and an upgrade in place changes the key.
var ocVersions sync.Map

// opencodeVersion reads `bin --version`, resolved on pathEnv ("" = the
// worker's PATH).
func opencodeVersion(ctx context.Context, bin, pathEnv string) ocVersion {
	if pathEnv == "" {
		pathEnv = os.Getenv("PATH")
	}
	path, err := lookPathIn(bin, pathEnv)
	if err != nil {
		return ocVersion{}
	}
	st, err := os.Stat(path)
	if err != nil {
		return ocVersion{}
	}
	key := fmt.Sprintf("%s|%d|%d", path, st.Size(), st.ModTime().UnixNano())
	if v, ok := ocVersions.Load(key); ok {
		return v.(ocVersion)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, "--version")
	cmd.Env = append(os.Environ(), "PATH="+pathEnv)
	out, _ := cmd.Output()
	v := parseOCVersion(string(out))
	if cctx.Err() == nil {
		ocVersions.Store(key, v)
	}
	return v
}

// checkV2 is Check for opencode 2: a release new enough for the policies
// the lockdown relies on, and the flags a run uses.
func (o opencodeRunner) checkV2(ctx context.Context, v ocVersion, pathEnv string) error {
	if v.major > 2 {
		return fmt.Errorf("%s is opencode %s; the worker's lockdown is verified for opencode 1 and 2 — use opencode 2", o.bin, v)
	}
	if !v.atLeast(2, 2, 3) {
		return fmt.Errorf("%s is opencode %s; opencode 2 before 2.2.3 ignores the policies that keep a run's skills, plugins and MCP servers to its own — upgrade opencode", o.bin, v)
	}
	missing, err := missingFlags(ctx, o.bin, pathEnv, []string{"run", "--help"}, "--format", "--session", "--model")
	if err != nil {
		return err
	}
	if more, err := missingFlags(ctx, o.bin, pathEnv, []string{"--help"}, "--standalone"); err == nil {
		missing = append(missing, more...)
	} else {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s (opencode %s) does not support %s — upgrade opencode", o.bin, v, strings.Join(missing, ", "))
	}
	return nil
}

// v2Args are the arguments of an opencode 2 run: a private server, JSON
// events, the model with the effort as its variant.
func opencodeV2Args(model, effort, session string, resume bool, prompt string) []string {
	args := []string{"run", "--standalone", "--format", "json"}
	if model != "" {
		if effort != "" && !strings.Contains(model, "#") {
			model += "#" + effort
		}
		args = append(args, "-m", model)
	}
	if resume && session != "" {
		args = append(args, "-s", session)
	}
	return append(args, prompt)
}

// ocRule is one opencode 2 permission rule or policy statement.
type ocRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// opencodeV2Config is the run's config file for opencode 2: the stage's
// permissions (deny all, the stage's allows, the hard denies), the same
// hard denies as policies, the run's MCP servers and nothing else.
func opencodeV2Config(allowed, denied []string, servers map[string]any) string {
	perms := []ocRule{{"*", "*", "deny"}}
	allow := func(action, resource string) {
		perms = append(perms, ocRule{action, resource, "allow"})
	}
	var hard []ocRule // denied as a permission AND as a tool.use policy
	deny := func(action, resource string) {
		hard = append(hard, ocRule{action, resource, "deny"})
	}
	for _, a := range allowed {
		name, pattern, hasPattern := strings.Cut(strings.TrimSpace(a), "(")
		pattern = strings.TrimSuffix(pattern, ")")
		switch {
		case name == "Read":
			allow("read", "*")
		case name == "Glob":
			allow("glob", "*")
		case name == "Grep":
			allow("grep", "*")
		case name == "Edit" || name == "Write" || name == "MultiEdit":
			allow("edit", "*")
		case name == "TodoWrite":
			allow("todowrite", "*")
		case name == "Bash" && !hasPattern:
			allow("shell", "*")
		case name == "Bash":
			prefix := strings.TrimSuffix(strings.TrimSuffix(pattern, ":*"), "*")
			allow("shell", prefix)
			allow("shell", prefix+" *")
		case strings.HasPrefix(name, "mcp__"):
			allow(opencodeMCPName(name)+"*", "*")
		}
	}
	// Edits to git's own files (opencodeEdit), relative and absolute.
	for _, p := range []string{".git", ".git/*", "*/.git", "*/.git/*"} {
		deny("edit", p)
	}
	for _, d := range denied {
		if n, p, ok := strings.Cut(d, "("); ok && n == "Bash" {
			p = strings.TrimSuffix(p, ")")
			if prefix, legacy := strings.CutSuffix(p, ":*"); legacy || !strings.Contains(p, "*") {
				deny("shell", prefix)
				deny("shell", prefix+" *")
			} else {
				deny("shell", p)
			}
		}
	}
	for _, p := range opencodeBashEscapes {
		deny("shell", p)
	}
	for _, d := range denied {
		if strings.HasPrefix(d, "mcp__") {
			deny(opencodeMCPName(d), "*")
		}
	}
	for _, a := range []string{"external_directory", "webfetch", "websearch", "question", "subagent", "skill"} {
		deny(a, "*")
	}
	perms = append(perms, hard...)

	var policies []ocRule
	for _, h := range hard {
		policies = append(policies, ocRule{"tool.use", h.Action + ":" + h.Resource, "deny"})
	}
	policies = append(policies,
		ocRule{"skill.use", "*", "deny"},
		ocRule{"plugin.use", "*", "deny"},
		ocRule{"mcp.use", "*", "deny"},
	)
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		policies = append(policies, ocRule{"mcp.use", name, "allow"})
	}

	cfg := map[string]any{
		"$schema":     "https://opencode.ai/config.json",
		"permissions": perms,
		"policies":    policies,
		"update":      "disable",
		"share":       "disabled",
	}
	if len(servers) > 0 {
		cfg["mcp"] = map[string]any{"servers": servers}
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// opencodeV2Servers translates a Claude-format mcp_config file into
// opencode 2's mcp.servers entries.
func opencodeV2Servers(path string) (map[string]any, error) {
	servers, err := readMCPServers(path)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for name, s := range servers {
		if s.URL != "" {
			m := map[string]any{"type": "remote", "url": s.URL}
			if len(s.Headers) > 0 {
				m["headers"] = s.Headers
			}
			out[name] = m
			continue
		}
		m := map[string]any{"type": "local", "command": append([]string{s.Command}, s.Args...)}
		if len(s.Env) > 0 {
			m["environment"] = s.Env
		}
		out[name] = m
	}
	return out, nil
}
