package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeLookup struct {
	info *KeyInfo
	err  error
}

func (f *fakeLookup) LookupKey(_ context.Context, _ string) (*KeyInfo, error) {
	return f.info, f.err
}

func TestPrincipalCanAccess(t *testing.T) {
	root := &Principal{Root: true}
	all := &Principal{AllWorkspaces: true, Workspaces: map[string]struct{}{}}
	scoped := &Principal{Workspaces: map[string]struct{}{"github.com/o/a": {}}}

	for _, tc := range []struct {
		name string
		p    *Principal
		ws   string
		want bool
	}{
		{"nil principal is system/root", nil, "any", true},
		{"root accesses anything", root, "any", true},
		{"all-workspaces accesses anything", all, "any", true},
		{"scoped member", scoped, "github.com/o/a", true},
		{"scoped non-member", scoped, "github.com/o/b", false},
		{"scoped empty workspace", scoped, "", false},
	} {
		if got := tc.p.CanAccess(tc.ws); got != tc.want {
			t.Errorf("%s: CanAccess(%q) = %v, want %v", tc.name, tc.ws, got, tc.want)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	scopedKey := &fakeLookup{info: &KeyInfo{
		ID: "k1", Name: "ci", Workspaces: []string{"github.com/o/a"},
	}}
	req := func(key string) *http.Request {
		r := httptest.NewRequest("POST", "/x", nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		return r
	}

	t.Run("disabled auth is root", func(t *testing.T) {
		p := NewAuthenticator("", nil).Authenticate(req(""))
		if p == nil || !p.Root {
			t.Fatal("empty root key must authenticate as root")
		}
	})
	t.Run("root key", func(t *testing.T) {
		p := NewAuthenticator("rootsecret", nil).Authenticate(req("rootsecret"))
		if p == nil || !p.Root {
			t.Fatal("root key must authenticate as root")
		}
	})
	t.Run("wrong root key, no lookup", func(t *testing.T) {
		if p := NewAuthenticator("rootsecret", nil).Authenticate(req("nope")); p != nil {
			t.Fatal("expected nil principal")
		}
	})
	t.Run("db key resolves scope", func(t *testing.T) {
		p := NewAuthenticator("rootsecret", scopedKey).Authenticate(req("sk_dbkey"))
		if p == nil || p.Root || p.KeyID != "k1" || p.Name != "ci" {
			t.Fatalf("unexpected principal: %+v", p)
		}
		if p.CanAccess("github.com/o/a") || !p.CanAccess("github.com/o/a") == false {
		}
		if !p.CanAccess("github.com/o/a") {
			t.Fatal("member workspace must be accessible")
		}
		if p.CanAccess("github.com/o/b") {
			t.Fatal("non-member workspace must not be accessible")
		}
	})
	t.Run("lookup error is unauthenticated", func(t *testing.T) {
		if p := NewAuthenticator("rootsecret", &fakeLookup{err: errors.New("boom")}).Authenticate(req("sk_x")); p != nil {
			t.Fatal("lookup error must not authenticate")
		}
	})
	t.Run("missing bearer", func(t *testing.T) {
		if p := NewAuthenticator("rootsecret", scopedKey).Authenticate(req("")); p != nil {
			t.Fatal("missing bearer must not authenticate")
		}
	})
}

func TestAuthenticatorMiddlewareInjectsPrincipal(t *testing.T) {
	var got *Principal
	h := NewAuthenticator("rootsecret", nil).Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/api/x", nil)
	r.Header.Set("Authorization", "Bearer rootsecret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || got == nil || !got.Root {
		t.Fatalf("expected 200 with root principal, got %d %+v", w.Code, got)
	}

	r2 := httptest.NewRequest("GET", "/api/x", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Code != 401 {
		t.Fatalf("expected 401 without credentials, got %d", w2.Code)
	}
}
