package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestParseCapabilities(t *testing.T) {
	got, err := ParseCapabilities([]string{" MCP ", "code:read", "plans:read", "plans:write", "code:read", ""})
	if err != nil {
		t.Fatal(err)
	}
	// Trimmed, lower-cased, deduplicated, sorted; plans:read is implied by
	// plans:write and dropped.
	if want := []string{"code:read", "mcp", "plans:write"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, err := ParseCapabilities(nil); err != nil || got != nil {
		t.Fatalf("no capabilities = full access (nil): %v, %v", got, err)
	}
	for _, bad := range []string{"admin", "code", "code:delete", "keys:read", "rest:read"} {
		if _, err := ParseCapabilities([]string{bad}); !errors.Is(err, ErrInvalidCapability) {
			t.Errorf("%q: want ErrInvalidCapability, got %v", bad, err)
		}
	}
	for name, set := range Presets {
		if _, err := ParseCapabilities(set); err != nil {
			t.Errorf("preset %s: %v", name, err)
		}
	}
}

func capped(caps ...string) *Principal {
	p := &Principal{KeyID: "k", Workspaces: map[string]struct{}{"ws": {}}, Capabilities: map[string]struct{}{}}
	for _, c := range caps {
		p.Capabilities[c] = struct{}{}
	}
	return p
}

func TestAllowsRequest(t *testing.T) {
	agent := capped(Presets["agent"]...)
	worker := capped("rest", "runs:write", "code:read")
	for _, tc := range []struct {
		name         string
		p            *Principal
		method, path string
		want         bool
	}{
		{"uncapped key reaches everything", &Principal{KeyID: "k"}, http.MethodPost, "/api/keys", true},
		{"root reaches everything", &Principal{Root: true, Capabilities: map[string]struct{}{}}, http.MethodGet, "/metrics", true},
		{"agent: mcp", agent, http.MethodPost, "/mcp", true},
		{"agent: no REST, even for an area it holds", agent, http.MethodGet, "/api/blackboard", false},
		{"agent: no SSE", agent, http.MethodGet, "/api/events/stream", false},
		{"agent: whoami always", agent, http.MethodGet, "/api/whoami", true},
		{"worker: no mcp", worker, http.MethodPost, "/mcp", false},
		{"worker: claim (runs:write)", worker, http.MethodPost, "/api/runs/claim", true},
		{"worker: workers list (runs read)", worker, http.MethodGet, "/api/workers", true},
		{"worker: code read", worker, http.MethodGet, "/api/codeindex/search", true},
		{"worker: no code write (index push)", worker, http.MethodPost, "/api/codeindex/push", false},
		{"worker: no inbox", worker, http.MethodPost, "/api/inbox/x/transition", false},
		{"capped: key management refused", capped("rest", "workspaces:write"), http.MethodGet, "/api/keys", false},
		{"capped: groups refused", capped("rest", "workspaces:write"), http.MethodGet, "/api/groups", false},
		{"capped: metrics refused", capped("rest"), http.MethodGet, "/metrics", false},
		{"capped: unmapped route refused", capped("rest", "code:write"), http.MethodGet, "/api/something-new", false},
		{"capped: preflight passes", agent, http.MethodOptions, "/api/blackboard", true},
		{"pipeline settings are workspaces", capped("rest", "workspaces:read"), http.MethodGet, "/api/workspaces/ws/pipeline", true},
		{"pipeline edit needs workspaces:write", capped("rest", "workspaces:read"), http.MethodPut, "/api/workspaces/ws/pipeline", false},
	} {
		if got, why := tc.p.AllowsRequest(tc.method, tc.path); got != tc.want {
			t.Errorf("%s: %s %s = %v (%s), want %v", tc.name, tc.method, tc.path, got, why, tc.want)
		}
	}
}

type fixedLookup struct{ info *KeyInfo }

func (f fixedLookup) LookupKey(context.Context, string) (*KeyInfo, error) { return f.info, nil }

// The middleware refuses a capped key's request outside its capabilities
// with 403 and lets the rest through with the capabilities on the principal.
func TestMiddlewareEnforcesCapabilities(t *testing.T) {
	a := NewAuthenticator("root-secret", fixedLookup{&KeyInfo{ID: "k1", Name: "agent", Workspaces: []string{"ws"}, Capabilities: Presets["agent"]}})
	var seen *Principal
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	do := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer sk_agent")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := do(http.MethodGet, "/api/blackboard"); code != http.StatusForbidden {
		t.Fatalf("REST with an mcp-only key: %d, want 403", code)
	}
	if code := do(http.MethodPost, "/mcp"); code != http.StatusOK {
		t.Fatalf("/mcp: %d", code)
	}
	if !seen.Capped() || !seen.Allows(AreaCode, false) || seen.Allows(AreaCode, true) || !seen.Allows(AreaBlackboard, true) {
		t.Fatalf("principal capabilities: %v", seen.CapabilityList())
	}
}
