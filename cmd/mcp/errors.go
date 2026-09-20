package mcp

import (
	"errors"

	"github.com/martinsuchenak/skopos/internal/auth"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	mcplib "github.com/paularlott/mcp"
)

// toolError classifies service errors uniformly across all tools: client
// mistakes (invalid input, unknown id, cycle, conflicting state) map to
// invalid-params, everything else (storage/internal failures) to internal.
// Before this helper the mutation tools reported internal failures as
// invalid-params while the read tools did the opposite.
func toolError(err error) error {
	if errors.Is(err, status.ErrInvalidInput) || errors.Is(err, status.ErrNotFound) ||
		errors.Is(err, blackboard.ErrInvalidInput) || errors.Is(err, blackboard.ErrNotFound) ||
		errors.Is(err, blackboard.ErrAlreadyAtTopScope) ||
		errors.Is(err, plans.ErrInvalidInput) || errors.Is(err, plans.ErrNotFound) ||
		errors.Is(err, plans.ErrCycleDetected) || errors.Is(err, plans.ErrClaimConflict) ||
		errors.Is(err, inbox.ErrInvalidInput) || errors.Is(err, inbox.ErrNotFound) ||
		errors.Is(err, inbox.ErrClaimConflict) || errors.Is(err, inbox.ErrAlreadyConverted) ||
		errors.Is(err, inbox.ErrFrozen) ||
		errors.Is(err, auth.ErrOutOfScope) || errors.Is(err, auth.ErrRootRequired) {
		return mcplib.NewToolErrorInvalidParams(err.Error())
	}
	return mcplib.NewToolErrorInternal(err.Error())
}
