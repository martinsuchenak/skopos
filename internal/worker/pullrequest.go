package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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
	Head   struct {
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
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
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

// Ensure returns the open pull request from head into base, opening one
// when there is none. Change-request rounds push to the same branch, so
// they find and reuse the PR the first round opened. Gitea has no draft
// flag in its API; a draft there is a "WIP: " title, which Gitea treats
// as work in progress.
func (c *prClient) Ensure(ctx context.Context, head, base, title, body string, draft bool) (*PullRequest, bool, error) {
	if pr, err := c.find(ctx, head, base); err != nil || pr != nil {
		return pr, false, err
	}
	in := map[string]any{"title": title, "head": head, "base": base, "body": body}
	if c.kind == "github" {
		in["draft"] = draft
	} else if draft {
		in["title"] = "WIP: " + title
	}
	var pr PullRequest
	if err := c.call(ctx, http.MethodPost, c.repoPath()+"/pulls", in, &pr); err != nil {
		return nil, false, err
	}
	return &pr, true, nil
}

// Close closes a pull request without merging it.
func (c *prClient) Close(ctx context.Context, number int) error {
	return c.call(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", c.repoPath(), number), map[string]any{"state": "closed"}, nil)
}

func (c *prClient) find(ctx context.Context, head, base string) (*PullRequest, error) {
	q := url.Values{"state": {"open"}}
	if c.kind == "github" {
		q.Set("head", c.owner+":"+head)
		q.Set("base", base)
	} else {
		q.Set("limit", "50")
	}
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
func (w *Worker) ensurePullRequest(ctx context.Context, repo *Repo, token, branch string, item *Item, steps []Step, summary string) (*PullRequest, bool, error) {
	c, err := newPRClient(repo.Push.Kind, repo.Push.RemoteURL, token)
	if err != nil {
		return nil, false, err
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
	fmt.Fprintf(&b, "---\nOpened by the skopos worker `%s` for inbox item `%s`. Sign off in skopos (Mark done), then merge here: skopos never merges.\n", w.cfg.ID, item.ID)
	return c.Ensure(ctx, branch, repo.DefaultBranch, item.Title, b.String(), repo.Push.PullRequest == "draft")
}
