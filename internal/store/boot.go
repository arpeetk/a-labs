package store

import "context"

// upserter is the reconcile-on-boot seam: both Memory and Postgres can insert
// or overwrite a Run by ID without the ErrExists / ErrNotFound distinction that
// CreateRun / UpdateRun enforce. It is intentionally NOT part of the frozen
// Store interface (spec §5.2 / implementation-plan §WS-3) — it is only used at
// apiserver start to re-learn in-flight runs from the AgentRun CRs, which are
// the source of truth for status. Callers that need it type-assert.
type upserter interface {
	UpsertRun(ctx context.Context, r *Run) error
}

// UpsertRun inserts or overwrites the run in s by ID. It works for any Store
// that implements the reconcile-on-boot seam (Memory and Postgres both do). If
// a Store ever does not, it falls back to Create-then-Update so behavior stays
// correct (a store is free to not optimize the boot path).
func UpsertRun(ctx context.Context, s Store, r *Run) error {
	if u, ok := s.(upserter); ok {
		return u.UpsertRun(ctx, r)
	}
	// Fallback for third-party Stores: try create, fall back to update.
	err := s.CreateRun(ctx, r)
	if err == ErrExists {
		return s.UpdateRun(ctx, r)
	}
	return err
}
