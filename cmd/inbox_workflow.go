package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/paularlott/cli"
)

// The agent-pipeline human actions from the terminal — the same endpoints
// as the dashboard and the Slack relay. Approvals bind to what the operator
// saw: `approve` and `mark-done` print the plan revision / head they act on
// and confirm in a terminal; non-interactive use must name it explicitly
// (--revision / --head), so a script can never approve something unseen.

func workflowFlags(extra ...cli.Flag) []cli.Flag {
	return append([]cli.Flag{
		&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
		&cli.StringFlag{Name: "api-key", Usage: "Skopos API key (approver permission for the human actions)", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
		&cli.StringFlag{Name: "id", Usage: "Item ID"},
	}, extra...)
}

func inboxWorkflowCmds() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "plan",
			Usage: "Show the plan at the approval gate: revision, steps, skills, questions",
			Flags: workflowFlags(),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				c, id, err := workflowClient(cmd)
				if err != nil {
					return err
				}
				_, _, err = c.showPlan(ctx, os.Stdout, id)
				return err
			},
		},
		{
			Name:  "queue",
			Usage: "Queue an open item into the agent pipeline (a worker plans it)",
			Flags: workflowFlags(),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				return simpleAction(ctx, cmd, "queue", nil, "queued")
			},
		},
		{
			Name:  "approve",
			Usage: "Approve the plan at the gate — shows the revision and confirms (non-interactive: --revision)",
			Flags: workflowFlags(
				&cli.StringFlag{Name: "revision", Usage: "Revision ID you reviewed (required without a terminal; see 'inbox plan')"},
				&cli.StringFlag{Name: "notes", Usage: "Notes for the implementer (answers to the planner's questions, overrides)"},
			),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				c, id, err := workflowClient(cmd)
				if err != nil {
					return err
				}
				revID := strings.TrimSpace(cmd.GetString("revision"))
				if revID == "" {
					if !stdinIsTerminal() {
						return fmt.Errorf("--revision is required without a terminal: approve names the revision you reviewed (skopos inbox plan --id %s)", id)
					}
					rev, _, cands, err := c.showPlanFull(ctx, os.Stdout, id)
					if err != nil {
						return err
					}
					switch {
					case len(cands) > 1:
						n := pickNumber(fmt.Sprintf("Approve which candidate [1-%d]", len(cands)), len(cands))
						if n == 0 {
							fmt.Println("not approved")
							return nil
						}
						revID = cands[n-1].ID
					case rev != nil:
						if !confirm(fmt.Sprintf("Approve revision #%d", rev.RevisionNo)) {
							fmt.Println("not approved")
							return nil
						}
						revID = rev.ID
					default:
						return fmt.Errorf("the item has no plan revision to approve")
					}
				}
				body := map[string]string{"revision_id": revID}
				if n := cmd.GetString("notes"); n != "" {
					body["notes"] = n
				}
				return c.action(ctx, id, "approve", body, "approved — the implement run is queued")
			},
		},
		{
			Name:  "request-changes",
			Usage: "Send an item back: a re-plan from the gate, or a re-implementation from review",
			Flags: workflowFlags(
				&cli.StringFlag{Name: "notes", Usage: "What to change (required; the next run reads it)"},
				&cli.StringFlag{Name: "revision", Usage: "At a multi-agent plan gate: the candidate to revise (one planner reworks it); omit to re-plan all"},
			),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				n := strings.TrimSpace(cmd.GetString("notes"))
				if n == "" {
					return fmt.Errorf("--notes is required: it is what the next run reads")
				}
				body := map[string]string{"notes": n}
				if rv := strings.TrimSpace(cmd.GetString("revision")); rv != "" {
					body["revision_id"] = rv
				}
				return simpleAction(ctx, cmd, "request-changes", body, "changes requested")
			},
		},
		{
			Name:  "retry",
			Usage: "Answer a blocked (or failed) item and requeue it",
			Flags: workflowFlags(&cli.StringFlag{Name: "answer", Usage: "Answer to the blocker (required; appended for the next run)"}),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				a := strings.TrimSpace(cmd.GetString("answer"))
				if a == "" {
					return fmt.Errorf("--answer is required")
				}
				return simpleAction(ctx, cmd, "retry", map[string]string{"answer": a}, "requeued")
			},
		},
		{
			Name:  "mark-done",
			Usage: "Approve the reviewed head and finish the item — shows the head and confirms (non-interactive: --head)",
			Flags: workflowFlags(&cli.StringFlag{Name: "head", Usage: "Head commit you reviewed (required without a terminal)"}),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				c, id, err := workflowClient(cmd)
				if err != nil {
					return err
				}
				head := strings.TrimSpace(cmd.GetString("head"))
				if head == "" {
					if !stdinIsTerminal() {
						return fmt.Errorf("--head is required without a terminal: mark-done approves the exact commit you reviewed")
					}
					run, err := c.latestImplementRun(ctx, id)
					if err != nil {
						return err
					}
					fmt.Printf("Branch: %s\nHead:   %s\n", run.Branch, run.HeadSHA)
					if run.Diffstat != "" {
						fmt.Println(strings.TrimSpace(run.Diffstat))
					}
					if !confirm("Approve this head and mark the item done") {
						fmt.Println("not marked done")
						return nil
					}
					head = run.HeadSHA
				}
				return c.action(ctx, id, "mark-done", map[string]string{"head_sha": head}, "done")
			},
		},
		{
			Name:  "reject",
			Usage: "Reject a workflow item: it leaves the pipeline and is discarded",
			Flags: workflowFlags(
				&cli.StringFlag{Name: "notes", Usage: "Why (recorded in the audit trail)"},
				&cli.BoolFlag{Name: "yes", Usage: "Do not ask for confirmation"},
			),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				c, id, err := workflowClient(cmd)
				if err != nil {
					return err
				}
				if !cmd.GetBool("yes") && stdinIsTerminal() && !confirm("Reject item "+id) {
					fmt.Println("not rejected")
					return nil
				}
				var body map[string]string
				if n := cmd.GetString("notes"); n != "" {
					body = map[string]string{"notes": n}
				}
				return c.action(ctx, id, "reject", body, "rejected")
			},
		},
	}
}

func simpleAction(ctx context.Context, cmd *cli.Command, action string, body map[string]string, ok string) error {
	c, id, err := workflowClient(cmd)
	if err != nil {
		return err
	}
	return c.action(ctx, id, action, body, ok)
}

// pickNumber asks for 1..max; 0 = declined or invalid.
func pickNumber(label string, max int) int {
	a := strings.TrimSpace(promptTo(bufio.NewReader(os.Stdin), os.Stdout, label+" (blank = cancel)", ""))
	n, err := strconv.Atoi(a)
	if err != nil || n < 1 || n > max {
		return 0
	}
	return n
}

func confirm(label string) bool {
	a := strings.ToLower(promptTo(bufio.NewReader(os.Stdin), os.Stdout, label+"? [y/N]", ""))
	return strings.HasPrefix(a, "y")
}

// apiClient is a tiny JSON client for the CLI's REST calls.
type apiClient struct {
	base, key string
}

func workflowClient(cmd *cli.Command) (*apiClient, string, error) {
	id := strings.TrimSpace(cmd.GetString("id"))
	if id == "" {
		return nil, "", fmt.Errorf("--id is required")
	}
	return &apiClient{base: strings.TrimRight(cmd.GetString("server-url"), "/"), key: cmd.GetString("api-key")}, id, nil
}

func (c *apiClient) call(ctx context.Context, method, path string, body, out any, verb string) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Skopos-Via", "cli")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s", apiErrorMessage(verb, resp))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *apiClient) action(ctx context.Context, id, action string, body map[string]string, ok string) error {
	if body == nil {
		body = map[string]string{}
	}
	var item struct {
		Status string `json:"status"`
	}
	if err := c.call(ctx, http.MethodPost, "/api/inbox/"+url.PathEscape(id)+"/"+action, body, &item, action); err != nil {
		return err
	}
	fmt.Printf("%s (status: %s)\n", ok, item.Status)
	return nil
}

type cliRevision struct {
	ID             string `json:"id"`
	RevisionNo     int    `json:"revision_no"`
	BaseSHA        string `json:"base_sha"`
	GroupID        string `json:"group_id"`
	Label          string `json:"label"`
	CandidateState string `json:"candidate_state"`
	Steps          []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Phase       string `json:"phase"`
	} `json:"steps"`
	Skills []string `json:"skills"`
}

type cliItem struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	PlanID  string `json:"plan_id"`
	Content string `json:"content"`
}

// showPlan prints the item's latest plan revision — what an approval binds.
func (c *apiClient) showPlan(ctx context.Context, out io.Writer, id string) (*cliRevision, *cliItem, error) {
	rev, item, _, err := c.showPlanFull(ctx, out, id)
	return rev, item, err
}

// showPlanFull is showPlan that also handles a multi-agent plan round: with
// two or more open candidates it prints them numbered (the order Slack and
// the dashboard use) and returns them instead of a single revision.
func (c *apiClient) showPlanFull(ctx context.Context, out io.Writer, id string) (*cliRevision, *cliItem, []cliRevision, error) {
	var item cliItem
	if err := c.call(ctx, http.MethodGet, "/api/inbox/"+url.PathEscape(id), nil, &item, "reading item"); err != nil {
		return nil, nil, nil, err
	}
	pf := func(f string, a ...any) { _, _ = fmt.Fprintf(out, f, a...) }
	pf("%s  [%s]\n", item.Title, item.Status)
	if item.PlanID == "" {
		pf("(no plan linked yet)\n")
		return nil, &item, nil, nil
	}
	var revs []cliRevision
	if err := c.call(ctx, http.MethodGet, "/api/plans/"+url.PathEscape(item.PlanID)+"/revisions", nil, &revs, "reading revisions"); err != nil {
		return nil, &item, nil, err
	}
	var cands []cliRevision
	for _, r := range revs { // newest first: the newest candidate's round
		if r.GroupID == "" {
			continue
		}
		for i := len(revs) - 1; i >= 0; i-- {
			if revs[i].GroupID == r.GroupID && revs[i].CandidateState == "open" {
				cands = append(cands, revs[i])
			}
		}
		break
	}
	printSteps := func(rev cliRevision, indent string) {
		n := 0
		for _, st := range rev.Steps {
			if st.Phase == "review" {
				continue
			}
			n++
			pf("%s%d. %s\n", indent, n, st.Title)
			if d := strings.TrimSpace(st.Description); d != "" {
				pf("%s   %s\n", indent, strings.ReplaceAll(d, "\n", "\n   "+indent))
			}
		}
		if len(rev.Skills) > 0 {
			pf("\n%sSkills: %s\n", indent, strings.Join(rev.Skills, ", "))
		}
	}
	short := func(sha string) string {
		if len(sha) > 10 {
			return sha[:10]
		}
		return sha
	}
	if len(cands) > 1 {
		pf("%d plan candidates — approve one (--revision <id>, or interactively)\n", len(cands))
		for i, cr := range cands {
			pf("\nCandidate %d · %s (%s) · base %s\n", i+1, cr.Label, cr.ID, short(cr.BaseSHA))
			printSteps(cr, "  ")
		}
		if cmp := lastSectionOf(item.Content, "## Comparison"); cmp != "" {
			pf("\n%s\n", cmp)
		}
		if q := lastSectionOf(item.Content, "## Questions"); q != "" {
			pf("\n%s\n", q)
		}
		pf("\n")
		return nil, &item, cands, nil
	}
	var rev *cliRevision
	for i := range revs {
		if revs[i].CandidateState != "superseded" {
			rev = &revs[i]
			break
		}
	}
	if rev == nil {
		pf("(the plan has no revision yet)\n")
		return nil, &item, nil, nil
	}
	pf("Plan revision #%d (%s) · base %s\n\n", rev.RevisionNo, rev.ID, short(rev.BaseSHA))
	printSteps(*rev, "")
	if q := lastSectionOf(item.Content, "## Questions"); q != "" {
		pf("\n%s\n", q)
	}
	pf("\n")
	return rev, &item, cands, nil
}

type cliRun struct {
	Stage    string `json:"stage"`
	Status   string `json:"status"`
	Branch   string `json:"branch"`
	HeadSHA  string `json:"head_sha"`
	Diffstat string `json:"diffstat"`
}

// latestImplementRun is the newest successful implement run (runs list
// newest first) — its pushed head is what mark-done approves.
func (c *apiClient) latestImplementRun(ctx context.Context, id string) (*cliRun, error) {
	var runs []cliRun
	if err := c.call(ctx, http.MethodGet, "/api/runs?item_id="+url.QueryEscape(id), nil, &runs, "reading runs"); err != nil {
		return nil, err
	}
	for i := range runs {
		if runs[i].Stage == "implement" && runs[i].Status == "succeeded" && runs[i].HeadSHA != "" {
			return &runs[i], nil
		}
	}
	return nil, fmt.Errorf("no successful implement run with a pushed head — nothing to approve yet")
}
