package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/secretbox"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
	"github.com/paularlott/cli"
)

func init() {
	Register(mcpServerCmd())
	Register(secretKeyCmd())
}

// keyValues reads repeated K=V flags into a map.
func keyValues(flag string, in []string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, kv := range in {
		k, v, ok := strings.Cut(kv, "=")
		if k = strings.TrimSpace(k); !ok || k == "" {
			return nil, fmt.Errorf("--%s %q: use NAME=value", flag, kv)
		}
		out[k] = v
	}
	return out, nil
}

func printMCPServer(m wsconfig.MCPServerView) {
	if m.Type == wsconfig.MCPCommand {
		fmt.Printf("%s  command %s\n", m.Name, jsonLine(m.Command))
	} else {
		fmt.Printf("%s  http %s\n", m.Name, m.URL)
	}
	for _, set := range []struct {
		label string
		m     map[string]string
	}{{"header", m.Headers}, {"env", m.Env}} {
		names := make([]string, 0, len(set.m))
		for k := range set.m {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Printf("  %s %s = %s\n", set.label, k, set.m[k])
		}
	}
	if len(m.Workspaces) > 0 {
		fmt.Printf("  for workspaces %s\n", strings.Join(m.Workspaces, ", "))
	}
	for _, st := range m.Secrets {
		state := "NOT SET — runs go without this server until it is (skopos mcp-server set-secret " + m.Name + " " + st.Name + ")"
		if st.Set {
			state = "set"
			if st.UpdatedAt != nil {
				state += " " + st.UpdatedAt.Format("2006-01-02")
			}
			if st.UpdatedBy != "" {
				state += " by " + st.UpdatedBy
			}
		}
		fmt.Printf("  secret %s: %s\n", st.Name, state)
	}
	if len(m.UsedBy) > 0 {
		fmt.Printf("  given to the runs of %s\n", strings.Join(m.UsedBy, ", "))
	}
}

// mcpServerCmd is `skopos mcp-server …`: MCP servers for pipeline runs,
// defined on the server (phase 2b).
func mcpServerCmd() *cli.Command {
	return &cli.Command{
		Name:  "mcp-server",
		Usage: "MCP servers for pipeline runs, defined on the server: where each is, and the secrets it needs",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List the MCP server definitions and which of their secrets are set (admin permission)",
				Flags: keyClientFlags(),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					var out struct {
						Servers []wsconfig.MCPServerView `json:"mcp_servers"`
					}
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, "/api/fleet/mcp-servers", nil, &out); err != nil {
						return err
					}
					if len(out.Servers) == 0 {
						fmt.Println("no MCP server definitions: each worker's own mcp_config applies")
						return nil
					}
					for _, m := range out.Servers {
						printMCPServer(m)
					}
					return nil
				},
			},
			{
				Name:  "set",
				Usage: "Define or redefine an MCP server (admin over every workspace)",
				Description: `An http server:
  skopos mcp-server set llmrouter --url https://llmrouter.example.com/mcp \
      --header 'Authorization=Bearer ${secret:token}' --for 'github.com/fortix/*'
  skopos mcp-server set-secret llmrouter token        # asks for the value

A command the worker runs (only on workers with server_commands = true):
  skopos mcp-server set scriptling --command '["scriptling","mcp"]' --env 'TOOLS=${TOOLS_DIR}/scriptling'

A credential is never part of the definition: name it as ${secret:NAME} and
set its value with set-secret. ${NAME} in a command server's command and env
is filled in on each worker from its [worker.vars].

--for says which workspaces' runs may be given the server, as patterns on the
workspace id ('*' for every workspace). A server that names secrets has to
say: its credentials go to every worker that runs one of those workspaces.
Changing an http server's host, or a command server's command, deletes its
stored secrets: set them again.

A workspace's runs get a server when its config names it and allows its tools:
  skopos workspace config set --set 'mcp=["llmrouter"]' --set 'mcp_servers=["mcp__llmrouter"]'
Once a workspace's config sets mcp, its runs get exactly those servers and
nothing from a worker's own mcp_config.`,
				Flags: append(keyClientFlags(),
					&cli.StringFlag{Name: "url", Usage: "An http server's URL"},
					&cli.StringSliceFlag{Name: "header", Usage: "An http server's header, NAME=value; repeat for each. A credential is ${secret:NAME}"},
					&cli.StringFlag{Name: "command", Usage: `A command server's argv as a JSON array: '["npx","-y","some-mcp-server"]'`},
					&cli.StringSliceFlag{Name: "env", Usage: "A command server's variable, NAME=value; repeat for each. A credential is ${secret:NAME}"},
					&cli.StringSliceFlag{Name: "for", Usage: "A pattern on the ids of the workspaces whose runs may be given the server ('github.com/you/*', '*' for all); repeat for each. Required for a server that names secrets"},
					&cli.StringFlag{Name: "file", Usage: "A TOML file with type, url, headers, command, env and workspaces, instead of the flags"},
				),
				Arguments: []cli.Argument{&cli.StringArg{Name: "name", Usage: "Server name (runs see its tools as mcp__<name>__…)", Required: true}},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					name := cmd.GetStringArg("name")
					var def wsconfig.MCPServerDef
					if file := cmd.GetString("file"); file != "" {
						md, err := toml.DecodeFile(file, &def)
						if err != nil {
							return fmt.Errorf("reading %s: %w", file, err)
						}
						if extra := md.Undecoded(); len(extra) > 0 {
							return fmt.Errorf("%s: %s is not a field of an MCP server (type, url, headers, command, env, workspaces)", file, extra[0])
						}
					} else {
						var err error
						def.URL, def.Workspaces = cmd.GetString("url"), cmd.GetStringSlice("for")
						if def.Headers, err = keyValues("header", cmd.GetStringSlice("header")); err != nil {
							return err
						}
						if def.Env, err = keyValues("env", cmd.GetStringSlice("env")); err != nil {
							return err
						}
						if v := strings.TrimSpace(cmd.GetString("command")); v != "" {
							if err := json.Unmarshal([]byte(v), &def.Command); err != nil {
								return fmt.Errorf("--command: a JSON array of strings (%v)", err)
							}
						}
						switch {
						case def.URL != "" && len(def.Command) > 0:
							return fmt.Errorf("give --url (an http server) or --command (a command the worker runs), not both")
						case def.URL != "":
							def.Type = wsconfig.MCPHTTP
						case len(def.Command) > 0:
							def.Type = wsconfig.MCPCommand
						default:
							return fmt.Errorf("give --url (an http server) or --command (a command the worker runs)")
						}
					}
					var v wsconfig.MCPServerView
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodPut, "/api/fleet/mcp-servers/"+url.PathEscape(name), def, &v); err != nil {
						return err
					}
					fmt.Printf("saved (revision %s)\n", v.Rev)
					printMCPServer(v)
					return nil
				},
			},
			{
				Name:  "delete",
				Usage: "Remove an MCP server definition and its stored secrets (admin over every workspace)",
				Flags: append(keyClientFlags(),
					&cli.BoolFlag{Name: "force", Usage: "Delete it although a workspace's or group's config still gives its runs the server"}),
				Arguments: []cli.Argument{&cli.StringArg{Name: "name", Usage: "Server name", Required: true}},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					name := cmd.GetStringArg("name")
					path := "/api/fleet/mcp-servers/" + url.PathEscape(name)
					if cmd.GetBool("force") {
						path += "?force=1"
					}
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodDelete, path, nil, nil); err != nil {
						return err
					}
					fmt.Println("deleted " + name + " and its stored secrets")
					return nil
				},
			},
			{
				Name:  "set-secret",
				Usage: "Store the value of a secret an MCP server's definition names (admin over every workspace). The value is never shown again",
				Description: `The value is read from the terminal (not echoed), from standard input when
it is piped, or from an environment variable:
  skopos mcp-server set-secret llmrouter token
  pass show fortix/llmrouter | skopos mcp-server set-secret llmrouter token
  skopos mcp-server set-secret llmrouter token --from-env LLMROUTER_TOKEN

It is never taken from the command line, where it would stay in the shell's
history. The server needs its secret key (integrations.secret_key).`,
				Flags: append(keyClientFlags(),
					&cli.StringFlag{Name: "from-env", Usage: "Read the value from this environment variable"}),
				Arguments: []cli.Argument{
					&cli.StringArg{Name: "server", Usage: "Server name", Required: true},
					&cli.StringArg{Name: "secret", Usage: "The secret's name, as the definition has it in ${secret:NAME}", Required: true},
				},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					server, secret := cmd.GetStringArg("server"), cmd.GetStringArg("secret")
					value, err := readSecretValue(cmd.GetString("from-env"), server+" "+secret)
					if err != nil {
						return err
					}
					path := "/api/fleet/mcp-servers/" + url.PathEscape(server) + "/secrets/" + url.PathEscape(secret)
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodPut, path, map[string]string{"value": value}, nil); err != nil {
						return err
					}
					fmt.Printf("stored secret %s of %s\n", secret, server)
					return nil
				},
			},
			{
				Name:  "unset-secret",
				Usage: "Delete a stored secret (admin over every workspace): runs go without the server until it is set again",
				Flags: keyClientFlags(),
				Arguments: []cli.Argument{
					&cli.StringArg{Name: "server", Usage: "Server name", Required: true},
					&cli.StringArg{Name: "secret", Usage: "The secret's name", Required: true},
				},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					server, secret := cmd.GetStringArg("server"), cmd.GetStringArg("secret")
					path := "/api/fleet/mcp-servers/" + url.PathEscape(server) + "/secrets/" + url.PathEscape(secret)
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodDelete, path, nil, nil); err != nil {
						return err
					}
					fmt.Printf("deleted secret %s of %s\n", secret, server)
					return nil
				},
			},
		},
	}
}

// readSecretValue reads a secret from an environment variable, from the
// terminal without echo, or from piped standard input — never from the
// command line.
func readSecretValue(fromEnv, label string) (string, error) {
	if fromEnv != "" {
		v := os.Getenv(fromEnv)
		if v == "" {
			return "", fmt.Errorf("the environment variable %s is empty or not set", fromEnv)
		}
		return v, nil
	}
	var v string
	if stdinIsTerminal() {
		v = promptSecret(bufio.NewReader(os.Stdin), os.Stderr, "Value of "+label)
	} else {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, secretbox.MaxValueBytes+1))
		if err != nil {
			return "", err
		}
		v = strings.TrimRight(string(b), "\r\n")
	}
	if v == "" {
		return "", fmt.Errorf("no value was given")
	}
	return v, nil
}

// secretKeyCmd is `skopos secret-key …`: the server's secret key and its
// rotation.
func secretKeyCmd() *cli.Command {
	printStatus := func(st *secretbox.Status) {
		if st.Current == "" {
			fmt.Println("the server has no secret key (integrations.secret_key): it stores no secrets, and webhooks are off")
			return
		}
		fmt.Printf("current key: %s\n", st.Current)
		if len(st.Previous) > 0 {
			fmt.Printf("previous keys still held: %s\n", strings.Join(st.Previous, ", "))
		}
		ids := make([]string, 0, len(st.Sealed))
		for id := range st.Sealed {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) == 0 {
			fmt.Println("no stored secrets")
		}
		for _, id := range ids {
			note := ""
			switch id {
			case st.Current:
				note = " (the current key)"
			case secretbox.LegacyKey:
				note = " (from before keys were named)"
			}
			fmt.Printf("  %d stored value(s) under %s%s\n", st.Sealed[id], id, note)
		}
		switch {
		case st.Unreadable > 0:
			fmt.Printf("%d stored value(s) are under a key this server does not hold: set that key as integrations.previous_secret_key, restart, and rotate\n", st.Unreadable)
		case st.Pending > 0:
			fmt.Printf("%d stored value(s) are not under the current key: run `skopos secret-key rotate`\n", st.Pending)
		case len(st.Previous) > 0:
			fmt.Println("everything is under the current key: integrations.previous_secret_key can be removed")
		}
	}
	return &cli.Command{
		Name:  "secret-key",
		Usage: "The server's secret key (integrations.secret_key): what it sealed, and rotating it",
		Commands: []*cli.Command{
			{
				Name:  "status",
				Usage: "Show which key sealed the stored secrets (root key)",
				Flags: keyClientFlags(),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					var st secretbox.Status
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, "/api/fleet/secret-key", nil, &st); err != nil {
						return err
					}
					printStatus(&st)
					return nil
				},
			},
			{
				Name:  "rotate",
				Usage: "Seal every stored secret again under the server's current key (root key)",
				Description: `To change the server's secret key:

  1. Set the new key as integrations.secret_key (SKOPOS_INTEGRATIONS_KEY) and
     the old one as integrations.previous_secret_key
     (SKOPOS_INTEGRATIONS_PREVIOUS_KEY), and restart the server. It opens
     what either key sealed and seals with the new one.
  2. skopos secret-key rotate
     Every stored secret — MCP server credentials, webhook signing secrets —
     is sealed again under the new key, all of them or none.
  3. Remove integrations.previous_secret_key and restart.

The keys themselves never travel: the command only tells the server to do it.
Keep a copy of the key apart from the database and its replica: without the
key the stored secrets cannot be opened.`,
				Flags: keyClientFlags(),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					var res secretbox.Result
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodPost, "/api/fleet/secret-key/rotate", nil, &res); err != nil {
						return err
					}
					if res.Resealed == 0 {
						fmt.Printf("nothing to do: every stored secret is already under the current key (%s)\n", res.Current)
					} else {
						fmt.Printf("sealed %d stored value(s) again under the current key (%s)\n", res.Resealed, res.Current)
					}
					var st secretbox.Status
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, "/api/fleet/secret-key", nil, &st); err == nil && len(st.Previous) > 0 && st.Pending == 0 && st.Unreadable == 0 {
						fmt.Println("integrations.previous_secret_key can be removed now")
					}
					return nil
				},
			},
		},
	}
}
