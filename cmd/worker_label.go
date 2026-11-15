package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/martinsuchenak/skopos/internal/labels"
	"github.com/paularlott/cli"
)

// skopos worker label: say what a worker has that the others serving the
// same workspace may not (a client's data, a licensed tool). An item that
// needs a label (skopos inbox add --needs) is worked only by a worker with
// it. The labels are held on the server; the worker's own key cannot set
// them, so this runs with a root or approver key.

func workerLabelCmd() *cli.Command {
	return &cli.Command{
		Name:  "label",
		Usage: "Show or set a worker's labels: an item that needs labels (skopos inbox add --needs) is worked only by a worker with all of them. Root, or an approver over every workspace the worker serves",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "id", Usage: "The worker's id, as the Workers page shows it"},
			&cli.StringFlag{Name: "set", Usage: "The worker's labels, comma-separated; replaces the list (e.g. stock,plant-2)"},
			&cli.BoolFlag{Name: "clear", Usage: "Remove every label"},
			// No EnvVars, ConfigPath or default on these two: labelTarget
			// resolves them itself.
			&cli.StringFlag{Name: "server-url", Usage: "Skopos server URL (default: SKOPOS_SERVER_URL, then [client] in the config)"},
			&cli.StringFlag{Name: "api-key", Usage: "A root or approver key (default: SKOPOS_API_KEY, then [client] in the config); the worker's own key is refused"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required: the worker's id (the Workers page lists them)")
			}
			server, key, err := labelTarget(cmd)
			if err != nil {
				return err
			}
			set, clear := cmd.GetString("set"), cmd.GetBool("clear")
			if set != "" && clear {
				return fmt.Errorf("give --set or --clear, not both")
			}
			if set == "" && !clear {
				var workers []struct {
					ID     string   `json:"id"`
					Labels []string `json:"labels"`
				}
				if err := apiDo(ctx, server, key, "", "list workers", http.MethodGet, "/api/workers", nil, &workers); err != nil {
					return err
				}
				for _, w := range workers {
					if w.ID == id {
						fmt.Println(labelLine(id, w.Labels))
						return nil
					}
				}
				return fmt.Errorf("no worker %s (a worker is listed once it has polled the server)", id)
			}
			list, err := labels.Normalize(labels.Split(set))
			if err != nil {
				return err
			}
			if list == nil {
				list = []string{}
			}
			var out struct {
				Labels []string `json:"labels"`
			}
			if err := apiDo(ctx, server, key, "cli", "label worker", http.MethodPut, "/api/workers/"+url.PathEscape(id)+"/labels",
				map[string]any{"labels": list}, &out); err != nil {
				return err
			}
			fmt.Println(labelLine(id, out.Labels))
			return nil
		},
	}
}

func labelLine(id string, list []string) string {
	if len(list) == 0 {
		return id + " has no labels: a run that needs a labelled worker is not handed to it."
	}
	if len(list) == 1 {
		return id + " has the label " + list[0] + "."
	}
	return id + " has the labels " + strings.Join(list, ", ") + "."
}

// labelTarget resolves the server and key: flag, environment, then the
// [client] section of the config (here, else in ~/.config/skopos). The
// worker's env file is not read: its key may not label a worker.
func labelTarget(cmd *cli.Command) (server, key string, err error) {
	cfgPath := configFileFor(cmd)
	if _, err := os.Stat(cfgPath); err != nil && !filepath.IsAbs(cfgPath) {
		if global := filepath.Join(GlobalConfigPath(), cfgPath); fileExists(global) {
			cfgPath = global
		}
	}
	clientCfg, err := readClientSection(cfgPath)
	if err != nil {
		return "", "", err
	}
	key = resolveClientSetting(cmd.GetString("api-key"), "SKOPOS_API_KEY", "", clientCfg.APIKey)
	server = resolveClientSetting(cmd.GetString("server-url"), "SKOPOS_SERVER_URL", "http://localhost:8080", clientCfg.ServerURL)
	return strings.TrimRight(server, "/"), key, nil
}
