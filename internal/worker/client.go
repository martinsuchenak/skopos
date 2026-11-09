package worker

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/build"
	"github.com/martinsuchenak/skopos/internal/logship"
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
	pollMS    int64 // idle poll interval, reported for presence
	// serverVersion: the release the server named on its last response.
	serverVersion string
	// indexEnabled / indexDefault: the index duty of the last claim answer
	// (indexer.go).
	indexEnabled, indexDefault []string
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
	return c.doHeaders(ctx, method, path, body, out, nil)
}

// doHeaders is do that also hands back the response headers (hdr non-nil).
func (c *Client) doHeaders(ctx context.Context, method, path string, body, out any, hdr *http.Header) error {
	req, err := c.build(ctx, method, path, body)
	if err != nil {
		return err
	}
	return c.execHeaders(req, out, hdr)
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
	// Every worker write is audited as via=worker (unset, the server
	// labels it "cli" — the end-to-end run showed the worker's content
	// edits recorded that way).
	req.Header.Set("X-Skopos-Via", "worker")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) exec(req *http.Request, out any) error { return c.execHeaders(req, out, nil) }

func (c *Client) execHeaders(req *http.Request, out any, hdr *http.Header) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if hdr != nil {
		*hdr = resp.Header
	}
	if v := resp.Header.Get("Skopos-Version"); v != "" {
		c.mu.Lock()
		c.serverVersion = v
		c.mu.Unlock()
	}
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode == http.StatusGone {
		// Only a heartbeat answers it: the run was cancelled.
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return &RunCancelledError{Reason: strings.TrimPrefix(cmp.Or(e.Error, "cancelled"), "run cancelled: ")}
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
func (c *Client) Claim(ctx context.Context, workerID string, runners, workspaces []string, unhealthy map[string]string, indexable []string) (*Run, error) {
	var run Run
	req := map[string]any{"worker_id": workerID, "runners": runners, "workspaces": workspaces,
		"version": build.Version, "poll_ms": c.pollMS}
	if len(unhealthy) > 0 {
		req["unhealthy"] = unhealthy
	}
	if len(indexable) > 0 {
		req["indexable"] = indexable
	}
	// 204 handling: do() returns nil without touching out.
	var hdr http.Header
	if err := c.doHeaders(ctx, http.MethodPost, "/api/runs/claim", req, &run, &hdr); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.indexEnabled, c.indexDefault = splitList(hdr.Get("Skopos-Index-Enabled")), splitList(hdr.Get("Skopos-Index-Default"))
	c.mu.Unlock()
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

// RunLogWriter streams a run's log lines to skopos (POST
// /api/runs/{id}/log, newline-delimited RunLogRecords), authorised by the
// claim's lease like a heartbeat. Lines a full or failing server can't take
// stay buffered (bounded); a refused batch (the lease is gone) is dropped.
func (c *Client) RunLogWriter(runID, workerID string) (*logship.Writer, error) {
	lease := c.leaseHeader(runID)
	if lease == "" {
		return nil, fmt.Errorf("run %s has no lease", runID)
	}
	return logship.New(logship.Config{
		URL:    c.base + "/api/runs/" + url.PathEscape(runID) + "/log",
		Format: logship.Raw,
		Headers: map[string]string{
			"Authorization": "Bearer " + c.key, "X-Skopos-Lease": lease,
			"X-Skopos-Worker": workerID, "X-Skopos-Via": "worker",
		},
		FlushEvery:  time.Second,
		BatchSize:   200,
		MaxBuffered: 5000,
	})
}

// IndexDuty is what the last claim answer said to index: the workspaces with
// the code_index switch on, and those whose default branch this worker
// keeps fresh.
func (c *Client) IndexDuty() (enabled, defaults []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.indexEnabled), slices.Clone(c.indexDefault)
}

// IndexedBranch is one branch of the server's code index.
type IndexedBranch struct {
	Head    string    // the commit the index was built from
	BuiltAt time.Time // zero when the server did not say
}

// IndexedBranches is the server's code index of a workspace, by branch.
func (c *Client) IndexedBranches(ctx context.Context, workspace string) (map[string]IndexedBranch, error) {
	var status []struct {
		Branch  string `json:"branch"`
		HeadSHA string `json:"head_sha"`
		BuiltAt string `json:"built_at"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/codeindex/"+url.PathEscape(workspace)+"/status", nil, &status); err != nil {
		return nil, err
	}
	out := make(map[string]IndexedBranch, len(status))
	for _, s := range status {
		b := IndexedBranch{Head: s.HeadSHA}
		if t, err := time.Parse(time.RFC3339Nano, s.BuiltAt); err == nil {
			b.BuiltAt = t
		}
		out[s.Branch] = b
	}
	return out, nil
}

// DropIndexBranch removes a branch's index from the server (404 = already
// gone).
func (c *Client) DropIndexBranch(ctx context.Context, workspace, branch string) error {
	err := c.do(ctx, http.MethodDelete, "/api/codeindex/"+url.PathEscape(workspace)+"/branch/"+url.PathEscape(branch), nil, nil)
	if err != nil && strings.Contains(err.Error(), ": 404 ") {
		return nil
	}
	return err
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ServerVersion is the release the server last named ("" before any poll,
// or from a server too old to say).
func (c *Client) ServerVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverVersion
}

// SetPollInterval: reported with every poll, so the server knows when a
// silent worker counts as offline.
func (c *Client) SetPollInterval(d time.Duration) { c.pollMS = d.Milliseconds() }

// Offline says goodbye on a clean shutdown (best effort).
func (c *Client) Offline(ctx context.Context, workerID string) error {
	return c.do(ctx, http.MethodPost, "/api/workers/"+url.PathEscape(workerID)+"/offline", map[string]any{}, nil)
}

// Heartbeat extends the run's lease. A non-empty phase says what a running
// implement run is doing (checks / fixing), for the dashboard.
func (c *Client) Heartbeat(ctx context.Context, runID, workerID, phase string) error {
	body := map[string]string{"worker_id": workerID}
	if phase != "" {
		body["phase"] = phase
	}
	return c.doHeader(ctx, http.MethodPost, "/api/runs/"+runID+"/heartbeat", body, nil, "X-Skopos-Lease", c.leaseHeader(runID))
}

func (c *Client) Report(ctx context.Context, runID string, in map[string]any) error {
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

// HeadChecks returns the checks the worker ran on head, from the item's
// newest implement run that reported that head (nil when none did or the
// runs can't be read).
func (c *Client) HeadChecks(ctx context.Context, itemID, head string) []Check {
	var runs []struct {
		Stage   string  `json:"stage"`
		HeadSHA string  `json:"head_sha"`
		Checks  []Check `json:"checks"`
	}
	if head == "" || c.do(ctx, http.MethodGet, "/api/runs?item_id="+url.QueryEscape(itemID), nil, &runs) != nil {
		return nil
	}
	for _, r := range runs { // newest first
		if r.Stage == "implement" && r.HeadSHA == head {
			return r.Checks
		}
	}
	return nil
}

// GroupChecks are the checks another member of a review-only panel already
// ran on head (nil when none has): members of the same round share them,
// and a new round runs its own.
func (c *Client) GroupChecks(ctx context.Context, itemID, groupID, head string) []Check {
	var runs []struct {
		Stage   string  `json:"stage"`
		GroupID string  `json:"group_id"`
		HeadSHA string  `json:"head_sha"`
		Checks  []Check `json:"checks"`
	}
	if groupID == "" || head == "" || c.do(ctx, http.MethodGet, "/api/runs?item_id="+url.QueryEscape(itemID), nil, &runs) != nil {
		return nil
	}
	for _, r := range runs {
		if r.Stage == "branch_review" && r.GroupID == groupID && r.HeadSHA == head && len(r.Checks) > 0 {
			return r.Checks
		}
	}
	return nil
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

// QuestionsPost is a run's questions for Martin (POST
// /api/runs/{id}/questions): who asks, about what, whether the item waits.
type QuestionsPost struct {
	WorkerID    string           `json:"worker_id"`
	Asker       string           `json:"asker"`
	Blocking    bool             `json:"blocking"`
	SubjectKind string           `json:"subject_kind,omitempty"`
	Subject     string           `json:"subject,omitempty"`
	Questions   []QuestionPosted `json:"questions"`
}

type QuestionPosted struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// AskQuestions posts a run's questions; the server records them and renders
// them into the item. false: the server predates the endpoint (404), and
// the caller appends the section itself.
func (c *Client) AskQuestions(ctx context.Context, runID string, in QuestionsPost) (bool, error) {
	err := c.doHeader(ctx, http.MethodPost, "/api/runs/"+runID+"/questions", in, nil, "X-Skopos-Lease", c.leaseHeader(runID))
	if err != nil && strings.Contains(err.Error(), ": 404 ") {
		return false, nil
	}
	return err == nil, err
}

// ReadQuestions is the item's questions and answers as the run's prompt
// reads them, rendered by the server from its record (the answers are
// marked as used by the run). false: the server predates the record (404).
func (c *Client) ReadQuestions(ctx context.Context, runID, workerID string) (string, bool, error) {
	var out struct {
		Prompt string `json:"prompt"`
	}
	err := c.doHeader(ctx, http.MethodPost, "/api/runs/"+runID+"/questions/read", map[string]string{"worker_id": workerID}, &out, "X-Skopos-Lease", c.leaseHeader(runID))
	if err != nil && strings.Contains(err.Error(), ": 404 ") {
		return "", false, nil
	}
	return out.Prompt, err == nil, err
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
func (c *Client) Replan(ctx context.Context, planID, baseSHA string, steps []Step, skills []string) (*Revision, error) {
	var rev Revision
	body := map[string]any{"base_sha": baseSHA, "steps": steps}
	if len(skills) > 0 {
		body["skills"] = skills
	}
	err := c.do(ctx, http.MethodPost, "/api/plans/"+planID+"/replan", body, &rev)
	return &rev, err
}

// CreateCandidate writes one planner's candidate in a multi-agent plan
// round (the live plan is untouched until Martin approves one).
func (c *Client) CreateCandidate(ctx context.Context, planID, baseSHA string, steps []Step, skills []string, groupID, label string) (*Revision, error) {
	var rev Revision
	body := map[string]any{"base_sha": baseSHA, "steps": steps, "group_id": groupID, "label": label}
	if len(skills) > 0 {
		body["skills"] = skills
	}
	err := c.do(ctx, http.MethodPost, "/api/plans/"+planID+"/candidates", body, &rev)
	return &rev, err
}

// RoundCandidates returns the open candidates of the plan's newest round,
// oldest first (numbered 1..n as at the gate), and the round's group id.
func (c *Client) RoundCandidates(ctx context.Context, planID string) ([]Revision, string, error) {
	var revs []Revision
	if err := c.do(ctx, http.MethodGet, "/api/plans/"+planID+"/revisions", nil, &revs); err != nil {
		return nil, "", err
	}
	group := ""
	for _, r := range revs { // newest first
		if r.GroupID != "" {
			group = r.GroupID
			break
		}
	}
	var out []Revision
	for i := len(revs) - 1; i >= 0; i-- {
		if group != "" && revs[i].GroupID == group && revs[i].State == "open" {
			out = append(out, revs[i])
		}
	}
	return out, group, nil
}

// PinBase reports the base this planner would read and returns the plan
// group's pinned base (the first member's) — every candidate plans at it.
func (c *Client) PinBase(ctx context.Context, runID, baseSHA string) (string, error) {
	var out struct {
		BaseSHA string `json:"base_sha"`
	}
	err := c.doHeader(ctx, http.MethodPost, "/api/runs/"+runID+"/pin-base", map[string]string{"base_sha": baseSHA}, &out, "X-Skopos-Lease", c.leaseHeader(runID))
	return out.BaseSHA, err
}

// ApprovedRevision is the revision a run executes: the run's own revision
// id, else the newest locked one (nil when the plan has none).
func (c *Client) ApprovedRevision(ctx context.Context, planID, revID string) (*Revision, error) {
	var revs []Revision
	if err := c.do(ctx, http.MethodGet, "/api/plans/"+planID+"/revisions", nil, &revs); err != nil {
		return nil, err
	}
	for i := range revs {
		if revID != "" && revs[i].ID == revID {
			return &revs[i], nil
		}
	}
	for i := range revs { // newest first
		if revID == "" && revs[i].LockedAt != nil {
			return &revs[i], nil
		}
	}
	return nil, nil
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
	ID          string `json:"id"`
	LeaseToken  string `json:"lease_token,omitempty"` // fencing token (claim response only)
	WorkspaceID string `json:"workspace_id"`
	ItemID      string `json:"item_id"`
	PlanRevID   string `json:"plan_rev_id"`
	Stage       string `json:"stage"`
	Runner      string `json:"runner"`
	WorkerID    string `json:"worker_id"`
	Status      string `json:"status"`
	ResumeKind  string `json:"resume_kind"`
	Attempt     int    `json:"attempt"`
	Branch      string `json:"branch"`
	BaseSHA     string `json:"base_sha"`
	HeadSHA     string `json:"head_sha"`
	// GroupID: set when the run is one member of a multi-agent stage;
	// Focus: the member's brief (.skopos/focus/<name>.md at the base).
	GroupID   string    `json:"group_id"`
	Focus     string    `json:"focus"`
	CreatedAt time.Time `json:"created_at"`
}

type Item struct {
	ID          string   `json:"id"`
	WorkspaceID string   `json:"workspace_id"`
	Title       string   `json:"title"`
	Content     string   `json:"content"`
	Tags        []string `json:"tags"`
	Status      string   `json:"status"`
	PlanID      string   `json:"plan_id"`
	// Branch, Ref and Flow: tmp/branch-handover — design.md.
	Branch string `json:"branch"`
	Ref    string `json:"ref"`
	Flow   string `json:"flow"`
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
	ID          string     `json:"id"`
	RevisionNo  int        `json:"revision_no"`
	ContentHash string     `json:"content_hash"`
	BaseSHA     string     `json:"base_sha"`
	Skills      []string   `json:"skills"`
	LockedAt    *time.Time `json:"locked_at"`
	Steps       []RevStep  `json:"steps"`
	Label       string     `json:"label"`
	GroupID     string     `json:"group_id"`
	State       string     `json:"candidate_state"`
}

// RevStep is one step of a revision snapshot.
type RevStep struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Phase       string `json:"phase"`
}

// RunCancelledError: skopos cancelled the run this worker holds (the item
// was rejected or marked done). execute stops it.
type RunCancelledError struct{ Reason string }

func (e *RunCancelledError) Error() string { return "run cancelled: " + e.Reason }
