package wsconfig

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/secretbox"
)

func TestNormalizeMCPServer(t *testing.T) {
	bad := map[string]MCPServerDef{
		"no type":                {Name: "x", URL: "https://x/mcp"},
		"bad name":               {Name: "Not A Name", Type: MCPHTTP, URL: "https://x/mcp"},
		"the worker's own name":  {Name: ReservedMCPName, Type: MCPHTTP, URL: "https://x/mcp"},
		"not a url":              {Name: "x", Type: MCPHTTP, URL: "x/mcp"},
		"credentials in the url": {Name: "x", Type: MCPHTTP, URL: "https://user:pass@x/mcp"},
		"http with a command":    {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Command: []string{"x"}},
		"an inline credential":   {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"Authorization": "Bearer abc123"}},
		"an inline api key":      {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"X-Api-Key": "abc123"}},
		"a bad secret reference": {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"Authorization": "Bearer ${secret:not a name}"}},
		"a worker variable":      {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"X-Dir": "${TOOLS}"}},
		"a bad header name":      {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"Bad Header": "x"}},
		"command without argv":   {Name: "x", Type: MCPCommand},
		"command with a url":     {Name: "x", Type: MCPCommand, Command: []string{"x"}, URL: "https://x"},
		"a secret in the argv":   {Name: "x", Type: MCPCommand, Command: []string{"x", "--token", "${secret:token}"}},
		"an inline env secret":   {Name: "x", Type: MCPCommand, Command: []string{"x"}, Env: map[string]string{"API_TOKEN": "abc123"}},
		"a bad env name":         {Name: "x", Type: MCPCommand, Command: []string{"x"}, Env: map[string]string{"A B": "1"}},
		// A credential anywhere in the definition, by the look of it.
		"a key in the query":       {Name: "x", Type: MCPHTTP, URL: "https://x/mcp?api_key=sk-live-abc"},
		"a token in the query":     {Name: "x", Type: MCPHTTP, URL: "https://x/mcp?v=1&access_token=abc"},
		"a token in the path":      {Name: "x", Type: MCPHTTP, URL: "https://x/hooks/T0aB3cD4eF5gH6iJ7kL8mN9/mcp"},
		"a uuid in the path":       {Name: "x", Type: MCPHTTP, URL: "https://x/0f8fad5b-d9cb-469f-a165-70867728950e/mcp"},
		"a session header":         {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"X-Session": "abc"}},
		"a bearer header":          {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"X-Bearer": "abc"}},
		"a key header":             {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"Key": "abc"}},
		"plain text beside a ref":  {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"Authorization": "Bearer abc123 ${secret:unused}"}, Workspaces: []string{"*"}},
		"an opaque header value":   {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"X-Tenant": "aB3cD4eF5gH6iJ7kL8mN9oP0"}},
		"a worker variable in url": {Name: "x", Type: MCPHTTP, URL: "https://${HOST}/mcp"},
		"a flag with a key":        {Name: "x", Type: MCPCommand, Command: []string{"x", "--api-key=abc"}},
		"a token flag":             {Name: "x", Type: MCPCommand, Command: []string{"x", "--token", "abc"}},
		"an opaque argument":       {Name: "x", Type: MCPCommand, Command: []string{"x", "aB3cD4eF5gH6iJ7kL8mN9oP0"}},
		"a password variable":      {Name: "x", Type: MCPCommand, Command: []string{"x"}, Env: map[string]string{"DB_PASS": "abc"}},
		"a pat":                    {Name: "x", Type: MCPCommand, Command: []string{"x"}, Env: map[string]string{"GITHUB_PAT": "abc"}},
		"a private key":            {Name: "x", Type: MCPCommand, Command: []string{"x"}, Env: map[string]string{"PRIVATE_KEY_PEM": "abc"}},
		"a url with a password":    {Name: "x", Type: MCPCommand, Command: []string{"x"}, Env: map[string]string{"DATABASE_URL": "postgres://u:p@db/x"}},
		"a double underscore":      {Name: "a__b", Type: MCPHTTP, URL: "https://x/mcp"},
		// Secrets go to every worker that runs a workspace the server is
		// given to: a server with secrets has to say which those are.
		"secrets without workspaces": {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"Authorization": "Bearer ${secret:token}"}},
		"a bad workspace pattern":    {Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Workspaces: []string{"fabrikam/[a"}},
	}
	// What is not a credential stays writable as it is.
	for name, d := range map[string]MCPServerDef{
		"a tenant header":   {Name: "x", Type: MCPHTTP, URL: "https://x/v2/mcp?mode=readonly", Headers: map[string]string{"X-Tenant": "globex", "Accept": "application/json"}},
		"a named package":   {Name: "x", Type: MCPCommand, Command: []string{"npx", "-y", "mcp-server-postgres15-readonly", "--port=8080"}},
		"paths and options": {Name: "x", Type: MCPCommand, Command: []string{"${TOOLS_DIR}/scripts", "mcp", "--data", "/opt/tools/scripts/data/v2"}, Env: map[string]string{"DATA": "/var/lib/scripts/data2", "NODE_ENV": "production"}},
	} {
		if _, err := NormalizeMCPServer(d); err != nil {
			t.Errorf("%s: refused (%v)", name, err)
		}
	}
	for name, d := range bad {
		if _, err := NormalizeMCPServer(d); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	http, err := NormalizeMCPServer(MCPServerDef{Name: "router", Type: " HTTP ", URL: " https://router.example.com/mcp?key=${secret:extra} ",
		Headers: map[string]string{"Authorization": "Bearer ${secret:token}", "X-Tenant": "globex", "X-Extra-Key": "${secret:extra}"}, Workspaces: []string{" fabrikam/* ", "fabrikam/*", ""}})
	if err != nil {
		t.Fatal(err)
	}
	if http.Type != MCPHTTP || http.URL != "https://router.example.com/mcp?key=${secret:extra}" || strings.Join(http.SecretNames(), ",") != "extra,token" || strings.Join(http.Workspaces, ",") != "fabrikam/*" {
		t.Fatalf("http: %+v %v", http, http.SecretNames())
	}
	cmd, err := NormalizeMCPServer(MCPServerDef{Name: "scripts", Type: MCPCommand, Command: []string{"${TOOLS_DIR}/scripts", "mcp"},
		Env: map[string]string{"SCRIPTS_TOKEN": "${secret:token}", "DATA": "${DATA_DIR}/s"}, Workspaces: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cmd.SecretNames(), ",") != "token" {
		t.Fatalf("command: %v", cmd.SecretNames())
	}
	// The stored form holds names, never anything a value could hide in.
	stored, _ := json.Marshal(http)
	if !strings.Contains(string(stored), "${secret:token}") {
		t.Fatalf("stored form: %s", stored)
	}
}

func mcpFixture(t *testing.T, key string) (*Service, *recSink, *secretbox.Store) {
	t.Helper()
	s, sqlDB, sink := fixture(t)
	store := secretbox.NewStore(sqlDB, secretbox.New(key))
	s.SetSecrets(store)
	return s, sink, store
}

func mcpConfig(servers ...string) Config {
	allow := make([]string, len(servers))
	for i, n := range servers {
		allow[i] = "mcp__" + n
	}
	return Config{MCP: &servers, MCPServers: &allow}
}

// Definitions and secrets: who may change them, what is recorded (names,
// never values), and what a run gets.
func TestMCPServersAndSecrets(t *testing.T) {
	s, sink, _ := mcpFixture(t, "test key")
	ctx := root()
	router := MCPServerDef{Name: "router", Type: MCPHTTP, URL: "https://router.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer ${secret:token}"},
		Workspaces: []string{"fabrikam/api"}}
	// Fleet admins only.
	if _, _, err := s.SetMCPServer(key(true, false, "fabrikam/api"), router, "cli"); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a workspace admin defined an MCP server: %v", err)
	}
	if _, err := s.MCPServers(key(false, true)); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a key without the admin permission listed MCP servers: %v", err)
	}
	// A config cannot name a server that is not defined.
	if _, err := s.Set(ctx, KindWorkspace, "fabrikam/api", mcpConfig("router"), "cli", ""); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("an undefined server: %v", err)
	}
	v, changed, err := s.SetMCPServer(key(true, true), router, "cli")
	if err != nil || !changed || len(v.Secrets) != 1 || v.Secrets[0].Name != "token" || v.Secrets[0].Set {
		t.Fatalf("set: %+v %v", v, err)
	}
	if e := sink.last(); e.EntityType != MCPAuditEntity || e.Action != "mcp_server.create" || !strings.Contains(e.Notes, "${secret:token}") || !strings.Contains(e.Notes, "secrets token") {
		t.Fatalf("audit: %+v", e)
	}
	// A server the runs get needs an allow rule for its tools.
	onlyAttached := []string{"router"}
	if _, err := s.Set(ctx, KindWorkspace, "fabrikam/api", Config{MCP: &onlyAttached}, "cli", ""); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "mcp__router") {
		t.Fatalf("attached and not allowed: %v", err)
	}
	if _, err := s.Set(ctx, KindWorkspace, "fabrikam/api", mcpConfig("router"), "cli", ""); err != nil {
		t.Fatal(err)
	}

	// The secret is not set: the run goes without the server, and is told.
	mcp, err := s.RunMCP(ctx, "fabrikam/api", true)
	if err != nil || mcp == nil || len(mcp.Servers) != 0 || len(mcp.Notes) != 1 || !strings.Contains(mcp.Notes[0], "secret token is not set") {
		t.Fatalf("without the secret: %+v %v", mcp, err)
	}
	const value = "tok-3f9a-very-secret"
	if err := s.SetMCPSecret(key(true, false, "fabrikam/api"), "router", "token", []byte(value), "cli"); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a workspace admin set a secret: %v", err)
	}
	if err := s.SetMCPSecret(ctx, "router", "nope", []byte(value), "cli"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a secret the definition does not name: %v", err)
	}
	if err := s.SetMCPSecret(ctx, "router", "token", []byte("short"), "cli"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a value too short to keep out of logs: %v", err)
	}
	if err := s.SetMCPSecret(ctx, "router", "token", []byte("has ${VAR} in it"), "cli"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a value with ${: %v", err)
	}
	if err := s.SetMCPSecret(ctx, "router", "token", []byte(value+"\n"), "cli"); err != nil {
		t.Fatal(err)
	}
	if e := sink.last(); e.Action != "mcp_server.secret_set" || !strings.Contains(e.Notes, "secret token") || strings.Contains(e.Notes, value) {
		t.Fatalf("the audit event names the secret, never its value: %+v", e)
	}
	mcp, err = s.RunMCP(ctx, "fabrikam/api", false)
	if err != nil || len(mcp.Servers) != 1 || len(mcp.Notes) != 0 || len(mcp.Secrets) != 1 || mcp.Secrets[0] != value {
		t.Fatalf("with the secret: %+v %v", mcp, err)
	}
	var entry struct {
		Type, URL string
		Headers   map[string]string
	}
	if err := json.Unmarshal(mcp.Servers["router"], &entry); err != nil || entry.Type != "http" || entry.Headers["Authorization"] != "Bearer "+value {
		t.Fatalf("the delivered server: %s %v", mcp.Servers["router"], err)
	}
	// Only for the workspace whose config names it.
	if other, err := s.RunMCP(ctx, "fabrikam/web", true); err != nil || other != nil {
		t.Fatalf("a workspace the server holds no MCP servers for: %+v %v", other, err)
	}
	if !s.MCPManaged(ctx, "fabrikam/api") || s.MCPManaged(ctx, "fabrikam/web") {
		t.Fatal("which workspaces' MCP servers the server holds")
	}
	// An empty list is held too: the runs get none, and no worker's file.
	if _, err := s.Set(ctx, KindWorkspace, "fabrikam/web", Config{MCP: &[]string{}}, "cli", ""); err != nil {
		t.Fatal(err)
	}
	if none, err := s.RunMCP(ctx, "fabrikam/web", true); err != nil || none == nil || len(none.Servers) != 0 || !s.MCPManaged(ctx, "fabrikam/web") {
		t.Fatalf("an empty list: %+v %v", none, err)
	}

	// Nothing that reads the definitions shows the value.
	views, err := s.MCPServers(ctx)
	if err != nil || len(views) != 1 || !views[0].Secrets[0].Set || strings.Join(views[0].UsedBy, ",") != "workspace:fabrikam/api" {
		t.Fatalf("list: %+v %v", views, err)
	}
	buf, _ := json.Marshal(views)
	if strings.Contains(string(buf), value) {
		t.Fatalf("the list holds the value: %s", buf)
	}
	for _, e := range sink.entries {
		if strings.Contains(e.Notes, value) {
			t.Fatalf("an audit note holds the value: %+v", e)
		}
	}

	// A command server goes only to a worker that takes commands.
	tool := MCPServerDef{Name: "scripts", Type: MCPCommand, Command: []string{"${TOOLS_DIR}/scripts", "mcp"}, Env: map[string]string{"DATA": "${DATA_DIR}/s"}}
	if _, _, err := s.SetMCPServer(ctx, tool, "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, KindWorkspace, "fabrikam/api", mcpConfig("router", "scripts"), "cli", ""); err != nil {
		t.Fatal(err)
	}
	without, _ := s.RunMCP(ctx, "fabrikam/api", false)
	if len(without.Servers) != 1 || len(without.Notes) != 1 || !strings.Contains(without.Notes[0], "server_commands") {
		t.Fatalf("a command server without server_commands: %+v", without)
	}
	with, _ := s.RunMCP(ctx, "fabrikam/api", true)
	var cmd struct {
		Command string
		Args    []string
		Env     map[string]string
	}
	if err := json.Unmarshal(with.Servers["scripts"], &cmd); err != nil || cmd.Command != "${TOOLS_DIR}/scripts" || cmd.Args[0] != "mcp" || cmd.Env["DATA"] != "${DATA_DIR}/s" {
		t.Fatalf("the worker's variables are left for the worker: %s %v", with.Servers["scripts"], err)
	}

	// The server is for fabrikam/api only: no other workspace's admin can give it
	// to its runs, and a group that reaches another workspace cannot either.
	if _, err := s.Set(ctx, KindWorkspace, "fabrikam/web", mcpConfig("router"), "cli", ""); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "may not be given to the runs of fabrikam/web") {
		t.Fatalf("a server given to a workspace it is not for: %v", err)
	}
	if _, err := s.Set(ctx, KindGroup, "globex", mcpConfig("router"), "cli", ""); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "fabrikam/web") {
		t.Fatalf("a group that reaches a workspace the server is not for: %v", err)
	}
	// The definition is narrowed after a config named it: the run goes
	// without the server, and is told.
	elsewhere := router
	elsewhere.Workspaces = []string{"other/*"}
	if _, _, err := s.SetMCPServer(ctx, elsewhere, "cli"); err != nil {
		t.Fatal(err)
	}
	if mcp, _ := s.RunMCP(ctx, "fabrikam/api", true); mcp.Servers["router"] != nil || !strings.Contains(strings.Join(mcp.Notes, " "), "may not be given to this workspace") {
		t.Fatalf("a server no longer for the workspace: %+v", mcp)
	}
	if _, _, err := s.SetMCPServer(ctx, router, "cli"); err != nil {
		t.Fatal(err)
	}
	if mcp, _ := s.RunMCP(ctx, "fabrikam/api", true); mcp.Servers["router"] == nil {
		t.Fatalf("the workspaces a server is for do not touch its secrets: %+v", mcp)
	}
	// A secret is for the server it was set for: a definition that sends
	// it to another host has to be given it again.
	moved := router
	moved.URL = "https://elsewhere.example.com/mcp"
	if _, _, err := s.SetMCPServer(ctx, moved, "cli"); err != nil {
		t.Fatal(err)
	}
	if e := sink.last(); !strings.Contains(e.Notes, "address changed") || !strings.Contains(e.Notes, "token") {
		t.Fatalf("audit: %+v", e)
	}
	if mcp, _ := s.RunMCP(ctx, "fabrikam/api", true); mcp.Servers["router"] != nil || !strings.Contains(strings.Join(mcp.Notes, " "), "secret token is not set") {
		t.Fatalf("the secret followed the server to another host: %+v", mcp)
	}
	if _, _, err := s.SetMCPServer(ctx, router, "cli"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMCPSecret(ctx, "router", "token", []byte("  "+value+" \t"), "cli"); err != nil {
		t.Fatal(err)
	}
	if mcp, _ := s.RunMCP(ctx, "fabrikam/api", true); len(mcp.Secrets) != 1 || mcp.Secrets[0] != value {
		t.Fatalf("a padded value is stored without its padding: %+v", mcp.Secrets)
	}

	// Redefining without the secret deletes its value; deleting a server
	// in use is refused.
	if _, err := s.DeleteMCPServer(ctx, "router", "cli", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting a server in use: %v", err)
	}
	plain := router
	plain.Headers = map[string]string{"X-Tenant": "globex"}
	if _, _, err := s.SetMCPServer(ctx, plain, "cli"); err != nil {
		t.Fatal(err)
	}
	if e := sink.last(); e.Action != "mcp_server.update" || !strings.Contains(e.Notes, "deleted: token") {
		t.Fatalf("audit: %+v", e)
	}
	if _, _, err := s.SetMCPServer(ctx, router, "cli"); err != nil {
		t.Fatal(err)
	}
	if mcp, _ := s.RunMCP(ctx, "fabrikam/api", true); len(mcp.Servers["router"]) != 0 {
		t.Fatal("a secret came back with the definition that names it again")
	}
	if err := s.UnsetMCPSecret(ctx, "router", "token", "cli"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unsetting a secret that is not set: %v", err)
	}
	if _, err := s.DeleteMCPServer(ctx, "router", "cli", true); err != nil {
		t.Fatal(err)
	}
	if mcp, _ := s.RunMCP(ctx, "fabrikam/api", true); len(mcp.Notes) == 0 || !strings.Contains(strings.Join(mcp.Notes, " "), "no longer defined") {
		t.Fatalf("a deleted server a config still names: %+v", mcp)
	}
}

// Without a secret key the server stores no secret, and a server that
// needs one is left out — never replaced by a worker's own.
func TestMCPWithoutASecretKey(t *testing.T) {
	s, _, _ := mcpFixture(t, "")
	ctx := root()
	for _, d := range []MCPServerDef{
		{Name: "needs", Type: MCPHTTP, URL: "https://a/mcp", Headers: map[string]string{"Authorization": "Bearer ${secret:token}"}, Workspaces: []string{"*"}},
		{Name: "open", Type: MCPHTTP, URL: "https://b/mcp"},
	} {
		if _, _, err := s.SetMCPServer(ctx, d, "cli"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetMCPSecret(ctx, "needs", "token", []byte("a-long-enough-value"), "cli"); !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("a secret stored without a key: %v", err)
	}
	if _, err := s.Set(ctx, KindWorkspace, "fabrikam/api", mcpConfig("needs", "open"), "cli", ""); err != nil {
		t.Fatal(err)
	}
	mcp, err := s.RunMCP(ctx, "fabrikam/api", true)
	if err != nil || mcp == nil || len(mcp.Servers) != 1 || mcp.Servers["open"] == nil || !strings.Contains(strings.Join(mcp.Notes, " "), "no secret key") {
		t.Fatalf("without a key: %+v %v", mcp, err)
	}
	if !s.MCPManaged(ctx, "fabrikam/api") {
		t.Fatal("the server still holds the workspace's MCP servers")
	}
}

// A secret sealed under a key the server no longer holds: the server is
// left out, with a reason that holds no value.
func TestMCPSecretUnderALostKey(t *testing.T) {
	s, sqlDB, _ := fixture(t)
	ctx := root()
	s.SetSecrets(secretbox.NewStore(sqlDB, secretbox.New("first key")))
	if _, _, err := s.SetMCPServer(ctx, MCPServerDef{Name: "x", Type: MCPHTTP, URL: "https://a/mcp", Headers: map[string]string{"Authorization": "${secret:token}"}, Workspaces: []string{"fabrikam/*"}}, "cli"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMCPSecret(ctx, "x", "token", []byte("a-long-enough-value"), "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, KindWorkspace, "fabrikam/api", mcpConfig("x"), "cli", ""); err != nil {
		t.Fatal(err)
	}
	s.SetSecrets(secretbox.NewStore(sqlDB, secretbox.New("another key")))
	mcp, err := s.RunMCP(ctx, "fabrikam/api", true)
	if err != nil || len(mcp.Servers) != 0 || len(mcp.Notes) != 1 || !strings.Contains(mcp.Notes[0], "cannot be opened") || strings.Contains(mcp.Notes[0], "a-long-enough-value") {
		t.Fatalf("a lost key: %+v %v", mcp, err)
	}
}

func TestMCPServerWorkspaces(t *testing.T) {
	with := func(patterns ...string) MCPServerDef {
		return MCPServerDef{Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Headers: map[string]string{"Authorization": "${secret:t}"}, Workspaces: patterns}
	}
	cases := []struct {
		def  MCPServerDef
		ws   string
		want bool
	}{
		{with("*"), "github.com/globex/shop", true},
		{with("github.com/globex/*"), "github.com/globex/shop", true},
		{with("github.com/globex/*"), "github.com/alex/personal", false},
		{with("github.com/globex/*"), "github.com/globex/a/b", false},
		{with("github.com/globex/shop", "github.com/alex/x"), "github.com/alex/x", true},
		// A server without secrets and without a list: any workspace.
		{MCPServerDef{Name: "x", Type: MCPHTTP, URL: "https://x/mcp"}, "anything/at/all", true},
		{MCPServerDef{Name: "x", Type: MCPHTTP, URL: "https://x/mcp", Workspaces: []string{"a/*"}}, "b/c", false},
	}
	for _, c := range cases {
		if got := c.def.AllowsWorkspace(c.ws); got != c.want {
			t.Errorf("%v for %s: %v, want %v", c.def.Workspaces, c.ws, got, c.want)
		}
	}
}

// A secret is for the server it was set for. A command server is told
// where to connect by its plain variables, so another value there is
// another server for the token beside it; and a value left attached to a
// definition it does not belong to (a save that failed halfway, in a
// release that saved the definition first) goes at the next save of the
// same definition, with an audit entry.
func TestMCPSecretsFollowTheTarget(t *testing.T) {
	s, sink, store := mcpFixture(t, "test key")
	ctx := root()
	def := MCPServerDef{Name: "tracker", Type: MCPCommand, Command: []string{"npx", "-y", "tracker-mcp"},
		Env: map[string]string{"TRACKER_URL": "https://tracker.example.com", "TRACKER_TOKEN": "${secret:token}"}, Workspaces: []string{"*"}}
	if _, _, err := s.SetMCPServer(ctx, def, "cli"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMCPSecret(ctx, "tracker", "token", []byte("tracker-secret-1"), "cli"); err != nil {
		t.Fatal(err)
	}
	// The same definition again keeps the value.
	if v, changed, err := s.SetMCPServer(ctx, def, "cli"); err != nil || changed || !v.Secrets[0].Set {
		t.Fatalf("an unchanged save keeps the secret: %+v %v %v", v, changed, err)
	}
	// Another address in a plain variable: the value has to be set again.
	def.Env["TRACKER_URL"] = "https://elsewhere.example.com"
	v, _, err := s.SetMCPServer(ctx, def, "cli")
	if err != nil || v.Secrets[0].Set {
		t.Fatalf("the token must not follow the command to another address: %+v %v", v, err)
	}
	last := sink.entries[len(sink.entries)-1]
	if last.Action != "mcp_server.update" || !strings.Contains(last.Notes, "stored secrets were deleted") {
		t.Fatalf("the audit entry says the secrets went: %+v", last)
	}

	// A value left behind under the owner is dropped by a save that changes
	// nothing else, and that is recorded.
	if err := store.Set(ctx, mcpOwner("tracker"), "stale", []byte("left-behind-value"), "test"); err != nil {
		t.Fatal(err)
	}
	before := len(sink.entries)
	if _, changed, err := s.SetMCPServer(ctx, def, "cli"); err != nil || changed {
		t.Fatalf("the definition itself is unchanged: %v %v", changed, err)
	}
	have, _ := store.List(ctx, mcpOwner("tracker"))
	if _, ok := have["stale"]; ok {
		t.Fatal("a secret the definition does not name was kept")
	}
	if len(sink.entries) != before+1 || !strings.Contains(sink.entries[len(sink.entries)-1].Notes, "stale") {
		t.Fatalf("dropping it is recorded: %+v", sink.entries[before:])
	}

	// A password in a URL with no user name is a credential in plain text.
	if _, err := NormalizeMCPServer(MCPServerDef{Name: "cache", Type: MCPCommand, Command: []string{"cache-mcp"},
		Env: map[string]string{"CACHE_ADDR": "redis://:s3cretPassw0rd@cache.example.com:6379/0"}}); err == nil {
		t.Fatal("a password in a URL without a user was accepted in plain text")
	}
}
