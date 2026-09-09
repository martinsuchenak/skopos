// Package ids generates time-sortable entity identifiers. All skopos rows use
// UUIDv7 (text) so creation order is preserved by the ID alone.
package ids

import "github.com/google/uuid"

// New returns a UUIDv7 string, falling back to a random UUID if v7 generation
// fails (e.g. clock skew).
func New() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
