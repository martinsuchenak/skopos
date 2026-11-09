package auth

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Key capabilities (tmp/agent-mcp-and-indexing — design.md §1) limit what a
// scoped key may do beyond its workspace scope. A key without capabilities —
// every key minted before them, and the root key — keeps full access. A
// capped key may use only the surfaces and areas it lists:
//
//   - surfaces: "rest" (every /api route, the dashboard's SSE, metrics) and
//     "mcp" (/mcp);
//   - areas, "<area>:read" or "<area>:write" (write implies read).
//
// Capabilities sit beside the other rules: workspace scope and the approver
// gate still apply to a capped key.

// Surfaces.
const (
	SurfaceREST = "rest"
	SurfaceMCP  = "mcp"
)

// Areas a capability can name.
const (
	AreaCode       = "code"
	AreaBlackboard = "blackboard"
	AreaPlans      = "plans"
	AreaInbox      = "inbox"
	AreaStatus     = "status"
	AreaRuns       = "runs"
	AreaWorkspaces = "workspaces"
	AreaEvents     = "events"
)

var knownAreas = map[string]bool{
	AreaCode: true, AreaBlackboard: true, AreaPlans: true, AreaInbox: true,
	AreaStatus: true, AreaRuns: true, AreaWorkspaces: true, AreaEvents: true,
}

// Presets are named capability sets for `skopos key create --preset`.
var Presets = map[string][]string{
	// agent: what a pipeline run's agent gets over MCP — the code index,
	// the blackboard (write), plans (to mark its steps), and the status and
	// inbox reads skopos_context returns. No REST: the agent never reaches
	// item transitions, runs or keys.
	"agent": {SurfaceMCP, AreaCode + ":read", AreaBlackboard + ":write", AreaPlans + ":write", AreaStatus + ":read", AreaInbox + ":read"},
	// integration: another system creating and following items over REST —
	// the inbox (create, start, the state endpoint, decisions), and reads of
	// the plans and runs behind them. Starting items and recording
	// decisions also need the approver permission, and naming the person
	// the delegate permission (both separate flags).
	"integration": {SurfaceREST, AreaInbox + ":write", AreaPlans + ":read", AreaRuns + ":read"},
}

// ErrInvalidCapability rejects an unknown capability name.
var ErrInvalidCapability = errors.New("invalid capability")

// ParseCapabilities validates and normalises a capability list: trimmed,
// lower-cased, deduplicated, sorted, and a ":write" makes the same area's
// ":read" redundant (dropped). An empty or nil list means "no capabilities"
// (full access) and returns nil.
func ParseCapabilities(in []string) ([]string, error) {
	seen := map[string]bool{}
	for _, raw := range in {
		c := strings.ToLower(strings.TrimSpace(raw))
		if c == "" {
			continue
		}
		switch c {
		case SurfaceREST, SurfaceMCP:
		default:
			area, mode, ok := strings.Cut(c, ":")
			if !ok || !knownAreas[area] || (mode != "read" && mode != "write") {
				return nil, fmt.Errorf("%w: %q (surfaces: rest, mcp; areas: %s, each :read or :write)", ErrInvalidCapability, raw, strings.Join(areaNames(), ", "))
			}
		}
		seen[c] = true
	}
	if len(seen) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		if area, mode, ok := strings.Cut(c, ":"); ok && mode == "read" && seen[area+":write"] {
			continue
		}
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

func areaNames() []string {
	out := make([]string, 0, len(knownAreas))
	for a := range knownAreas {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Capped reports whether the principal is limited by capabilities. Root,
// internal callers (nil) and keys without capabilities are not.
func (p *Principal) Capped() bool {
	return p != nil && !p.Root && p.Capabilities != nil
}

// AllowsSurface reports whether the principal may use surface (rest, mcp).
func (p *Principal) AllowsSurface(surface string) bool {
	if !p.Capped() {
		return true
	}
	_, ok := p.Capabilities[surface]
	return ok
}

// Allows reports whether the principal may read (write=false) or write an
// area. Write implies read.
func (p *Principal) Allows(area string, write bool) bool {
	if !p.Capped() {
		return true
	}
	if _, ok := p.Capabilities[area+":write"]; ok {
		return true
	}
	if write {
		return false
	}
	_, ok := p.Capabilities[area+":read"]
	return ok
}

// CapabilityList returns the principal's capabilities sorted (nil when not
// capped).
func (p *Principal) CapabilityList() []string {
	if !p.Capped() {
		return nil
	}
	out := make([]string, 0, len(p.Capabilities))
	for c := range p.Capabilities {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// restAreas maps an /api path prefix to its area. Longest prefix wins, so a
// sub-resource can name a different area than its parent.
var restAreas = []struct{ prefix, area string }{
	{"/api/blackboard", AreaBlackboard},
	{"/api/codeindex", AreaCode},
	{"/api/plans", AreaPlans},
	{"/api/inbox", AreaInbox},
	{"/api/sessions", AreaStatus},
	{"/api/reports", AreaStatus},
	{"/api/runs", AreaRuns},
	{"/api/workers", AreaRuns},
	{"/api/workspaces", AreaWorkspaces},
	{"/api/events", AreaEvents},
}

// alwaysAllowed paths answer any authenticated principal, whatever its
// capabilities: whoami is how a client (the worker checking its agent key)
// learns what its key may do.
var alwaysAllowed = map[string]bool{"/api/whoami": true}

// RouteCapability names what a request to path with method needs from a
// capped principal: the surface, and the area and mode (area "" = none).
// ok=false means the route is refused to every capped key (key and group
// management, metrics, anything not mapped).
func RouteCapability(method, path string) (surface, area string, write, ok bool) {
	if alwaysAllowed[path] {
		return "", "", false, true
	}
	if path == "/mcp" || strings.HasPrefix(path, "/mcp/") {
		return SurfaceMCP, "", false, true
	}
	write = method != http.MethodGet && method != http.MethodHead
	best := ""
	for _, ra := range restAreas {
		if (path == ra.prefix || strings.HasPrefix(path, ra.prefix+"/")) && len(ra.prefix) > len(best) {
			best, area = ra.prefix, ra.area
		}
	}
	if best == "" {
		return SurfaceREST, "", write, false
	}
	return SurfaceREST, area, write, true
}

// AllowsRequest reports whether the principal may make this request, and
// why not.
func (p *Principal) AllowsRequest(method, path string) (bool, string) {
	if !p.Capped() || method == http.MethodOptions {
		return true, ""
	}
	surface, area, write, ok := RouteCapability(method, path)
	if !ok {
		return false, "this key's capabilities do not cover " + path
	}
	if surface != "" && !p.AllowsSurface(surface) {
		return false, "this key may not use the " + surface + " surface"
	}
	if area != "" && !p.Allows(area, write) {
		mode := "read"
		if write {
			mode = "write"
		}
		return false, "this key lacks " + area + ":" + mode
	}
	return true, ""
}
