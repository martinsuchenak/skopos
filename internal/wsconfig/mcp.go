package wsconfig

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/confrev"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/runs"
	"github.com/martinsuchenak/skopos/internal/secretbox"
)

// MCP servers for runs (tmp/central-config — design.md, phase 2b). A
// server is defined once: where it is (an http URL, or a command the worker
// runs) and which secrets it needs, by name. A workspace's or group's
// config says which servers its runs get (`mcp`). The values of the secrets
// are stored apart, sealed (internal/secretbox), and leave the server only
// inside the answer to a claim — for that run's workspace, to the worker
// that runs it. They are never part of a config fetch, an export, a
// revision or an audit note.

// KindMCPServer is the revision kind of MCP server definitions (owner
// "mcp_server:<name>").
const KindMCPServer = "mcp_server"

// MCPAuditEntity is the audit-log entity type of MCP server definitions;
// the entity id is the server's name.
const MCPAuditEntity = "mcp_server"

// MCP server types.
const (
	MCPHTTP    = "http"
	MCPCommand = "command"
)

// ReservedMCPName is the server name the worker provides itself: the run's
// own access to skopos.
const ReservedMCPName = "skopos"

// MCPServerDef defines an MCP server. A value that is a credential is
// never written here: it is named as ${secret:NAME} and set apart
// (SetMCPSecret).
type MCPServerDef struct {
	Name string `json:"name" toml:"-"`
	// Type: http (the agent calls URL) or command (the worker's host runs
	// Command and the agent talks to it).
	Type string `json:"type" toml:"type"`
	// URL and Headers of an http server. ${secret:NAME} is filled in from
	// the server's secrets.
	URL     string            `json:"url,omitempty" toml:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty" toml:"headers,omitempty"`
	// Command (argv) and Env of a command server. ${NAME} is filled in on
	// the worker from its worker.vars; Env values may also name secrets.
	Command []string          `json:"command,omitempty" toml:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty" toml:"env,omitempty"`
	// Workspaces: the workspaces whose runs may be given the server, as
	// globs on the workspace id ("github.com/acme/*", "*" for all). A
	// server that names secrets must say: its credentials go to every
	// worker that runs a workspace it is given to. Empty on a server
	// without secrets = any workspace.
	Workspaces []string `json:"workspaces,omitempty" toml:"workspaces,omitempty"`
}

// AllowsWorkspace reports whether the server may be given to a workspace's
// runs.
func (d MCPServerDef) AllowsWorkspace(ws string) bool {
	if len(d.Workspaces) == 0 {
		return len(d.SecretNames()) == 0
	}
	for _, p := range d.Workspaces {
		// "*" alone is every workspace; any other pattern matches the way
		// a workspace group's does (a * stops at a slash).
		if p == "*" {
			return true
		}
		if ok, _ := path.Match(p, ws); ok {
			return true
		}
	}
	return false
}

var (
	reMCPName    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
	reSecretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	reSecretRef  = regexp.MustCompile(`\$\{secret:([^}]*)\}`)
	reHeaderName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,100}$`)
)

// credentialWords are the words in a name — a header, a variable, a URL
// query parameter, a command flag — that say its value is a credential.
var credentialWords = map[string]bool{
	"AUTH": true, "AUTHORIZATION": true, "TOKEN": true, "SECRET": true, "PASSWORD": true, "PASSWD": true, "PASS": true, "PWD": true,
	"PAT": true, "CRED": true, "CREDS": true, "CREDENTIAL": true, "CREDENTIALS": true, "SESSION": true, "BEARER": true, "JWT": true,
	"DSN": true, "KEY": true, "APIKEY": true, "PRIVATE": true, "COOKIE": true, "SIGNATURE": true, "SIG": true,
}

// credentialName reports a name that holds a credential by the look of it:
// its value has to come from a secret.
func credentialName(name string) bool {
	up := strings.ToUpper(name)
	for _, word := range []string{"TOKEN", "SECRET", "PASSWORD", "APIKEY", "AUTH", "CREDENTIAL"} {
		if strings.Contains(up, word) {
			return true
		}
	}
	for _, seg := range strings.FieldsFunc(up, func(r rune) bool { return (r < 'A' || r > 'Z') && (r < '0' || r > '9') }) {
		if credentialWords[seg] {
			return true
		}
	}
	return false
}

var (
	reOpaque  = regexp.MustCompile(`^[A-Za-z0-9_\-.~+=]{20,}$`)
	reHexLike = regexp.MustCompile(`^[0-9a-fA-F-]+$`)
	reURLCred = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`)
	reScheme  = regexp.MustCompile(`(?i)^(bearer|basic|token|apikey|api-key)?[\s:=]*$`)
)

// looksOpaque reports text that reads as a generated token: long, without
// spaces or slashes, and either mixed-case with digits or all hex (a
// UUID). A name like "mcp-server-postgres15" is not one.
func looksOpaque(v string) bool {
	if !reOpaque.MatchString(v) {
		return false
	}
	if reHexLike.MatchString(v) {
		return len(strings.ReplaceAll(v, "-", "")) >= 24
	}
	lower := strings.ContainsFunc(v, func(r rune) bool { return r >= 'a' && r <= 'z' })
	upper := strings.ContainsFunc(v, func(r rune) bool { return r >= 'A' && r <= 'Z' })
	return lower && upper && strings.ContainsAny(v, "0123456789")
}

// plainCredential says why a value written into a definition looks like a
// credential in plain text ("" = it does not). name is what the value is
// for. The check errs on the side of asking for a secret: a definition is
// stored, exported and recorded as written.
func plainCredential(name, v string) string {
	rest := reSecretRef.ReplaceAllString(v, "")
	switch {
	case reURLCred.MatchString(rest):
		return "holds a URL with a user and password in it"
	case credentialName(name) && !reScheme.MatchString(rest):
		// Around the secret only a scheme word may stand ("Bearer ").
		return "looks like a credential"
	case looksOpaque(strings.TrimSpace(rest)):
		return "holds what looks like a token"
	}
	return ""
}

// secretRefs lists the secrets a value names. A reference that is not a
// secret name is an error.
func secretRefs(v string) ([]string, error) {
	var out []string
	for _, m := range reSecretRef.FindAllStringSubmatch(v, -1) {
		if !reSecretName.MatchString(m[1]) {
			return nil, fmt.Errorf("%q is not a secret reference (${secret:NAME})", m[0])
		}
		out = append(out, m[1])
	}
	return out, nil
}

// SecretNames lists the secrets the definition names, sorted.
func (d MCPServerDef) SecretNames() []string {
	var out []string
	add := func(v string) {
		names, _ := secretRefs(v)
		for _, n := range names {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	add(d.URL)
	for _, v := range d.Headers {
		add(v)
	}
	for _, v := range d.Env {
		add(v)
	}
	sort.Strings(out)
	return out
}

// NormalizeMCPServer validates a definition and returns its stored form.
func NormalizeMCPServer(d MCPServerDef) (MCPServerDef, error) {
	out := MCPServerDef{Name: strings.TrimSpace(d.Name), Type: strings.ToLower(strings.TrimSpace(d.Type)), URL: strings.TrimSpace(d.URL)}
	bad := func(format string, args ...any) (MCPServerDef, error) {
		return MCPServerDef{}, fmt.Errorf("%w: mcp server %s: %s", ErrInvalid, out.Name, fmt.Sprintf(format, args...))
	}
	if !reMCPName.MatchString(out.Name) {
		return MCPServerDef{}, fmt.Errorf("%w: mcp server name %q: lower-case letters, digits, '_' and '-', at most 40", ErrInvalid, d.Name)
	}
	if out.Name == ReservedMCPName {
		return bad("the name is the worker's own: every run already reaches skopos under it")
	}
	if strings.Contains(out.Name, "__") {
		return bad("a name may not hold \"__\": a run's tools are named mcp__<server>__<tool>")
	}
	for _, p := range d.Workspaces {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := path.Match(p, "x"); err != nil || len(p) > 200 || !plainText(p) {
			return bad("workspaces: %q is not a pattern on a workspace id", p)
		}
		if !slices.Contains(out.Workspaces, p) {
			out.Workspaces = append(out.Workspaces, p)
		}
	}
	if len(out.Workspaces) > 50 {
		return bad("workspaces names %d patterns (at most 50)", len(out.Workspaces))
	}
	checkValue := func(what, name, v string, vars bool) error {
		if len(v) > 4000 || !plainText(v) {
			return fmt.Errorf("%s %s: the value must be one line of at most 4000 characters", what, name)
		}
		refs, err := secretRefs(v)
		if err != nil {
			return fmt.Errorf("%s %s: %v", what, name, err)
		}
		if why := plainCredential(name, v); why != "" || (credentialName(name) && len(refs) == 0) {
			return fmt.Errorf("%s %s %s: name a secret in it (${secret:NAME}) and set the value with `skopos mcp-server set-secret`, so it is never stored with the definition", what, name, cmp.Or(why, "looks like a credential"))
		}
		// ${NAME} is the worker's (a host path); it is only meaningful
		// where the worker runs something.
		rest := reSecretRef.ReplaceAllString(v, "")
		if names, err := VarRefs(rest); err != nil {
			return fmt.Errorf("%s %s: %v", what, name, err)
		} else if len(names) > 0 && !vars {
			return fmt.Errorf("%s %s: ${%s} is a worker variable, which only a command server's command and env take", what, name, names[0])
		}
		return nil
	}
	switch out.Type {
	case MCPHTTP:
		if len(d.Command) > 0 || len(d.Env) > 0 {
			return bad("an http server has a url and headers, not a command or env")
		}
		if _, err := secretRefs(out.URL); err != nil {
			return bad("url: %v", err)
		}
		u, err := url.Parse(reSecretRef.ReplaceAllString(out.URL, "x"))
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || len(out.URL) > 2000 || !plainText(out.URL) {
			return bad("url %q is not an http(s) URL", out.URL)
		}
		if u.User != nil {
			return bad("the url must not carry credentials: name a secret in a header instead")
		}
		if names, _ := VarRefs(reSecretRef.ReplaceAllString(out.URL, "")); len(names) > 0 {
			return bad("url: ${%s} is a worker variable, which only a command server's command and env take", names[0])
		}
		// A token in the path or the query string is a credential like one
		// in a header: it has to be a secret.
		raw, _ := url.Parse(out.URL)
		if raw != nil {
			for name, vals := range raw.Query() {
				for _, v := range vals {
					if why := plainCredential(name, v); why != "" || (credentialName(name) && !strings.Contains(v, "${secret:")) {
						return bad("url: the query parameter %s %s: name a secret in it (%s=${secret:NAME})", name, cmp.Or(why, "looks like a credential"), name)
					}
				}
			}
			for _, seg := range strings.Split(raw.Path, "/") {
				if looksOpaque(reSecretRef.ReplaceAllString(seg, "")) {
					return bad("url: a part of the path looks like a token: name a secret there (${secret:NAME})")
				}
			}
		}
		if len(d.Headers) > 20 {
			return bad("%d headers (at most 20)", len(d.Headers))
		}
		for k, v := range d.Headers {
			if !reHeaderName.MatchString(k) {
				return bad("header %q is not a header name", k)
			}
			if err := checkValue("header", k, v, false); err != nil {
				return bad("%v", err)
			}
			if out.Headers == nil {
				out.Headers = map[string]string{}
			}
			out.Headers[k] = v
		}
	case MCPCommand:
		if out.URL != "" || len(d.Headers) > 0 {
			return bad("a command server has a command and env, not a url or headers")
		}
		var err error
		if out.Command, err = normalizeArgv("command", d.Command, 100); err != nil {
			return bad("%v", err)
		}
		if len(out.Command) == 0 {
			return bad("command is empty")
		}
		for i, a := range out.Command {
			if refs, _ := secretRefs(a); len(refs) > 0 || strings.Contains(a, "${secret:") {
				return bad("the command may not name a secret (an argument shows in the process list): pass it through env")
			}
			// A credential on the command line would be stored here and
			// shown in every process list.
			if flag, val, isFlag := strings.Cut(a, "="); strings.HasPrefix(a, "-") && credentialName(strings.TrimLeft(flag, "-")) {
				return bad("the command passes a credential as an argument (%s): pass it through env, as a secret", flag)
			} else if (isFlag && looksOpaque(val)) || (i > 0 && looksOpaque(a)) || reURLCred.MatchString(a) {
				// (a flag's value, or a bare argument, that reads as a
				// generated token; an ordinary name or path does not)
				return bad("an argument of the command looks like a credential: pass it through env, as a secret")
			}
		}
		if len(d.Env) > maxStageEnv {
			return bad("%d env variables (at most %d)", len(d.Env), maxStageEnv)
		}
		for k, v := range d.Env {
			if !reEnvName.MatchString(k) {
				return bad("env %q is not a variable name", k)
			}
			if err := checkValue("env", k, v, true); err != nil {
				return bad("%v", err)
			}
			if out.Env == nil {
				out.Env = map[string]string{}
			}
			out.Env[k] = v
		}
	default:
		return bad("type %q is not one of http, command", out.Type)
	}
	if len(out.SecretNames()) > 0 && len(out.Workspaces) == 0 {
		return bad("it names secrets, so say which workspaces' runs may be given it: workspaces = [\"github.com/you/*\"] (\"*\" for every workspace). Its credentials go to every worker that runs one of them")
	}
	return out, nil
}

// SecretState says whether one of a server's secrets has a value. Never
// the value.
type SecretState struct {
	Name      string     `json:"name"`
	Set       bool       `json:"set"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
}

// MCPServerView is a definition with its revision and the state of its
// secrets.
type MCPServerView struct {
	MCPServerDef
	Rev       string        `json:"rev"`
	UpdatedAt time.Time     `json:"updated_at"`
	UpdatedBy string        `json:"updated_by,omitempty"`
	Secrets   []SecretState `json:"secrets"`
	// UsedBy: the owners ("workspace:<id>", "group:<id>") whose config
	// gives their runs the server.
	UsedBy []string `json:"used_by,omitempty"`
}

func mcpOwner(name string) string { return "mcp_server:" + name }

// SetSecrets wires the store the secrets' values live in.
func (s *Service) SetSecrets(store *secretbox.Store) { s.secrets = store }

// requireConfigReader: MCP server definitions are for those who configure
// runs — root, and keys with the admin permission.
func requireConfigReader(ctx context.Context) error {
	p := auth.PrincipalFromContext(ctx)
	if p == nil || p.Root || (p.Admin && p.Run == nil) {
		return nil
	}
	return auth.ErrAdminRequired
}

type mcpSetKey struct{}

// WithMCPSet makes config checks under ctx treat exactly these MCP server
// definitions as the ones that exist (see WithWrapperSet).
func WithMCPSet(ctx context.Context, defs map[string]MCPServerDef) context.Context {
	set := make(map[string]MCPServerDef, len(defs))
	for name, d := range defs {
		if norm, err := NormalizeMCPServer(func() MCPServerDef { d.Name = name; return d }()); err == nil {
			set[name] = norm
		}
	}
	return context.WithValue(ctx, mcpSetKey{}, set)
}

func (s *Service) mcpDefs(ctx context.Context) (map[string]MCPServerView, error) {
	heads, err := s.revs.Heads(ctx, KindMCPServer)
	if err != nil {
		return nil, err
	}
	out := make(map[string]MCPServerView, len(heads))
	for _, rev := range heads {
		var d MCPServerDef
		if err := json.Unmarshal(rev.Content, &d); err != nil {
			return nil, fmt.Errorf("stored mcp server definition %s: %w", rev.ID, err)
		}
		out[d.Name] = MCPServerView{MCPServerDef: d, Rev: rev.ID, UpdatedAt: rev.CreatedAt, UpdatedBy: rev.Actor}
	}
	return out, nil
}

func (s *Service) secretStates(ctx context.Context, d MCPServerDef) ([]SecretState, error) {
	names := d.SecretNames()
	out := make([]SecretState, 0, len(names))
	if len(names) == 0 {
		return out, nil
	}
	have := map[string]secretbox.Meta{}
	if s.secrets != nil {
		var err error
		if have, err = s.secrets.List(ctx, mcpOwner(d.Name)); err != nil {
			return nil, err
		}
	}
	for _, n := range names {
		st := SecretState{Name: n}
		if m, ok := have[n]; ok {
			at := m.UpdatedAt
			st.Set, st.UpdatedAt, st.UpdatedBy = true, &at, m.UpdatedBy
		}
		out = append(out, st)
	}
	return out, nil
}

// MCPServers lists the definitions with the state of their secrets, by
// name. For root and admin keys.
func (s *Service) MCPServers(ctx context.Context) ([]MCPServerView, error) {
	if err := requireConfigReader(ctx); err != nil {
		return nil, err
	}
	defs, err := s.mcpDefs(ctx)
	if err != nil {
		return nil, err
	}
	users := map[string][]string{}
	if auth.RequireFleetAdmin(ctx) == nil {
		if users, err = s.MCPUsers(ctx); err != nil {
			return nil, err
		}
	}
	out := make([]MCPServerView, 0, len(defs))
	for _, v := range defs {
		if v.Secrets, err = s.secretStates(ctx, v.MCPServerDef); err != nil {
			return nil, err
		}
		v.UsedBy = users[v.Name]
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// MCPUsers returns, per MCP server name, the owners whose stored config
// gives their runs the server. Internal: no scope check.
func (s *Service) MCPUsers(ctx context.Context) (map[string][]string, error) {
	heads, err := s.revs.Heads(ctx, KindWorkspaceConfig)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for owner, rev := range heads {
		c, err := decode(rev)
		if err != nil {
			return nil, err
		}
		if c.MCP == nil {
			continue
		}
		for _, name := range *c.MCP {
			out[name] = append(out[name], owner)
		}
	}
	for name := range out {
		sort.Strings(out[name])
	}
	return out, nil
}

// checkMCP refuses a config that gives its runs an MCP server that is not
// defined, or one that may not be given to the workspaces the config
// reaches (workspaces: the workspace itself, or a group's members).
func (s *Service) checkMCP(ctx context.Context, c *Config, workspaces []string) error {
	if c == nil || c.MCP == nil || len(*c.MCP) == 0 {
		return nil
	}
	defs, err := s.mcpDefs(ctx)
	if err != nil {
		return err
	}
	pending, isPending := ctx.Value(mcpSetKey{}).(map[string]MCPServerDef)
	for _, name := range *c.MCP {
		var def MCPServerDef
		if isPending {
			d, ok := pending[name]
			if !ok {
				return fmt.Errorf("%w: mcp: server %q is not defined (define it first: skopos mcp-server set %s …)", ErrInvalid, name, name)
			}
			def = d
		} else {
			v, ok := defs[name]
			if !ok {
				return fmt.Errorf("%w: mcp: server %q is not defined (define it first: skopos mcp-server set %s …)", ErrInvalid, name, name)
			}
			def = v.MCPServerDef
		}
		for _, ws := range workspaces {
			if !def.AllowsWorkspace(ws) {
				return fmt.Errorf("%w: mcp: server %s may not be given to the runs of %s (its definition lists the workspaces it is for; an admin over every workspace changes that with skopos mcp-server set)", ErrInvalid, name, ws)
			}
		}
	}
	return nil
}

// PlanMCPServer checks a definition as SetMCPServer would and reports
// whether saving it would change anything ("create", "update" or ""),
// writing nothing.
func (s *Service) PlanMCPServer(ctx context.Context, d MCPServerDef) (string, error) {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return "", err
	}
	norm, err := NormalizeMCPServer(d)
	if err != nil {
		return "", err
	}
	content, _ := json.Marshal(norm)
	before, err := s.revs.Head(ctx, KindMCPServer, mcpOwner(norm.Name))
	switch {
	case err != nil:
		return "", err
	case before == nil:
		return "create", nil
	case before.Hash != confrev.Hash(content):
		return "update", nil
	}
	return "", nil
}

// SetMCPServer defines or redefines an MCP server. Fleet admins only: a
// command server runs on workers, and an http server receives what agents
// send it. Secrets the new definition no longer names are deleted.
func (s *Service) SetMCPServer(ctx context.Context, d MCPServerDef, via string) (*MCPServerView, bool, error) {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return nil, false, err
	}
	via = audit.CleanVia(via)
	norm, err := NormalizeMCPServer(d)
	if err != nil {
		return nil, false, err
	}
	content, _ := json.Marshal(norm)
	owner := mcpOwner(norm.Name)
	before, err := s.revs.Head(ctx, KindMCPServer, owner)
	if err != nil {
		return nil, false, err
	}
	actor, keyID := auth.Actor(ctx)
	rev, changed, err := s.revs.Put(ctx, KindMCPServer, owner, content, confrev.Meta{Actor: actor, ActorKeyID: keyID, Via: via})
	if err != nil {
		return nil, false, err
	}
	if changed {
		action, was := "mcp_server.create", "none"
		if before != nil {
			action, was = "mcp_server.update", before.ID
		}
		notes := "revision " + rev.ID + " (was " + was + "): " + describeMCP(norm)
		if s.secrets != nil {
			// A stored value is for the server it was set for. A new
			// definition starts without any (one left by an earlier server
			// of the name must not attach to it), and one that sends the
			// secret somewhere else — another host, another command — has
			// to be given it again.
			keep := norm.SecretNames()
			moved := false
			if before == nil {
				keep = nil
			} else {
				var prev MCPServerDef
				if json.Unmarshal(before.Content, &prev) != nil || !sameTarget(prev, norm) {
					keep, moved = nil, true
				}
			}
			gone, err := s.secrets.Keep(ctx, owner, keep)
			if err != nil {
				return nil, false, err
			}
			switch {
			case len(gone) > 0 && moved:
				notes += "; the server's address changed, so its stored secrets were deleted (set them again): " + strings.Join(gone, ", ")
			case len(gone) > 0:
				notes += "; secrets no longer named were deleted: " + strings.Join(gone, ", ")
			}
		}
		s.recordMCP(ctx, norm.Name, action, via, notes)
		s.mcpChanged()
	}
	v := &MCPServerView{MCPServerDef: norm, Rev: rev.ID, UpdatedAt: rev.CreatedAt, UpdatedBy: rev.Actor}
	if v.Secrets, err = s.secretStates(ctx, norm); err != nil {
		return nil, false, err
	}
	return v, changed, nil
}

// sameTarget reports whether two definitions of a server send its secrets
// to the same place: the same kind, and the same host for an http server
// or the same command for a command server.
func sameTarget(a, b MCPServerDef) bool {
	if a.Type != b.Type {
		return false
	}
	if a.Type == MCPCommand {
		return slices.Equal(a.Command, b.Command)
	}
	ua, erra := url.Parse(reSecretRef.ReplaceAllString(a.URL, "x"))
	ub, errb := url.Parse(reSecretRef.ReplaceAllString(b.URL, "x"))
	return erra == nil && errb == nil && ua.Scheme == ub.Scheme && ua.Host == ub.Host
}

// DeleteMCPServer removes a definition and its secrets. A server a stored
// config still gives its runs is refused unless force is set: those runs
// would go without it.
func (s *Service) DeleteMCPServer(ctx context.Context, name, via string, force bool) (bool, error) {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return false, err
	}
	name = strings.TrimSpace(name)
	if !reMCPName.MatchString(name) {
		return false, fmt.Errorf("%w: mcp server %s", ErrNotFound, name)
	}
	if head, err := s.revs.Head(ctx, KindMCPServer, mcpOwner(name)); err != nil {
		return false, err
	} else if head == nil {
		return false, fmt.Errorf("%w: mcp server %s is not defined", ErrNotFound, name)
	}
	users, err := s.MCPUsers(ctx)
	if err != nil {
		return false, err
	}
	if in := users[name]; len(in) > 0 && !force {
		return false, fmt.Errorf("%w: mcp server %s is given to the runs of %s. Take it out of their config first, or force the delete (skopos mcp-server delete --force)", ErrConflict, name, strings.Join(in, ", "))
	}
	was, err := s.revs.Clear(ctx, KindMCPServer, mcpOwner(name))
	if err != nil {
		return false, err
	}
	if was == nil {
		return false, fmt.Errorf("%w: mcp server %s is not defined", ErrNotFound, name)
	}
	if s.secrets != nil {
		if err := s.secrets.DeleteOwner(ctx, mcpOwner(name)); err != nil {
			return false, err
		}
	}
	notes := "was revision " + was.ID + "; its secrets were deleted"
	if in := users[name]; len(in) > 0 {
		notes += "; forced while given to the runs of " + strings.Join(in, ", ")
	}
	s.recordMCP(ctx, name, "mcp_server.delete", audit.CleanVia(via), notes)
	s.mcpChanged()
	return true, nil
}

// ErrNoSecretKey: the server has no secret key, so it stores no secrets.
var ErrNoSecretKey = secretbox.ErrNoKey

// SetMCPSecret stores the value of a secret a definition names. The value
// is sealed, never shown again and never recorded: the audit event names
// the secret only.
func (s *Service) SetMCPSecret(ctx context.Context, server, name string, value []byte, via string) error {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return err
	}
	def, err := s.mcpDef(ctx, server)
	if err != nil {
		return err
	}
	if !slices.Contains(def.SecretNames(), name) {
		return fmt.Errorf("%w: mcp server %s names no secret %q (it names: %s)", ErrInvalid, server, name, orNone(def.SecretNames()))
	}
	if s.secrets == nil || !s.secrets.Ring().Enabled() {
		return ErrNoSecretKey
	}
	// What a terminal or a file adds around the value is not part of it,
	// and the worker's redactor matches the value without it.
	value = []byte(strings.TrimSpace(string(value)))
	switch {
	case len(value) < 8:
		// A worker keeps a value out of a run's log only from this length
		// up: shorter text is everywhere.
		return fmt.Errorf("%w: the secret's value is shorter than 8 characters", ErrInvalid)
	case strings.Contains(string(value), "${") || !plainText(string(value)):
		return fmt.Errorf("%w: the secret's value must be one line and may not contain ${", ErrInvalid)
	}
	actor, _ := auth.Actor(ctx)
	if len(value) > secretbox.MaxValueBytes {
		return fmt.Errorf("%w: the secret's value is %d bytes (at most %d)", ErrInvalid, len(value), secretbox.MaxValueBytes)
	}
	if err := s.secrets.Set(ctx, mcpOwner(def.Name), name, value, actor); err != nil {
		return err
	}
	s.recordMCP(ctx, def.Name, "mcp_server.secret_set", audit.CleanVia(via), "secret "+name+" (sealed under key "+s.secrets.Ring().Current()+")")
	return nil
}

// UnsetMCPSecret deletes a secret's stored value.
func (s *Service) UnsetMCPSecret(ctx context.Context, server, name, via string) error {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return err
	}
	def, err := s.mcpDef(ctx, server)
	if err != nil {
		return err
	}
	if s.secrets == nil {
		return fmt.Errorf("%w: secret %s of mcp server %s is not set", ErrNotFound, name, server)
	}
	had, err := s.secrets.Delete(ctx, mcpOwner(def.Name), name)
	if err != nil {
		return err
	}
	if !had {
		return fmt.Errorf("%w: secret %s of mcp server %s is not set", ErrNotFound, name, server)
	}
	s.recordMCP(ctx, def.Name, "mcp_server.secret_unset", audit.CleanVia(via), "secret "+name)
	return nil
}

func (s *Service) mcpDef(ctx context.Context, name string) (*MCPServerDef, error) {
	name = strings.TrimSpace(name)
	if !reMCPName.MatchString(name) {
		return nil, fmt.Errorf("%w: mcp server %s", ErrNotFound, name)
	}
	head, err := s.revs.Head(ctx, KindMCPServer, mcpOwner(name))
	if err != nil {
		return nil, err
	}
	if head == nil {
		return nil, fmt.Errorf("%w: mcp server %s is not defined", ErrNotFound, name)
	}
	var d MCPServerDef
	if err := json.Unmarshal(head.Content, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func (s *Service) mcpChanged() {
	if s.publisher != nil {
		s.publisher.Publish(events.Event{Type: events.TypeWorkspaces})
	}
}

func (s *Service) recordMCP(ctx context.Context, name, action, via, notes string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Record(ctx, audit.RecordInput{EntityType: MCPAuditEntity, EntityID: name, Action: action, Via: via, Notes: notes})
}

// describeMCP renders a definition for an audit note: it holds secret
// names, never values.
func describeMCP(d MCPServerDef) string {
	var parts []string
	if d.Type == MCPHTTP {
		parts = append(parts, "http "+d.URL)
		if len(d.Headers) > 0 {
			parts = append(parts, "headers "+Describe(mustJSON(d.Headers)))
		}
	} else {
		parts = append(parts, "command "+Describe(mustJSON(d.Command)))
		if len(d.Env) > 0 {
			parts = append(parts, "env "+Describe(mustJSON(d.Env)))
		}
	}
	if names := d.SecretNames(); len(names) > 0 {
		parts = append(parts, "secrets "+strings.Join(names, ", "))
	}
	if len(d.Workspaces) > 0 {
		parts = append(parts, "for workspaces "+strings.Join(d.Workspaces, ", "))
	}
	out := strings.Join(parts, ", ")
	if len(out) > 1500 {
		out = out[:1500] + "… (the revision holds the rest)"
	}
	return out
}

// MCPManaged reports whether the server holds the MCP servers of a
// workspace's runs: its config, or a group's under it, sets `mcp`. A
// workspace's runs then get exactly the servers listed (none for an empty
// list) and nothing from a worker's own mcp_config.
func (s *Service) MCPManaged(ctx context.Context, ws string) bool {
	eff, err := s.Effective(ctx, ws)
	// Not readable: treated as held, so no worker falls back to its file.
	return err != nil || (eff.Managed && eff.Config.MCP != nil)
}

// RunMCP resolves the MCP servers a run in the workspace gets, with their
// secrets filled in: what a claim answer carries, and nothing else ever
// does. nil = the server does not hold the workspace's MCP servers (the
// worker's own mcp_config applies). A server that cannot be delivered is
// left out with a note — the run never gets it half-configured:
//   - a command server, for a worker that does not take commands from the
//     server;
//   - a server one of whose secrets is not set, or cannot be opened.
func (s *Service) RunMCP(ctx context.Context, ws string, serverCommands bool) (*runs.RunMCP, error) {
	eff, err := s.Effective(ctx, ws)
	if err != nil {
		return nil, err
	}
	if !eff.Managed || eff.Config.MCP == nil {
		return nil, nil
	}
	out := &runs.RunMCP{Servers: map[string]json.RawMessage{}}
	defs, err := s.mcpDefs(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range *eff.Config.MCP {
		v, ok := defs[name]
		if !ok {
			out.Notes = append(out.Notes, fmt.Sprintf("mcp server %s is no longer defined: left out", name))
			continue
		}
		d := v.MCPServerDef
		// Checked when the config was saved, and again here: the
		// definition or a group's members may have changed since.
		if !d.AllowsWorkspace(ws) {
			out.Notes = append(out.Notes, fmt.Sprintf("mcp server %s may not be given to this workspace's runs (its definition lists the workspaces it is for): left out", name))
			continue
		}
		if d.Type == MCPCommand && !serverCommands {
			out.Notes = append(out.Notes, fmt.Sprintf("mcp server %s runs a command on the worker, and this worker does not take commands from the server (worker.server_commands): left out", name))
			continue
		}
		values := map[string]string{}
		problem := ""
		for _, sn := range d.SecretNames() {
			if s.secrets == nil || !s.secrets.Ring().Enabled() {
				problem = "the server has no secret key"
				break
			}
			val, err := s.secrets.Get(ctx, mcpOwner(name), sn)
			if errors.Is(err, secretbox.ErrNotSet) {
				problem = "its secret " + sn + " is not set"
				break
			}
			if err != nil {
				// What stands in the way is said; the error's text never
				// holds a value.
				problem = "its secret " + sn + " cannot be opened (" + err.Error() + ")"
				break
			}
			values[sn] = string(val)
		}
		if problem != "" {
			out.Notes = append(out.Notes, fmt.Sprintf("mcp server %s: %s: left out", name, problem))
			continue
		}
		fill := func(v string) string {
			return reSecretRef.ReplaceAllStringFunc(v, func(ref string) string { return values[ref[len("${secret:"):len(ref)-1]] })
		}
		var entry map[string]any
		if d.Type == MCPHTTP {
			entry = map[string]any{"type": "http", "url": fill(d.URL)}
			if len(d.Headers) > 0 {
				h := make(map[string]string, len(d.Headers))
				for k, v := range d.Headers {
					h[k] = fill(v)
				}
				entry["headers"] = h
			}
		} else {
			entry = map[string]any{"command": d.Command[0], "args": d.Command[1:]}
			if len(d.Env) > 0 {
				e := make(map[string]string, len(d.Env))
				for k, v := range d.Env {
					e[k] = fill(v)
				}
				entry["env"] = e
			}
		}
		buf, _ := json.Marshal(entry)
		out.Servers[name] = buf
		for _, sn := range d.SecretNames() {
			if !slices.Contains(out.Secrets, values[sn]) {
				out.Secrets = append(out.Secrets, values[sn])
			}
		}
	}
	sort.Strings(out.Notes)
	return out, nil
}
