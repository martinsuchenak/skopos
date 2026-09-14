package apikeys

import "time"

// Key is the API view of a stored key. The plaintext secret exists only in
// the CreateResult returned once at creation; storage keeps a hash.
type Key struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Prefix        string    `json:"key_prefix"`
	AllWorkspaces bool      `json:"all_workspaces"`
	Workspaces    []string  `json:"workspaces"`
	CreatedAt     time.Time `json:"created_at"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

// CreateResult carries the generated secret exactly once.
type CreateResult struct {
	Key    Key    `json:"key"`
	Secret string `json:"key_secret"`
}
