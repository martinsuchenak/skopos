package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/martinsuchenak/skopos/internal/ctxkeys"
)

// Principal is the resolved identity behind a request. Root is the
// configured root key (or authentication disabled); everything else is a
// DB-backed API key with a workspace scope. A nil *Principal means an
// internal/background caller and is treated as root by CanAccess.
type Principal struct {
	Root          bool
	KeyID, Name   string
	AllWorkspaces bool                 // "*" scope
	Workspaces    map[string]struct{}  // explicit scope
}

// CanAccess reports whether the principal may read/write workspace ws.
func (p *Principal) CanAccess(ws string) bool {
	if p == nil || p.Root || p.AllWorkspaces {
		return true
	}
	_, ok := p.Workspaces[ws]
	return ok
}

// IsRoot reports whether the principal holds unrestricted privileges
// (key/workspace management, codeindex refresh).
func (p *Principal) IsRoot() bool { return p == nil || p.Root }

// WorkspaceList returns the explicit workspace scope (nil when root or "*").
func (p *Principal) WorkspaceList() []string {
	if p == nil || p.Root || p.AllWorkspaces {
		return nil
	}
	out := make([]string, 0, len(p.Workspaces))
	for w := range p.Workspaces {
		out = append(out, w)
	}
	return out
}

// KeyInfo is the storage-level view of an API key, without secrets.
type KeyInfo struct {
	ID, Name      string
	AllWorkspaces bool
	Workspaces    []string
}

// KeyLookup resolves a key hash to its scope; implemented by the apikeys
// storage. Returning (nil, nil) means "unknown key".
type KeyLookup interface {
	LookupKey(ctx context.Context, keyHash string) (*KeyInfo, error)
}

// KeyToucher is optionally implemented by lookups to record key usage.
// Touches are advisory (best-effort, throttled by the caller).
type KeyToucher interface {
	TouchKey(ctx context.Context, keyID string)
}

// Authenticator resolves requests to Principals. rootKey is the configured
// root credential (empty = authentication disabled, loopback dev mode).
type Authenticator struct {
	rootKey string
	lookup  KeyLookup
	touched map[string]time.Time
	touchMu sync.Mutex
}

func NewAuthenticator(rootKey string, lookup KeyLookup) *Authenticator {
	return &Authenticator{rootKey: rootKey, lookup: lookup, touched: map[string]time.Time{}}
}

// touchLastUsed records key usage at most once per 5 minutes per key — a
// write per request would itself contend on the SQLite write lock.
func (a *Authenticator) touchLastUsed(keyID string) {
	a.touchMu.Lock()
	if time.Since(a.touched[keyID]) < 5*time.Minute {
		a.touchMu.Unlock()
		return
	}
	a.touched[keyID] = time.Now()
	a.touchMu.Unlock()
	if toucher, ok := a.lookup.(KeyToucher); ok {
		toucher.TouchKey(context.Background(), keyID)
	}
}

// Authenticate resolves the request's bearer credential. It returns nil when
// the request is unauthenticated (callers reject with 401).
func (a *Authenticator) Authenticate(r *http.Request) *Principal {
	if a.rootKey == "" {
		return &Principal{Root: true}
	}
	cred := BearerToken(r)
	if cred == "" {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(cred), []byte(a.rootKey)) == 1 {
		return &Principal{Root: true}
	}
	if a.lookup == nil {
		return nil
	}
	// Hash lookup, not a scan: the digest is constant regardless of the
	// stored keys, so there is no per-key timing channel.
	info, err := a.lookup.LookupKey(r.Context(), HashKey(cred))
	if err != nil || info == nil {
		return nil
	}
	a.touchLastUsed(info.ID)
	p := &Principal{
		KeyID:         info.ID,
		Name:          info.Name,
		AllWorkspaces: info.AllWorkspaces,
		Workspaces:    make(map[string]struct{}, len(info.Workspaces)),
	}
	for _, w := range info.Workspaces {
		p.Workspaces[w] = struct{}{}
	}
	return p
}

// BearerToken extracts the Authorization: Bearer credential ("" when absent).
func BearerToken(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
}

// HashKey is the stored form of an API key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Middleware authenticates every request, injects the Principal into the
// request context, and rejects unauthenticated requests with 401. CORS
// preflight (OPTIONS) requests always pass, matching APIKeyMiddleware.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions {
			p := a.Authenticate(r)
			if p == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			r = r.WithContext(WithPrincipal(r.Context(), p))
		}
		next.ServeHTTP(w, r)
	})
}

// WithPrincipal attaches p to ctx (system privileges when p is nil).
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxkeys.PrincipalKey, p)
}

// PrincipalFromContext returns the request's principal, or nil for
// internal/background callers (registrar, health ticker, refresher) —
// nil principals pass CanAccess by design.
func PrincipalFromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxkeys.PrincipalKey).(*Principal)
	return p
}

// Authorization sentinels shared by every domain service. Handlers map
// ErrOutOfScope to 403 (explicit-workspace operations) or 404 (by-id
// operations, avoiding a cross-tenant existence oracle); MCP maps both to
// invalid-params.
var (
	ErrOutOfScope   = errors.New("workspace out of scope for this credential")
	ErrRootRequired = errors.New("this operation requires the root key")
)

// RequireWorkspace enforces the caller's scope for an explicit workspace.
// A nil principal (internal/background caller) passes.
func RequireWorkspace(ctx context.Context, ws string) error {
	p := PrincipalFromContext(ctx)
	if p.CanAccess(ws) {
		return nil
	}
	list := p.WorkspaceList()
	if len(list) == 0 {
		return fmt.Errorf("%w: %q (this key has no workspace scope)", ErrOutOfScope, ws)
	}
	return fmt.Errorf("%w: %q (accessible: %s)", ErrOutOfScope, ws, strings.Join(list, ", "))
}

// RequireRoot enforces root-only operations (key/workspace management,
// server-side refresh).
func RequireRoot(ctx context.Context) error {
	if PrincipalFromContext(ctx).IsRoot() {
		return nil
	}
	return ErrRootRequired
}

// ScopedContext reports whether the caller is a workspace-scoped key (not
// root, not internal): read paths then enumerate the scope instead of the
// whole store.
func ScopedContext(ctx context.Context) bool {
	p := PrincipalFromContext(ctx)
	return p != nil && !p.Root && !p.AllWorkspaces
}
