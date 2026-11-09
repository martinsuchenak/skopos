package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/ctxkeys"
)

// Principal is the resolved identity behind a request. Root is the
// configured root key (or authentication disabled); everything else is a
// DB-backed API key with a workspace scope. A nil *Principal means an
// internal/background caller and is treated as root by CanAccess.
type Principal struct {
	Root          bool
	KeyID, Name   string
	AllWorkspaces bool                // "*" scope
	Workspaces    map[string]struct{} // explicit scope
	Approver      bool                // may perform human-only workflow actions (agent-pipeline §2)
	// Delegate: the key may name the person it acts for
	// (X-Skopos-On-Behalf-Of).
	Delegate bool
	// OnBehalfOf is the person a delegate key (or root) named for this
	// request; the audit log and approvals record it beside the key.
	OnBehalfOf string
	// Capabilities limit the key's surfaces and areas (capabilities.go);
	// nil = not capped (full access).
	Capabilities map[string]struct{}
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
	Approver      bool
	Delegate      bool
	Workspaces    []string
	Capabilities  []string // nil = full access
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
		Approver:      info.Approver,
		Delegate:      info.Delegate,
		Workspaces:    make(map[string]struct{}, len(info.Workspaces)),
	}
	for _, w := range info.Workspaces {
		p.Workspaces[w] = struct{}{}
	}
	if info.Capabilities != nil {
		p.Capabilities = make(map[string]struct{}, len(info.Capabilities))
		for _, c := range info.Capabilities {
			p.Capabilities[c] = struct{}{}
		}
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
// request context, rejects unauthenticated requests with 401, and a capped
// key's requests outside its capabilities (surface or area) with 403. CORS
// preflight (OPTIONS) requests always pass, matching APIKeyMiddleware.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions {
			p := a.Authenticate(r)
			if p == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			if ok, why := p.AllowsRequest(r.Method, r.URL.Path); !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": why})
				return
			}
			if v, present := r.Header[OnBehalfOfHeader]; present {
				person, err := onBehalfOf(p, strings.Join(v, ","))
				if err != nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
					return
				}
				p.OnBehalfOf = person
			}
			r = r.WithContext(WithPrincipal(r.Context(), p))
		}
		next.ServeHTTP(w, r)
	})
}

// OnBehalfOfHeader names the person a delegate key acts for.
const OnBehalfOfHeader = "X-Skopos-On-Behalf-Of"

// maxOnBehalfOf bounds the named person (an email or a username).
const maxOnBehalfOf = 200

// onBehalfOf validates the header: only root or a delegate key may name a
// person, and the name is 1-200 printable characters. A key without the
// permission is refused rather than ignored, so a misconfigured
// integration never records decisions as the key alone without knowing.
func onBehalfOf(p *Principal, v string) (string, error) {
	if !p.Root && !p.Delegate {
		return "", fmt.Errorf("%s needs a key with the delegate permission", OnBehalfOfHeader)
	}
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxOnBehalfOf || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("%s must be 1 to %d printable characters", OnBehalfOfHeader, maxOnBehalfOf)
	}
	return v, nil
}

// Actor renders the caller the way the audit log, approvals and answers
// record it — system, root, key <name>, or "<person> (key <name>)" when a
// delegate key named the person — with the key id ("" for root and
// internal callers).
func Actor(ctx context.Context) (actor, keyID string) {
	p := PrincipalFromContext(ctx)
	switch {
	case p == nil:
		return "system", ""
	case p.Root:
		if p.OnBehalfOf != "" {
			return p.OnBehalfOf + " (root)", ""
		}
		return "root", ""
	case p.OnBehalfOf != "":
		return p.OnBehalfOf + " (key " + p.Name + ")", p.KeyID
	default:
		return "key " + p.Name, p.KeyID
	}
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
	ErrOutOfScope       = errors.New("workspace out of scope for this credential")
	ErrRootRequired     = errors.New("this operation requires the root key")
	ErrApproverRequired = errors.New("this operation requires the approver permission")
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

// RequireWorkspaceQuiet is RequireWorkspace for by-id operations: the error
// carries no workspace detail, so a foreign object is indistinguishable from
// a nonexistent one (uniform 404 — no existence or ownership oracle).
func RequireWorkspaceQuiet(ctx context.Context, ws string) error {
	p := PrincipalFromContext(ctx)
	if p.CanAccess(ws) {
		return nil
	}
	return ErrOutOfScope
}

// RequireRoot enforces root-only operations (key/workspace management,
// server-side refresh).
func RequireRoot(ctx context.Context) error {
	if PrincipalFromContext(ctx).IsRoot() {
		return nil
	}
	return ErrRootRequired
}

// RequireApprover enforces human-only workflow actions (inbox_queue,
// inbox_approve, inbox_request_changes, inbox_reject, inbox_mark_done —
// docs/design/agent-pipeline.md §2). Root passes implicitly; internal
// callers (nil principal) pass like RequireRoot; every other key needs the
// approver permission, which agent and worker keys never hold.
func RequireApprover(ctx context.Context) error {
	p := PrincipalFromContext(ctx)
	if p == nil || p.Root || p.Approver {
		return nil
	}
	return ErrApproverRequired
}

// ScopedContext reports whether the caller is a workspace-scoped key (not
// root, not internal): read paths then enumerate the scope instead of the
// whole store.
func ScopedContext(ctx context.Context) bool {
	p := PrincipalFromContext(ctx)
	return p != nil && !p.Root && !p.AllWorkspaces
}
