package ctxkeys

type contextKey string

const (
	UserIDKey    contextKey = "user_id"
	RequestIDKey contextKey = "request_id"
	APIKeyKey    contextKey = "api_key"
	// PrincipalKey carries the resolved auth.Principal behind a request.
	// Absent means an internal/background caller (registrar, health ticker,
	// refresher) — those run with system privileges.
	PrincipalKey contextKey = "auth_principal"
)
