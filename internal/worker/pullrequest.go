package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/srvurl"
)

// prClient opens, finds and closes pull requests on the forge the worker
// pushes to (GitHub or Gitea), with the repo's push token. It only ever
// touches pull requests whose head is the item's own branch, and never
// merges: merging stays with the approver.
type prClient struct {
	kind    string // github | gitea
	apiBase string // https://api.github.com, https://host/api/v1, …
	owner   string
	repo    string
	token   string
	http    *http.Client
}

// PullRequest is the part of a forge pull request the worker uses.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	// Merged: GitHub reports merged_at, Gitea a merged flag.
	MergedAt *string `json:"merged_at"`
	Merged   bool    `json:"merged"`
	// Body tells skopos's pull requests from someone else's on a branch
	// taken over (prMarker).
	Body string `json:"body"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// newPRClient derives the API from the push URL: github.com uses
// api.github.com, any other GitHub host its /api/v3, Gitea its /api/v1
// (under the same path prefix when Gitea is served from a subpath). The
// last two path segments are owner and repository.
func newPRClient(kind, remoteURL, token string) (*prClient, error) {
	u, err := url.Parse(remoteURL)
	if err != nil || u.Host == "" || !srvurl.HTTPScheme(u.Scheme) {
		return nil, fmt.Errorf("push.remote_url %q is not an http(s) URL", remoteURL)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return nil, fmt.Errorf("push.remote_url %q does not end in owner/repository", remoteURL)
	}
	owner, repo := parts[len(parts)-2], strings.TrimSuffix(parts[len(parts)-1], ".git")
	prefix := strings.Join(parts[:len(parts)-2], "/")
	if prefix != "" {
		prefix = "/" + prefix
	}
	c := &prClient{kind: kind, owner: owner, repo: repo, token: token, http: &http.Client{Timeout: 30 * time.Second}}
	switch kind {
	case "github":
		if strings.EqualFold(u.Host, "github.com") {
			c.apiBase = "https://api.github.com"
		} else {
			c.apiBase = u.Scheme + "://" + u.Host + prefix + "/api/v3"
		}
	case "gitea":
		c.apiBase = u.Scheme + "://" + u.Host + prefix + "/api/v1"
	default:
		return nil, fmt.Errorf("pull requests need push.kind github or gitea, not %q", kind)
	}
	return c, nil
}

// Ensure returns the item's pull request from head into base, and what it
// did: "reused" an open one, "reopened" one that was closed without being
// merged (an approver closed it, or the cleanup after a Reject that was
// then undone), or "opened" a new one. Change-request rounds push to the
// same branch, so they land on the PR the first round opened. Gitea has no
// draft flag in its API; a draft there is a "WIP: " title, which Gitea
// treats as work in progress.
//
// A closed pull request is reopened only when owns says skopos opened it: on
// a branch taken over from someone, their closed pull request stays closed
// and a new one is opened.
func (c *prClient) Ensure(ctx context.Context, head, base, title, body string, draft bool, owns func(*PullRequest) bool) (*PullRequest, string, error) {
	pr, err := c.find(ctx, head, base)
	if err != nil {
		return nil, "", err
	}
	if pr != nil && pr.State == "open" {
		return pr, "reused", nil
	}
	if pr != nil && !pr.Merged && pr.MergedAt == nil && owns(pr) {
		var out PullRequest
		if err := c.call(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", c.repoPath(), pr.Number), map[string]any{"state": "open"}, &out); err == nil {
			if out.Number == 0 {
				out = *pr
				out.State = "open"
			}
			return &out, "reopened", nil
		}
		// Not reopenable (the forge refuses, e.g. the branch was rewritten):
		// open a new one.
	}
	in := map[string]any{"title": title, "head": head, "base": base, "body": body}
	if c.kind == "github" {
		in["draft"] = draft
	} else if draft {
		in["title"] = "WIP: " + title
	}
	var created PullRequest
	if err := c.call(ctx, http.MethodPost, c.repoPath()+"/pulls", in, &created); err != nil {
		return nil, "", err
	}
	return &created, "opened", nil
}

// Close closes a pull request without merging it.
func (c *prClient) Close(ctx context.Context, number int) error {
	return c.call(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", c.repoPath(), number), map[string]any{"state": "closed"}, nil)
}

// find returns the newest pull request from head into base in any state,
// or nil. GitHub filters the list by head and base; Gitea looks the pair
// up directly (GET /pulls/{base}/{head}), so a busy repo's first page of
// open PRs never hides it.
func (c *prClient) find(ctx context.Context, head, base string) (*PullRequest, error) {
	if c.kind == "gitea" {
		var pr PullRequest
		err := c.call(ctx, http.MethodGet, c.repoPath()+"/pulls/"+url.PathEscape(base)+"/"+head, nil, &pr)
		var pe *prError
		if errors.As(err, &pe) && pe.Status == http.StatusNotFound {
			return nil, nil
		}
		if err != nil || pr.Number == 0 {
			return nil, err
		}
		return &pr, nil
	}
	q := url.Values{"state": {"all"}, "head": {c.owner + ":" + head}, "base": {base}, "sort": {"created"}, "direction": {"desc"}, "per_page": {"20"}}
	var prs []PullRequest
	if err := c.call(ctx, http.MethodGet, c.repoPath()+"/pulls?"+q.Encode(), nil, &prs); err != nil {
		return nil, err
	}
	for i := range prs {
		if prs[i].Head.Ref == head && prs[i].Base.Ref == base {
			return &prs[i], nil
		}
	}
	return nil, nil
}

// prError is a forge API refusal: Status tells a retryable outage (5xx)
// from a final no (4xx: no permission, already merged, gone).
type prError struct {
	Status int
	Msg    string
}

func (e *prError) Error() string { return e.Msg }

func (c *prClient) repoPath() string {
	return "/repos/" + url.PathEscape(c.owner) + "/" + url.PathEscape(c.repo)
}

func (c *prClient) call(ctx context.Context, method, path string, in, out any) error {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, rd)
	if err != nil {
		return err
	}
	if c.kind == "github" {
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	} else {
		req.Header.Set("Authorization", "token "+c.token)
		req.Header.Set("Accept", "application/json")
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &prError{Status: resp.StatusCode, Msg: fmt.Sprintf("%s %s: %d %s", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode, msg)}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ensurePullRequest opens (or finds) the item's pull request into the
// default branch: the item title, then the implementer's summary, the
// approved plan's steps and where the sign-off happens.
func (w *Worker) ensurePullRequest(ctx context.Context, repo *Repo, token, branch string, item *Item, steps []Step, summary string, draft bool) (*PullRequest, string, error) {
	c, err := newPRClient(repo.Push.Kind, repo.Push.RemoteURL, token)
	if err != nil {
		return nil, "", err
	}
	var b strings.Builder
	if s := strings.TrimSpace(summary); s != "" {
		b.WriteString(s + "\n\n")
	}
	if len(steps) > 0 {
		b.WriteString("### Plan\n\n")
		for i, st := range steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, st.Title)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "---\nOpened by the skopos worker `%s` for %s. Sign off in skopos (Mark done), then merge here: skopos never merges.\n", w.cfg.ID, prMarker(item.ID))
	return c.Ensure(ctx, branch, repo.DefaultBranch, item.Title, b.String(), draft, func(pr *PullRequest) bool {
		return ownsPullRequest(pr, item.ID)
	})
}

// prMarker is in the body of every pull request skopos opens for an item.
func prMarker(itemID string) string { return "inbox item `" + itemID + "`" }

// ownsPullRequest: skopos opened this pull request for the item (any of its
// workers). Someone else's — on a branch taken over — is reused while open,
// but never reopened or closed.
func ownsPullRequest(pr *PullRequest, itemID string) bool {
	return pr != nil && strings.Contains(pr.Body, prMarker(itemID))
}
