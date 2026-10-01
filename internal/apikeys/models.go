package apikeys

import "time"

// Key is the API view of a stored key. The plaintext secret exists only in
// the CreateResult returned once at creation; storage keeps a hash.
type Key struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Prefix        string     `json:"key_prefix"`
	AllWorkspaces bool       `json:"all_workspaces"`
	Approver      bool       `json:"approver"` // may perform human-only workflow actions (agent-pipeline §2)
	Workspaces    []string   `json:"workspaces"`
	Groups        []string   `json:"groups"` // group names (unique) the key holds
	CreatedAt     time.Time  `json:"created_at"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

// CreateResult carries the generated secret exactly once.
type CreateResult struct {
	Key    Key    `json:"key"`
	Secret string `json:"key_secret"`
}

// Group is a named bundle of workspaces — explicit members plus
// auto-membership patterns — that keys can hold instead of listing
// workspaces one by one (docs/design/agent-pipeline.md §3). Names are unique;
// patterns follow Go path.Match (`*` never crosses `/`, there is no `**`).
type Group struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Members     []string  `json:"members"`
	Patterns    []string  `json:"patterns"`
	CreatedAt   time.Time `json:"created_at"`
}

// KeyReach explains how one key reaches one workspace (the who-can view).
// Via is "explicit", "group:<name>" or "pattern:<group>:<pattern>".
type KeyReach struct {
	Key Key    `json:"key"`
	Via string `json:"via"`
}

// ScopeResolution is the whoami detail behind a key's effective scope: the
// groups it holds and how each reachable workspace is reached.
type ScopeResolution struct {
	Groups []Group           `json:"groups"`
	Via    map[string]string `json:"via"`
}
