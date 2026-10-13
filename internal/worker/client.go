package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Client is the worker's REST client against skopos (claim/heartbeat/report,
// inbox reads, plan writes). The API key travels as the bearer credential.
// Lease tokens (M5) live in memory only: a restart loses them, the lease
// expires to interrupted, and the re-claim issues a fresh token — restart
// safety comes for free.
type Client struct {
	base, key string
	http      *http.Client
	leases    map[string]string // run id -> fencing token
	mu        sync.Mutex
}

func NewClient(base, key string) *Client {
	return &Client{base: base, key: key, http: &http.Client{Timeout: 60 * time.Second}, leases: map[string]string{}}
}

// doHeader is do with one extra header (the lease token).
func (c *Client) doHeader(ctx context.Context, method, path string, body, out any, name, value string) error {
	if value == "" {
		return c.do(ctx, method, path, body, out)
	}
	// minimal: reuse do with a header via a wrapper request
	req, err := c.build(ctx, method, path, body)
	if err != nil {
		return err
	}
	req.Header.Set(name, value)
	return c.exec(req, out)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.build(ctx, method, path, body)
	if err != nil {
		return err
	}
	return c.exec(req, out)
}

func (c *Client) build(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) exec(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("skopos %s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Claim polls the queue. (nil, nil) = nothing to do.
func (c *Client) Claim(ctx context.Context, workerID string, runners, workspaces []string) (*Run, error) {
	var run Run
	req := map[string]any{"worker_id": workerID, "runners": runners, "workspaces": workspaces}
	// 204 handling: do() returns nil without touching out.
	if err := c.do(ctx, http.MethodPost, "/api/runs/claim", req, &run); err != nil {
		return nil, err
	}
	if run.ID == "" {
		return nil, nil
	}
	if run.LeaseToken != "" {
		c.mu.Lock()
		c.leases[run.ID] = run.LeaseToken
		c.mu.Unlock()
	}
	return &run, nil
}

func (c *Client) leaseHeader(runID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leases[runID]
}

func (c *Client) Heartbeat(ctx context.Context, runID, workerID string) error {
	return c.doHeader(ctx, http.MethodPost, "/api/runs/"+runID+"/heartbeat", map[string]string{"worker_id": workerID}, nil, "X-Skopos-Lease", c.leaseHeader(runID))
}

func (c *Client) Report(ctx context.Context, runID string, in map[string]any) error {
	in["worker_id"] = in["worker_id"]
	return c.do(ctx, http.MethodPost, "/api/runs/"+runID+"/report", in, nil)
}

func (c *Client) ReportResult(ctx context.Context, runID, workerID string, succeeded bool, fields map[string]any) error {
	body := map[string]any{"worker_id": workerID}
	if succeeded {
		body["status"] = "succeeded"
	} else {
		body["status"] = "failed"
	}
	for k, v := range fields {
		body[k] = v
	}
	err := c.doHeader(ctx, http.MethodPost, "/api/runs/"+runID+"/report", body, nil, "X-Skopos-Lease", c.leaseHeader(runID))
	if err == nil {
		c.dropLease(runID) // succeeded/failed are terminal — the token is spent
	}
	return err
}

func (c *Client) Pause(ctx context.Context, runID, workerID, reason string, resumeAfter time.Time) error {
	body := map[string]any{"worker_id": workerID, "reason": reason}
	if !resumeAfter.IsZero() {
		body["resume_after"] = resumeAfter.UTC().Format(time.RFC3339Nano)
	}
	return c.doHeader(ctx, http.MethodPost, "/api/runs/"+runID+"/pause", body, nil, "X-Skopos-Lease", c.leaseHeader(runID))
}

func (c *Client) CancelRun(ctx context.Context, runID, workerID, reason string) error {
	err := c.doHeader(ctx, http.MethodPost, "/api/runs/"+runID+"/cancel", map[string]string{"worker_id": workerID, "reason": reason}, nil, "X-Skopos-Lease", c.leaseHeader(runID))
	if err == nil {
		c.dropLease(runID)
	}
	return err
}

// dropLease forgets a run's fencing token once it reaches a terminal state
// (round-6 F4: the map leaked an entry per run on long-lived workers).
func (c *Client) dropLease(runID string) {
	c.mu.Lock()
	delete(c.leases, runID)
	c.mu.Unlock()
}

// GetItem fetches a full inbox item (list rows carry excerpts only).
func (c *Client) GetItem(ctx context.Context, id string) (*Item, error) {
	var it Item
	if err := c.do(ctx, http.MethodGet, "/api/inbox/"+id, nil, &it); err != nil {
		return nil, err
	}
	return &it, nil
}

// AppendSection appends a markdown section to the item (enrichment, review
// verdicts ride on the content).
func (c *Client) AppendSection(ctx context.Context, id, heading, body string) error {
	it, err := c.GetItem(ctx, id)
	if err != nil {
		return err
	}
	content := it.Content + "\n\n## " + heading + "\n\n" + body + "\n"
	return c.do(ctx, http.MethodPatch, "/api/inbox/"+id, map[string]string{"content": content}, nil)
}

// CreatePlan, AddPlanItem, CreateRevision, LinkPlan, Transition: the writes
// the stage executors need. Plan item text is immutable server-side.
func (c *Client) GetPlan(ctx context.Context, id string) (*Plan, error) {
	var p Plan
	err := c.do(ctx, http.MethodGet, "/api/plans/"+id, nil, &p)
	return &p, err
}

func (c *Client) CreatePlan(ctx context.Context, workspace, name, branch, agent string) (*Plan, error) {
	var p Plan
	err := c.do(ctx, http.MethodPost, "/api/plans", map[string]string{
		"workspace_id": workspace, "name": name, "branch_name": branch, "author_agent_id": agent,
	}, &p)
	return &p, err
}

func (c *Client) AddPlanItem(ctx context.Context, planID, title, description, phase string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/api/plans/"+planID+"/items", map[string]string{
		"title": title, "description": description, "phase": phase,
	}, &out)
	return out.ID, err
}

func (c *Client) DeletePlanItem(ctx context.Context, planID, itemID string) error {
	return c.do(ctx, http.MethodDelete, "/api/plans/"+planID+"/items/"+itemID, nil, nil)
}

func (c *Client) SetPlanItemStatus(ctx context.Context, planID, itemID, status string) error {
	return c.do(ctx, http.MethodPatch, "/api/plans/"+planID+"/items/"+itemID, map[string]string{"status": status}, nil)
}

func (c *Client) CreateRevision(ctx context.Context, planID, baseSHA string) (*Revision, error) {
	var rev Revision
	err := c.do(ctx, http.MethodPost, "/api/plans/"+planID+"/revisions", map[string]string{"base_sha": baseSHA}, &rev)
	return &rev, err
}

func (c *Client) AmendRevision(ctx context.Context, planID, baseSHA string) (*Revision, error) {
	var rev Revision
	err := c.do(ctx, http.MethodPost, "/api/plans/"+planID+"/revisions", map[string]string{"base_sha": baseSHA}, &rev)
	return &rev, err
}

// Replan atomically replaces all non-review steps and snapshots (M6).
func (c *Client) Replan(ctx context.Context, planID, baseSHA string, steps []Step) (*Revision, error) {
	var rev Revision
	err := c.do(ctx, http.MethodPost, "/api/plans/"+planID+"/replan", map[string]any{
		"base_sha": baseSHA, "steps": steps,
	}, &rev)
	return &rev, err
}

// Amend is the atomic amendment (append steps + snapshot past the lock).
func (c *Client) Amend(ctx context.Context, planID, baseSHA string, steps []Step) (*Revision, error) {
	var rev Revision
	err := c.do(ctx, http.MethodPost, "/api/plans/"+planID+"/amend", map[string]any{
		"base_sha": baseSHA, "steps": steps,
	}, &rev)
	return &rev, err
}

func (c *Client) LinkPlan(ctx context.Context, itemID, planID string) error {
	return c.do(ctx, http.MethodPost, "/api/inbox/"+itemID+"/link-plan", map[string]string{"plan_id": planID}, nil)
}

func (c *Client) Transition(ctx context.Context, itemID, to, reason string) error {
	return c.do(ctx, http.MethodPost, "/api/inbox/"+itemID+"/transition",
		map[string]string{"to": to, "reason": reason, "via": "worker"}, nil)
}

// --- wire types (kept lean; the server's openapi is the contract) ---

type Run struct {
	ID          string    `json:"id"`
	LeaseToken  string    `json:"lease_token,omitempty"` // fencing token (claim response only)
	WorkspaceID string    `json:"workspace_id"`
	ItemID      string    `json:"item_id"`
	PlanRevID   string    `json:"plan_rev_id"`
	Stage       string    `json:"stage"`
	Runner      string    `json:"runner"`
	WorkerID    string    `json:"worker_id"`
	Status      string    `json:"status"`
	ResumeKind  string    `json:"resume_kind"`
	Attempt     int       `json:"attempt"`
	Branch      string    `json:"branch"`
	BaseSHA     string    `json:"base_sha"`
	HeadSHA     string    `json:"head_sha"`
	CreatedAt   time.Time `json:"created_at"`
}

type Item struct {
	ID          string   `json:"id"`
	WorkspaceID string   `json:"workspace_id"`
	Title       string   `json:"title"`
	Content     string   `json:"content"`
	Tags        []string `json:"tags"`
	Status      string   `json:"status"`
	PlanID      string   `json:"plan_id"`
}

type Plan struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Items []PlanItem `json:"items"`
}

type PlanItem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

type Revision struct {
	ID          string `json:"id"`
	RevisionNo  int    `json:"revision_no"`
	ContentHash string `json:"content_hash"`
	BaseSHA     string `json:"base_sha"`
}
