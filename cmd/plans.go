package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/workspace"
	"github.com/paularlott/cli"
)

func init() {
	Register(planCmd())
}

func planCmd() *cli.Command {
	return &cli.Command{
		Name:  "plan",
		Usage: "Manage agent plans and todo lists",
		Commands: []*cli.Command{
			planCreateCmd(),
			planListCmd(),
			planShowCmd(),
			planDoneCmd(),
			planItemCmd(),
		},
	}
}

func planCreateCmd() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "Create a new plan",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "name", Usage: "Plan name"},
			&cli.StringFlag{Name: "branch", Usage: "Branch name (omit for project-wide)"},
			&cli.StringFlag{Name: "description", Usage: "Optional description"},
			&cli.StringFlag{Name: "agent-id", Usage: "Agent identifier", EnvVars: []string{"SKOPOS_AGENT_ID"}},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			ws := cmd.GetString("workspace")
			if ws == "" {
				if id, err := workspace.Resolve("."); err == nil {
					ws = id
				}
			}
			plan, err := plansPost(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), plans.CreatePlanInput{
				Name:          cmd.GetString("name"),
				BranchName:    cmd.GetString("branch"),
				Description:   cmd.GetString("description"),
				AuthorAgentID: cmd.GetString("agent-id"),
				WorkspaceID:   ws,
			})
			if err != nil {
				return err
			}
			fmt.Printf("created id=%s name=%q\n", plan.ID, plan.Name)
			return nil
		},
	}
}

func planListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List plans",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "branch", Usage: "Filter by branch name"},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			ws := cmd.GetString("workspace")
			if ws == "" {
				if id, err := workspace.Resolve("."); err == nil {
					ws = id
				}
			}
			ps, err := plansGetList(ctx, cmd.GetString("server-url"), cmd.GetString("branch"), ws)
			if err != nil {
				return err
			}
			if len(ps) == 0 {
				fmt.Println("no plans")
				return nil
			}
			fmt.Printf("%-36s  %-8s  %-12s  %s\n", "ID", "STATUS", "BRANCH", "NAME")
			for _, p := range ps {
				fmt.Printf("%-36s  %-8s  %-12s  %s\n", p.ID, p.Status, p.BranchName, p.Name)
			}
			return nil
		},
	}
}

func planShowCmd() *cli.Command {
	return &cli.Command{
		Name:  "show",
		Usage: "Show a plan with its items",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "id", Usage: "Plan ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			plan, err := plansGetOne(ctx, cmd.GetString("server-url"), id)
			if err != nil {
				return err
			}
			fmt.Printf("Plan: %s (%s)\n", plan.Name, plan.Status)
			if plan.BranchName != "" {
				fmt.Printf("Branch: %s\n", plan.BranchName)
			}
			if len(plan.Items) == 0 {
				fmt.Println("No items.")
				return nil
			}
			for _, item := range plan.Items {
				claimed := ""
				if item.ClaimedByAgentID != "" {
					claimed = " [" + item.ClaimedByAgentID + "]"
				}
				fmt.Printf("  [%s] %s%s\n", item.Status, item.Title, claimed)
			}
			return nil
		},
	}
}

func planDoneCmd() *cli.Command {
	return &cli.Command{
		Name:  "done",
		Usage: "Mark a plan as completed",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "id", Usage: "Plan ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			return plansPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id,
				plans.UpdatePlanInput{Status: plans.PlanCompleted})
		},
	}
}

func planItemCmd() *cli.Command {
	return &cli.Command{
		Name:  "item",
		Usage: "Manage plan items",
		Commands: []*cli.Command{
			planItemAddCmd(),
			planItemDoneCmd(),
			planItemClaimCmd(),
			planItemUnclaimCmd(),
			planItemBlockCmd(),
		},
	}
}

func planItemAddCmd() *cli.Command {
	return &cli.Command{
		Name:  "add",
		Usage: "Add an item to a plan",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "plan-id", Usage: "Plan ID"},
			&cli.StringFlag{Name: "title", Usage: "Item title"},
			&cli.StringFlag{Name: "description", Usage: "Optional description"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			planID := strings.TrimSpace(cmd.GetString("plan-id"))
			if planID == "" {
				return fmt.Errorf("--plan-id is required")
			}
			item, err := plansItemPost(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				planID, plans.CreateItemInput{
					Title:       cmd.GetString("title"),
					Description: cmd.GetString("description"),
				})
			if err != nil {
				return err
			}
			fmt.Printf("added item id=%s title=%q\n", item.ID, item.Title)
			return nil
		},
	}
}

func planItemDoneCmd() *cli.Command {
	return &cli.Command{
		Name:  "done",
		Usage: "Mark an item as done",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "plan-id", Usage: "Plan ID"},
			&cli.StringFlag{Name: "item-id", Usage: "Item ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			planID := strings.TrimSpace(cmd.GetString("plan-id"))
			itemID := strings.TrimSpace(cmd.GetString("item-id"))
			if planID == "" || itemID == "" {
				return fmt.Errorf("--plan-id and --item-id are required")
			}
			return plansItemPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				planID, itemID, plans.UpdateItemInput{Status: plans.ItemDone},
				"done")
		},
	}
}

func planItemClaimCmd() *cli.Command {
	return &cli.Command{
		Name:  "claim",
		Usage: "Claim an item as being worked on",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "plan-id", Usage: "Plan ID"},
			&cli.StringFlag{Name: "item-id", Usage: "Item ID"},
			&cli.StringFlag{Name: "agent-id", Usage: "Agent identifier", EnvVars: []string{"SKOPOS_AGENT_ID"}},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			planID := strings.TrimSpace(cmd.GetString("plan-id"))
			itemID := strings.TrimSpace(cmd.GetString("item-id"))
			if planID == "" || itemID == "" {
				return fmt.Errorf("--plan-id and --item-id are required")
			}
			agentID := cmd.GetString("agent-id")
			return plansItemPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				planID, itemID, plans.UpdateItemInput{ClaimedByAgentID: &agentID},
				"claim")
		},
	}
}

func planItemUnclaimCmd() *cli.Command {
	return &cli.Command{
		Name:  "unclaim",
		Usage: "Release claim on an item",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "plan-id", Usage: "Plan ID"},
			&cli.StringFlag{Name: "item-id", Usage: "Item ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			planID := strings.TrimSpace(cmd.GetString("plan-id"))
			itemID := strings.TrimSpace(cmd.GetString("item-id"))
			if planID == "" || itemID == "" {
				return fmt.Errorf("--plan-id and --item-id are required")
			}
			empty := ""
			return plansItemPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				planID, itemID, plans.UpdateItemInput{ClaimedByAgentID: &empty},
				"unclaim")
		},
	}
}

func planItemBlockCmd() *cli.Command {
	return &cli.Command{
		Name:  "block",
		Usage: "Mark an item as blocked",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "plan-id", Usage: "Plan ID"},
			&cli.StringFlag{Name: "item-id", Usage: "Item ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			planID := strings.TrimSpace(cmd.GetString("plan-id"))
			itemID := strings.TrimSpace(cmd.GetString("item-id"))
			if planID == "" || itemID == "" {
				return fmt.Errorf("--plan-id and --item-id are required")
			}
			return plansItemPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				planID, itemID, plans.UpdateItemInput{Status: plans.ItemBlocked},
				"block")
		},
	}
}

func plansPost(ctx context.Context, serverURL, apiKey string, input plans.CreatePlanInput) (*plans.Plan, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encoding input: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/api/plans",
		bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting plan: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("posting plan: unexpected status %s", resp.Status)
	}
	var plan plans.Plan
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &plan, nil
}

func plansGetList(ctx context.Context, serverURL, branch, workspaceID string) ([]plans.Plan, error) {
	base := strings.TrimRight(serverURL, "/") + "/api/plans"
	q := url.Values{}
	if branch != "" {
		q.Set("branch", branch)
	}
	if workspaceID != "" {
		q.Set("workspace", workspaceID)
	}
	u := base
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing plans: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing plans: unexpected status %s", resp.Status)
	}
	var result []plans.Plan
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding plans: %w", err)
	}
	return result, nil
}

func plansGetOne(ctx context.Context, serverURL, id string) (*plans.Plan, error) {
	u := strings.TrimRight(serverURL, "/") + "/api/plans/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting plan: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getting plan: unexpected status %s", resp.Status)
	}
	var plan plans.Plan
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, fmt.Errorf("decoding plan: %w", err)
	}
	return &plan, nil
}

func plansPatch(ctx context.Context, serverURL, apiKey, id string, input plans.UpdatePlanInput) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encoding input: %w", err)
	}
	u := strings.TrimRight(serverURL, "/") + "/api/plans/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("patching plan: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("patching plan: unexpected status %s", resp.Status)
	}
	return nil
}

func plansItemPost(ctx context.Context, serverURL, apiKey, planID string, input plans.CreateItemInput) (*plans.Item, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encoding input: %w", err)
	}
	u := strings.TrimRight(serverURL, "/") + "/api/plans/" + url.PathEscape(planID) + "/items"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("adding item: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("adding item: unexpected status %s", resp.Status)
	}
	var item plans.Item
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return nil, fmt.Errorf("decoding item: %w", err)
	}
	return &item, nil
}

func plansItemPatch(ctx context.Context, serverURL, apiKey, planID, itemID string, input plans.UpdateItemInput, action string) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encoding input: %w", err)
	}
	u := strings.TrimRight(serverURL, "/") + "/api/plans/" +
		url.PathEscape(planID) + "/items/" + url.PathEscape(itemID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s item: %w", action, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s item: unexpected status %s", action, resp.Status)
	}
	return nil
}
