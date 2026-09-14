package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/paularlott/cli"
)

func init() {
	Register(keyCmd())
	Register(whoamiCmd())
}

func keyCmd() *cli.Command {
	return &cli.Command{
		Name:  "key",
		Usage: "Manage scoped API keys (requires the root key)",
		Commands: []*cli.Command{
			keyCreateCmd(),
			keyListCmd(),
			keyEditCmd(),
			keyRevokeCmd(),
			keyDeleteCmd(),
			keyGenerateRootCmd(),
		},
	}
}

func keyClientFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
		&cli.StringFlag{Name: "api-key", Usage: "Skopos API key (root key for management)", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
	}
}

func keyCreateCmd() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "Create a scoped API key; the secret is printed once",
		Flags: append(keyClientFlags(),
			&cli.StringFlag{Name: "name", Usage: "Key name (e.g. ci, zcode-laptop)"},
			&cli.StringSliceFlag{Name: "workspace", Usage: "Workspace ID the key can access (repeat or comma-separate)"},
			&cli.BoolFlag{Name: "all-workspaces", Usage: "Grant access to every workspace"},
		),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			name := strings.TrimSpace(cmd.GetString("name"))
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			workspaces := cmd.GetStringSlice("workspace")
			all := cmd.GetBool("all-workspaces")
			if !all && len(workspaces) == 0 {
				return fmt.Errorf("pass --workspace <id> (repeatable) or --all-workspaces")
			}
			if all {
				workspaces = []string{"*"}
			}
			var result apikeys.CreateResult
			if err := keysCall(ctx, cmd, http.MethodPost, "/api/keys",
				map[string]any{"name": name, "workspaces": workspaces}, &result); err != nil {
				return err
			}
			fmt.Println("API key created — store it now, it is shown only once:")
			fmt.Printf("  %s\n", result.Secret)
			fmt.Printf("  id: %s  name: %s  scope: %s\n", result.Key.ID, result.Key.Name, scopeLabel(result.Key))
			return nil
		},
	}
}

func keyListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List API keys (including revoked)",
		Flags: keyClientFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			var keys []apikeys.Key
			if err := keysCall(ctx, cmd, http.MethodGet, "/api/keys", nil, &keys); err != nil {
				return err
			}
			if len(keys) == 0 {
				fmt.Println("no API keys")
				return nil
			}
			for _, k := range keys {
				status := ""
				if k.RevokedAt != nil {
					status = "  [revoked]"
				}
				fmt.Printf("%s  %-20s %-17s %s%s\n", k.ID, k.Name, k.Prefix, scopeLabel(k), status)
			}
			return nil
		},
	}
}

func keyRevokeCmd() *cli.Command {
	return &cli.Command{
		Name:    "revoke",
		Usage:   "Revoke an API key by id",
		MaxArgs: 1,
		Flags:   keyClientFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.GetArgs()
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("usage: skopos key revoke <id>")
			}
			if err := keysCall(ctx, cmd, http.MethodDelete, "/api/keys/"+strings.TrimSpace(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Println("revoked")
			return nil
		},
	}
}

func whoamiCmd() *cli.Command {
	return &cli.Command{
		Name:  "whoami",
		Usage: "Show what the configured credential can access",
		Flags: keyClientFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			var who struct {
				Root       bool `json:"root"`
				Key        *struct {
					ID            string   `json:"id"`
					Name          string   `json:"name"`
					AllWorkspaces bool     `json:"all_workspaces"`
					Workspaces    []string `json:"workspaces"`
				} `json:"key"`
				Workspaces []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"workspaces"`
			}
			if err := keysCall(ctx, cmd, http.MethodGet, "/api/whoami", nil, &who); err != nil {
				return err
			}
			if who.Root {
				fmt.Println("root key — full access")
			} else if who.Key != nil {
				fmt.Printf("key %s (%s) — scope: %s\n", who.Key.ID, who.Key.Name, scopeLabelKey(who.Key.AllWorkspaces, who.Key.Workspaces))
			}
			for _, ws := range who.Workspaces {
				name := ws.Name
				if name == ws.ID {
					fmt.Printf("  workspace: %s\n", ws.ID)
				} else {
					fmt.Printf("  workspace: %s (%s)\n", ws.ID, name)
				}
			}
			return nil
		},
	}
}

func keyEditCmd() *cli.Command {
	return &cli.Command{
		Name:    "edit",
		Usage:   "Edit an existing key's name and/or workspace scope (only provided fields change)",
		MaxArgs: 1,
		Flags: append(keyClientFlags(),
			&cli.StringFlag{Name: "name", Usage: "New key name"},
			&cli.StringSliceFlag{Name: "workspace", Usage: "Workspace IDs the key can access (repeat or comma-separate; replaces the current list)"},
			&cli.BoolFlag{Name: "all-workspaces", Usage: "Grant access to every workspace (replaces the list)"},
		),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.GetArgs()
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("usage: skopos key edit <id> [--name n] [--workspace id] | --all-workspaces")
			}
			body := map[string]any{}
			if name := strings.TrimSpace(cmd.GetString("name")); name != "" {
				body["name"] = name
			}
			if cmd.GetBool("all-workspaces") {
				body["workspaces"] = []string{"*"}
			} else if workspaces := cmd.GetStringSlice("workspace"); len(workspaces) > 0 {
				body["workspaces"] = workspaces
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to edit: pass --name, --workspace, or --all-workspaces")
			}
			var key apikeys.Key
			if err := keysCall(ctx, cmd, http.MethodPatch, "/api/keys/"+strings.TrimSpace(args[0]), body, &key); err != nil {
				return err
			}
			fmt.Printf("updated %s  name: %s  scope: %s\n", key.ID, key.Name, scopeLabel(key))
			return nil
		},
	}
}

func keyDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:    "delete",
		Usage:   "Hard-delete a key (row and scope; usually an old revoked key — use revoke for active keys)",
		MaxArgs: 1,
		Flags: append(keyClientFlags(),
			&cli.BoolFlag{Name: "force", Usage: "Hard-delete even an active key without revoking first"},
		),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.GetArgs()
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("usage: skopos key delete <id> [--force]")
			}
			path := "/api/keys/" + strings.TrimSpace(args[0]) + "?hard=true"
			if !cmd.GetBool("force") {
				// Without --force, refuse active keys: revoking first
				// terminates their SSE streams and keeps the audit trail
				// until the operator deliberately removes it.
				var keys []apikeys.Key
				if err := keysCall(ctx, cmd, http.MethodGet, "/api/keys", nil, &keys); err == nil {
					for _, k := range keys {
						if k.ID == strings.TrimSpace(args[0]) && k.RevokedAt == nil {
							return fmt.Errorf("key %s is still active — revoke it first (skopos key revoke) or pass --force", k.ID)
						}
					}
				}
			}
			if err := keysCall(ctx, cmd, http.MethodDelete, path, nil, nil); err != nil {
				return err
			}
			fmt.Println("deleted")
			return nil
		},
	}
}

func keyGenerateRootCmd() *cli.Command {
	return &cli.Command{
		Name:  "generate-root",
		Usage: "Generate a strong root key for the server configuration (offline; nothing is sent anywhere)",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "quiet", Usage: "Print only the key itself (for scripting)"},
		},
		Run: func(_ context.Context, cmd *cli.Command) error {
			secret, err := apikeys.GenerateSecret()
			if err != nil {
				return err
			}
			if cmd.GetBool("quiet") {
				fmt.Println(secret)
				return nil
			}
			fmt.Println("Generated root key (256 bits) — set it as the server's root credential:")
			fmt.Println()
			fmt.Printf("  %s\n", secret)
			fmt.Println()
			fmt.Println("Where to configure it on the server:")
			fmt.Println("  skopos-config.toml [auth] api_key = \"...\"   (or the SKOPOS_API_KEY env var)")
			fmt.Println()
			fmt.Println("Rotation notes:")
			fmt.Println("  - the previous root key stops working as soon as the server restarts with the new one")
			fmt.Println("  - update every client (skopos install --api-key, [client] api_key) afterwards")
			fmt.Println("  - prefer minting scoped keys per agent (skopos key create) instead of sharing the root")
			fmt.Println("  - the old root key cannot be recovered or re-derived — store this one now")
			return nil
		},
	}
}

func scopeLabel(k apikeys.Key) string {
	return scopeLabelKey(k.AllWorkspaces, k.Workspaces)
}

func scopeLabelKey(all bool, workspaces []string) string {
	if all {
		return "* (all workspaces)"
	}
	return strings.Join(workspaces, ", ")
}

// keysCall performs an authenticated API-keys HTTP call for a parsed command.
func keysCall(ctx context.Context, cmd *cli.Command, method, path string, body any, out any) error {
	return keysDo(ctx, strings.TrimRight(cmd.GetString("server-url"), "/"), cmd.GetString("api-key"), method, path, body, out)
}

// keysDo performs the HTTP call, decoding into out when non-nil.
func keysDo(ctx context.Context, serverURL, apiKey, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, serverURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s", apiErrorMessage("api keys", resp))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}
