package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubLookup map[string]*KeyInfo

func (s stubLookup) LookupKey(_ context.Context, hash string) (*KeyInfo, error) { return s[hash], nil }

// A delegate key names the person it acts for; the actor reads
// "<person> (key <name>)". A key without the permission is refused, never
// silently recorded as the key alone.
func TestOnBehalfOf(t *testing.T) {
	lookup := stubLookup{
		HashKey("sk_fortix"): {ID: "k1", Name: "fortix", Approver: true, Delegate: true, Workspaces: []string{"ws"}},
		HashKey("sk_plain"):  {ID: "k2", Name: "plain", Approver: true, Workspaces: []string{"ws"}},
	}
	a := NewAuthenticator("root-key", lookup)
	var actor, keyID, person string
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, keyID = Actor(r.Context())
		person = PrincipalFromContext(r.Context()).OnBehalfOf
	}))
	call := func(key, onBehalf string, set bool) int {
		actor, keyID, person = "", "", ""
		r := httptest.NewRequest(http.MethodGet, "/api/inbox", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		if set {
			r.Header.Set(OnBehalfOfHeader, onBehalf)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	if code := call("sk_fortix", "martin@fortix.example", true); code != http.StatusOK || actor != "martin@fortix.example (key fortix)" || keyID != "k1" || person != "martin@fortix.example" {
		t.Fatalf("delegate: %d %q %q %q", code, actor, keyID, person)
	}
	if code := call("sk_fortix", "", false); code != http.StatusOK || actor != "key fortix" || person != "" {
		t.Fatalf("delegate without header: %d %q", code, actor)
	}
	if code := call("root-key", "martin", true); code != http.StatusOK || actor != "martin (root)" {
		t.Fatalf("root: %d %q", code, actor)
	}
	if code := call("sk_plain", "martin", true); code != http.StatusBadRequest || actor != "" {
		t.Fatalf("non-delegate: %d %q", code, actor)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", 201), "a\nb"} {
		if code := call("sk_fortix", bad, true); code != http.StatusBadRequest {
			t.Errorf("header %q: %d", bad, code)
		}
	}
	if got, _ := Actor(context.Background()); got != "system" {
		t.Fatalf("internal caller: %q", got)
	}
}
