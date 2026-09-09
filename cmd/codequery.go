package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/martinsuchenak/skopos/internal/codeindex"
	"github.com/paularlott/cli"
)

func execCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

// postJSON performs an authenticated JSON/ndjson request and decodes the
// response into T.
func postJSON[T any](ctx context.Context, serverURL, apiKey, method, path string, body any, contentType string) (T, error) {
	var zero T
	var payload []byte
	switch b := body.(type) {
	case []byte:
		payload = b
	default:
		raw, err := json.Marshal(body)
		if err != nil {
			return zero, err
		}
		payload = raw
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(serverURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", contentType)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return zero, fmt.Errorf("%s", apiErrorMessage(method+" "+path, resp))
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, fmt.Errorf("decoding response: %w", err)
	}
	return out, nil
}

// getJSON performs an authenticated GET and decodes the response into T.
func getJSON[T any](ctx context.Context, serverURL, apiKey, path string) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(serverURL, "/")+path, nil)
	if err != nil {
		return zero, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return zero, fmt.Errorf("%s", apiErrorMessage("GET "+path, resp))
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, fmt.Errorf("decoding response: %w", err)
	}
	return out, nil
}

func init() {
	Register(codeSearchCmd())
	Register(codeSymbolCmd())
	Register(codeWhoCallsCmd())
	Register(codeOutlineCmd())
}

// queryFlags are the shared flags of the code query commands: remote server
// or a local index directory.
func queryFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "server-url", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}, Usage: "Remote skopos (omit to query a local index-dir)"},
		&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
		&cli.StringFlag{Name: "index-dir", DefaultValue: ".skopos/indexes", ConfigPath: []string{"codeindex.dir"}, Usage: "Local index directory (when no server-url)"},
		&cli.StringFlag{Name: "workspace", Usage: "Workspace ID"},
		&cli.StringFlag{Name: "branch", Usage: "Branch (default: the workspace's default branch)"},
		&cli.BoolFlag{Name: "json", Usage: "Output raw JSON (same shape as the REST API and MCP tools)"},
	}
}

// printJSON emits a result in the canonical JSON shape shared with the REST
// API and MCP tools — one format, two transports.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func resolveQueryWorkspace(cmd *cli.Command) string {
	ws := cmd.GetString("workspace")
	if ws == "" {
		ws = workspaceOrDefault("")
	}
	return ws
}

// queryTarget routes a search/symbol/graph query to a remote server or a
// local index store.
func queryTarget[T any](ctx context.Context, cmd *cli.Command, remotePath func(workspace, branch string) string, local func(svc *codeindex.Service, workspace, branch string) (T, error)) (T, error) {
	var zero T
	workspace := resolveQueryWorkspace(cmd)
	if workspace == "" {
		return zero, fmt.Errorf("--workspace is required")
	}
	branch := cmd.GetString("branch")
	if cmd.GetString("server-url") != "" {
		return getJSON[T](ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), remotePath(workspace, branch))
	}
	store, err := codeindex.NewStore(cmd.GetString("index-dir"))
	if err != nil {
		return zero, err
	}
	defer store.Close()
	return local(codeindex.NewService(store), workspace, branch)
}

// displayName returns the fully-qualified form when one exists.
func displayName(name, qualified string) string {
	if qualified != "" {
		return qualified
	}
	return name
}

// printHit is the canonical symbol line used by every command:
//
//	{FQN}  {kind}  {path}:{line}
//	    {declaration}
//
// prefix carries command-specific leading columns (depth, tree indent);
// continuation lines align under the symbol.
func printHit(prefix, indent, name, qualified, kind, path string, line int, signature string) {
	fmt.Printf("%s%-44s %-9s %s:%d\n", prefix, displayName(name, qualified), kind, path, line)
	if signature != "" {
		fmt.Printf("%s    %s\n", indent, signature)
	}
}

func printHits(res codeindex.SearchResults) {
	if res.Note != "" {
		fmt.Println("note:", res.Note)
	}
	if len(res.Hits) == 0 {
		fmt.Println("no matches")
		return
	}
	for _, h := range res.Hits {
		printHit("", "", h.Name, h.Qualified, h.Kind, h.Path, h.Line, h.Signature)
	}
}

func codeSearchCmd() *cli.Command {
	return &cli.Command{
		Name:    "search",
		Usage:   "Full-text symbol search in the code index",
		MinArgs: 1, MaxArgs: 1,
		Flags: queryFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			q := cmd.GetArgs()[0]
			res, err := queryTarget(ctx, cmd,
				func(ws, branch string) string {
					return fmt.Sprintf("/api/codeindex/%s/search?q=%s&branch=%s", url.PathEscape(ws), url.QueryEscape(q), url.QueryEscape(branch))
				},
				func(svc *codeindex.Service, ws, branch string) (codeindex.SearchResults, error) {
					r, err := svc.Search(ctx, ws, branch, q, 0)
					if err != nil {
						return codeindex.SearchResults{}, err
					}
					return *r, nil
				})
			if err != nil {
				return err
			}
			if cmd.GetBool("json") {
				return printJSON(res)
			}
			printHits(res)
			return nil
		},
	}
}

func codeSymbolCmd() *cli.Command {
	return &cli.Command{
		Name:    "symbol",
		Usage:   "Find a symbol's definitions by exact name",
		MinArgs: 1, MaxArgs: 1,
		Flags: queryFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.GetArgs()[0]
			res, err := queryTarget(ctx, cmd,
				func(ws, branch string) string {
					return fmt.Sprintf("/api/codeindex/%s/symbol?name=%s&branch=%s", url.PathEscape(ws), url.QueryEscape(name), url.QueryEscape(branch))
				},
				func(svc *codeindex.Service, ws, branch string) (codeindex.SearchResults, error) {
					r, err := svc.Symbol(ctx, ws, branch, name)
					if err != nil {
						return codeindex.SearchResults{}, err
					}
					return *r, nil
				})
			if err != nil {
				return err
			}
			if cmd.GetBool("json") {
				return printJSON(res)
			}
			printHits(res)
			return nil
		},
	}
}

func codeWhoCallsCmd() *cli.Command {
	return &cli.Command{
		Name:    "who-calls",
		Usage:   "List call sites of a symbol",
		MinArgs: 1, MaxArgs: 1,
		Flags: queryFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.GetArgs()[0]
			res, err := queryTarget(ctx, cmd,
				func(ws, branch string) string {
					return fmt.Sprintf("/api/codeindex/%s/callers?name=%s&branch=%s", url.PathEscape(ws), url.QueryEscape(name), url.QueryEscape(branch))
				},
				func(svc *codeindex.Service, ws, branch string) (codeindex.GraphResults, error) {
					r, err := svc.Callers(ctx, ws, branch, name, 0)
					if err != nil {
						return codeindex.GraphResults{}, err
					}
					return *r, nil
				})
			if err != nil {
				return err
			}
			if cmd.GetBool("json") {
				return printJSON(res)
			}
			if res.Note != "" {
				fmt.Println("note:", res.Note)
			}
			if len(res.Edges) == 0 {
				fmt.Println("no call sites found")
				return nil
			}
			for _, e := range res.Edges {
				caller := e.Caller
				if caller == "" {
					caller = "(file scope)"
				}
				fmt.Printf("%-32s -> %s  %s:%d\n", caller, e.Callee, e.Path, e.Line)
			}
			return nil
		},
	}
}

func codeOutlineCmd() *cli.Command {
	return &cli.Command{
		Name:    "outline",
		Usage:   "List a file's definitions in source order",
		MinArgs: 1, MaxArgs: 1,
		Flags: queryFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			path := strings.TrimPrefix(cmd.GetArgs()[0], "/")
			res, err := queryTarget(ctx, cmd,
				func(ws, branch string) string {
					return fmt.Sprintf("/api/codeindex/%s/outline?path=%s&branch=%s", url.PathEscape(ws), url.QueryEscape(path), url.QueryEscape(branch))
				},
				func(svc *codeindex.Service, ws, branch string) (codeindex.SearchResults, error) {
					r, err := svc.Outline(ctx, ws, branch, path)
					if err != nil {
						return codeindex.SearchResults{}, err
					}
					return *r, nil
				})
			if err != nil {
				return err
			}
			if cmd.GetBool("json") {
				return printJSON(res)
			}
			if res.Note != "" {
				fmt.Println("note:", res.Note)
			}
			if len(res.Hits) == 0 {
				fmt.Println("no indexed definitions in that file")
				return nil
			}
			// IDE-outline style: types at top level, members indented beneath
			// their class (qualified names carry the nesting).
			lastContainer := ""
			for _, h := range res.Hits {
				indent := ""
				if h.Qualified != "" {
					container := strings.SplitN(h.Qualified, "::", 2)[0]
					if container != lastContainer {
						// container header already printed as its own hit row
						lastContainer = container
					}
					indent = "    "
				} else if lastContainer != "" {
					lastContainer = "" // back to top level
				}
				fmt.Printf("%5d  %s%-9s %s\n", h.Line, indent, h.Kind, h.Name)
				if h.Signature != "" && h.Qualified != "" {
					fmt.Printf("       %s  %s\n", indent, clipSignature(h.Signature))
				}
			}
			return nil
		},
	}
}

var _ = io.Discard // keep io imported for future streaming helpers

func init() {
	Register(codeImpactCmd())
	Register(codeDeadCmd())
	Register(codeCyclesCmd())
	Register(codeCallTreeCmd())
	Register(codeBranchDiffCmd())
}

func codeImpactCmd() *cli.Command {
	return &cli.Command{
		Name:    "impact",
		Usage:   "Show what is transitively affected by changing a symbol",
		Flags:   queryFlags(),
		MinArgs: 1, MaxArgs: 1,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.GetArgs()[0]
			depth := 3
			res, err := queryTarget(ctx, cmd,
				func(ws, branch string) string {
					return fmt.Sprintf("/api/codeindex/%s/impact?name=%s&branch=%s&depth=%d", url.PathEscape(ws), url.QueryEscape(name), url.QueryEscape(branch), depth)
				},
				func(svc *codeindex.Service, ws, branch string) (codeindex.ImpactResults, error) {
					r, err := svc.Impact(ctx, ws, branch, name, depth)
					if err != nil {
						return codeindex.ImpactResults{}, err
					}
					return *r, nil
				})
			if err != nil {
				return err
			}
			if cmd.GetBool("json") {
				return printJSON(res)
			}
			if len(res.Affected) == 0 {
				fmt.Println("no affected symbols found")
				return nil
			}
			for _, a := range res.Affected {
				if a.Path != "" {
					fmt.Printf("  depth %d  %-44s %s:%d\n", a.Depth, a.Name, a.Path, a.Line)
				} else {
					fmt.Printf("  depth %d  %s\n", a.Depth, a.Name)
				}
			}
			return nil
		},
	}
}

func codeDeadCmd() *cli.Command {
	return &cli.Command{
		Name:    "dead-code",
		Usage:   "List symbols with no incoming call references (verify before deleting)",
		Flags:   queryFlags(),
		MinArgs: 0, MaxArgs: 0,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			res, err := queryTarget(ctx, cmd,
				func(ws, branch string) string {
					return fmt.Sprintf("/api/codeindex/%s/dead?branch=%s", url.PathEscape(ws), url.QueryEscape(branch))
				},
				func(svc *codeindex.Service, ws, branch string) (codeindex.DeadResult, error) {
					r, err := svc.Dead(ctx, ws, branch, 0)
					if err != nil {
						return codeindex.DeadResult{}, err
					}
					return *r, nil
				})
			if err != nil {
				return err
			}
			if cmd.GetBool("json") {
				return printJSON(res)
			}
			if res.Note != "" {
				fmt.Println("note:", res.Note)
			}
			if len(res.Symbols) == 0 {
				fmt.Println("no dead-code candidates")
				return nil
			}
			for _, s := range res.Symbols {
				printHit("", "", s.Name, s.Qualified, s.Kind, s.Path, s.Line, s.Signature)
			}
			return nil
		},
	}
}

func codeCyclesCmd() *cli.Command {
	return &cli.Command{
		Name:  "cycles",
		Usage: "Find cycles in the call graph",
		Flags: queryFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			res, err := queryTarget(ctx, cmd,
				func(ws, branch string) string {
					return fmt.Sprintf("/api/codeindex/%s/cycles?branch=%s", url.PathEscape(ws), url.QueryEscape(branch))
				},
				func(svc *codeindex.Service, ws, branch string) (codeindex.CyclesResult, error) {
					r, err := svc.Cycles(ctx, ws, branch)
					if err != nil {
						return codeindex.CyclesResult{}, err
					}
					return *r, nil
				})
			if err != nil {
				return err
			}
			if cmd.GetBool("json") {
				return printJSON(res)
			}
			if len(res.Cycles) == 0 {
				fmt.Println("no cycles found")
				return nil
			}
			for _, c := range res.Cycles {
				parts := make([]string, 0, len(c.Names)+1)
				for _, n := range c.Names {
					if loc, ok := c.Locations[n]; ok {
						parts = append(parts, fmt.Sprintf("%s (%s)", n, loc))
					} else {
						parts = append(parts, n)
					}
				}
				first := c.Names[0]
				if loc, ok := c.Locations[first]; ok {
					first = fmt.Sprintf("%s (%s)", first, loc)
				}
				fmt.Println(strings.Join(parts, " -> ") + " -> " + first)
			}
			return nil
		},
	}
}

func codeCallTreeCmd() *cli.Command {
	return &cli.Command{
		Name:    "call-tree",
		Usage:   "Expand what a symbol calls, recursively",
		Flags:   append(queryFlags(), &cli.BoolFlag{Name: "mermaid", Usage: "Emit a Mermaid flowchart instead of ASCII"}),
		MinArgs: 1, MaxArgs: 1,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			name := cmd.GetArgs()[0]
			depth := 3
			res, err := queryTarget(ctx, cmd,
				func(ws, branch string) string {
					return fmt.Sprintf("/api/codeindex/%s/call-tree?name=%s&branch=%s&depth=%d", url.PathEscape(ws), url.QueryEscape(name), url.QueryEscape(branch), depth)
				},
				func(svc *codeindex.Service, ws, branch string) (codeindex.CallTreeResult, error) {
					r, err := svc.CallTree(ctx, ws, branch, name, depth)
					if err != nil {
						return codeindex.CallTreeResult{}, err
					}
					return *r, nil
				})
			if err != nil {
				return err
			}
			if cmd.GetBool("json") {
				return printJSON(res)
			}
			if res.Note != "" {
				fmt.Println("note:", res.Note)
			}
			if cmd.GetBool("mermaid") {
				fmt.Println("flowchart TD")
				mermaidNodes(res.Tree)
				return nil
			}
			printTree(res.Tree, 0)
			return nil
		},
	}
}

func printTree(n codeindex.CallTreeNode, depth int) {
	indent := strings.Repeat("  ", depth)
	loc := ""
	if n.Path != "" {
		loc = fmt.Sprintf("  %s:%d", n.Path, n.Line)
	}
	fmt.Printf("%s%s%s\n", indent, n.Name, loc)
	for _, c := range n.Children {
		printTree(c, depth+1)
	}
}

func mermaidNodes(n codeindex.CallTreeNode) {
	var walk func(node codeindex.CallTreeNode)
	seen := map[string]bool{}
	label := func(n codeindex.CallTreeNode) string {
		if n.Path != "" {
			return fmt.Sprintf("%s (%s:%d)", n.Name, n.Path, n.Line)
		}
		return n.Name
	}
	walk = func(node codeindex.CallTreeNode) {
		if seen[node.Name] {
			return
		}
		seen[node.Name] = true
		for _, c := range node.Children {
			fmt.Printf("  %q --> %q\n", label(node), label(c))
			walk(c)
		}
	}
	walk(n)
}

func codeBranchDiffCmd() *cli.Command {
	return &cli.Command{
		Name:    "branch-diff",
		Usage:   "Compare a feature branch's indexed symbols against the default branch",
		Flags:   queryFlags(),
		MinArgs: 1, MaxArgs: 1,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			branch := cmd.GetArgs()[0]
			res, err := queryTarget(ctx, cmd,
				func(ws, _ string) string {
					return fmt.Sprintf("/api/codeindex/%s/branch-diff?branch=%s", url.PathEscape(ws), url.QueryEscape(branch))
				},
				func(svc *codeindex.Service, ws, _ string) (codeindex.BranchDiffResult, error) {
					r, err := svc.BranchDiff(ctx, ws, branch)
					if err != nil {
						return codeindex.BranchDiffResult{}, err
					}
					return *r, nil
				})
			if err != nil {
				return err
			}
			if cmd.GetBool("json") {
				return printJSON(res)
			}
			fmt.Printf("branch %s vs %s\n", res.Branch, res.Base)
			for _, f := range res.Files {
				fmt.Printf("  %-8s %s\n", f.Change, f.Path)
			}
			for _, s := range res.Symbols {
				fmt.Printf("  %-8s %-9s %-32s %s\n", s.Change, s.Kind, s.Name, s.Path)
			}
			return nil
		},
	}
}

// clipSignature keeps outline hover lines readable.
func clipSignature(sig string) string {
	if len(sig) > 100 {
		return sig[:97] + "..."
	}
	return sig
}
