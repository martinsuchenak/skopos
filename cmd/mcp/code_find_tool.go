package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/martinsuchenak/skopos/internal/codeindex"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterCodeIndexTool(registerCodeFindTool)
}

// codeFindDesc frames the tool-selection decision: this is the tool for
// questions that describe a ROLE or MECHANISM with no identifier to grep
// for — the measured anchorless regime where indexes beat grep on recall.
const codeFindDesc = "Find code by ROLE or DESCRIPTION — use when the question names no identifier ('where is deferred work drained?', 'which class renders errors as HTML?'). Prefer code_search when you have a name. Returns a compact top-N of file:line + name + kind + why it matched; open the file or code_symbol for detail."

func registerCodeFindTool(server *mcplib.Server, svc *codeindex.Service) {
	registerTool(server, 
		mcplib.NewTool("code_find",
			codeFindDesc,
			wsParam(),
			mcplib.String("q", "Natural-language description of the behavior, mechanism, or role", mcplib.Required()),
			mcplib.String("path", "Optional path prefix filter (e.g. app/Services)"),
			mcplib.String("branch", "Branch (optional — falls back to the default branch)"),
			mcplib.Integer("limit", "Max results (default 10; compact by design)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			limit := 10
			if n, err := req.Int("limit"); err == nil && n > 0 {
				limit = n
			}
			q := req.StringOr("q", "")
			pathPrefix := req.StringOr("path", "")
			branch := req.StringOr("branch", "")

			var res *codeindex.SearchResults
			if semanticSearcher != nil {
				// Semantic-first: embeddings fuse meaning with FTS; this is
				// the path of least resistance, not a flag the agent must
				// remember.
				res, err = svc.SemanticSearch(ctx, ws, branch, q, pathPrefix, limit, semanticSearcher)
			} else {
				// No embeddings: FTS can't match prose, so mine the query
				// for identifier-shaped tokens and AND them, dropping
				// stop-words — a description usually carries the key nouns.
				res, err = findFTS(ctx, svc, ws, branch, q, pathPrefix, limit)
			}
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(compactFind(res)), nil
		},
	)
}

// compactFind projects search hits to the minimum decision-relevant fields:
// measured token economics showed chunky results are paid for on every
// subsequent turn. Detail lives one code_symbol call away.
func compactFind(res *codeindex.SearchResults) map[string]any {
	hits := make([]map[string]any, 0, len(res.Hits))
	for _, h := range res.Hits {
		entry := map[string]any{
			"name": h.Name,
			"kind": h.Kind,
			"at":   fmt.Sprintf("%s:%d", h.Path, h.Line),
		}
		if h.MatchedBy != "" {
			entry["matched_by"] = h.MatchedBy
		}
		// One-line doc summary — the role hint that confirms the match.
		if h.Doc != "" {
			doc := h.Doc
			for i := 0; i < len(doc); i++ {
				if doc[i] == '\n' {
					doc = doc[:i]
					break
				}
			}
			if len(doc) > 160 {
				doc = doc[:157] + "..."
			}
			entry["summary"] = doc
		}
		hits = append(hits, entry)
	}
	out := map[string]any{"matches": hits, "count": len(hits), "branch": res.Branch}
	if res.Fallback {
		out["note"] = res.Note
	}
	if !res.Semantic {
		out["note"] = "semantic embeddings not configured — matched by name/text only; deploy with embeddings for full role-based recall"
	}
	return out
}

// findFTS extracts identifier-shaped tokens from a natural-language query
// and searches progressively: all terms ANDed, then the single strongest
// (longest) term — mirroring the prompt-hook pre-fetch strategy.
func findFTS(ctx context.Context, svc *codeindex.Service, ws, branch, q, pathPrefix string, limit int) (*codeindex.SearchResults, error) {
	stop := map[string]bool{"the": true, "a": true, "an": true, "of": true, "to": true, "in": true, "for": true, "with": true, "and": true, "or": true, "is": true, "are": true, "was": true, "were": true, "which": true, "what": true, "where": true, "how": true, "does": true, "do": true, "use": true, "uses": true, "used": true, "using": true, "this": true, "that": true, "from": true, "on": true, "at": true, "by": true, "my": true, "our": true, "i": true, "we": true, "it": true, "its": true, "be": true, "been": true, "has": true, "have": true, "had": true, "will": true, "would": true, "should": true, "could": true, "can": true, "when": true, "not": true, "no": true, "yes": true, "value": true, "returns": true, "return": true, "function": true, "func": true, "class": true, "method": true, "code": true, "find": true, "me": true, "show": true}
	var terms []string
	for _, t := range strings.Fields(strings.ToLower(q)) {
		t = strings.Trim(t, ".,;:!?()\"'")
		if len(t) > 1 && !stop[t] {
			terms = append(terms, t)
		}
	}
	if len(terms) == 0 {
		return svc.Search(ctx, ws, branch, q, pathPrefix, limit)
	}
	if res, err := svc.Search(ctx, ws, branch, strings.Join(terms, " "), pathPrefix, limit); err == nil && len(res.Hits) > 0 {
		return res, nil
	}
	// longest term = most distinctive
	best := terms[0]
	for _, t := range terms {
		if len(t) > len(best) {
			best = t
		}
	}
	return svc.Search(ctx, ws, branch, best, pathPrefix, limit)
}
