package blackboard

import "testing"

func TestModelConstants(t *testing.T) {
	_ = ScopeSession
	_ = ScopeBranch
	_ = ScopeProject
	_ = TypeFinding
	_ = TypeDecision
	_ = TypeBug
	_ = TypeDebt
	_ = TypeWarning
	_ = TypeContext
	_ = ErrInvalidInput
	_ = ErrNotFound
	_ = ErrAlreadyAtTopScope
	var _ Entry
	var _ WriteInput
	var _ WriteResult
	var _ Bundle
}
