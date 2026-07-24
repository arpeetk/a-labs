package store

import (
	"context"
	"testing"
	"time"
)

// fallbackStore is a Store that deliberately does NOT implement the upserter
// seam, forcing UpsertRun down its create-then-update fallback path. It forwards
// each Store method to an inner Memory explicitly (no embedding) so Memory's
// UpsertRun is not promoted.
type fallbackStore struct{ m *Memory }

func (f fallbackStore) CreateProject(ctx context.Context, p *Project) error {
	return f.m.CreateProject(ctx, p)
}
func (f fallbackStore) GetProject(ctx context.Context, n string) (*Project, error) {
	return f.m.GetProject(ctx, n)
}
func (f fallbackStore) ListProjects(ctx context.Context) ([]*Project, error) {
	return f.m.ListProjects(ctx)
}
func (f fallbackStore) CreateRun(ctx context.Context, r *Run) error { return f.m.CreateRun(ctx, r) }
func (f fallbackStore) GetRun(ctx context.Context, id string) (*Run, error) {
	return f.m.GetRun(ctx, id)
}
func (f fallbackStore) ListRuns(ctx context.Context, fl RunFilter) ([]*Run, error) {
	return f.m.ListRuns(ctx, fl)
}
func (f fallbackStore) UpdateRun(ctx context.Context, r *Run) error    { return f.m.UpdateRun(ctx, r) }
func (f fallbackStore) DeleteRun(ctx context.Context, id string) error { return f.m.DeleteRun(ctx, id) }

func TestUpsertRunFallback(t *testing.T) {
	ctx := context.Background()
	var s Store = fallbackStore{NewMemory()}
	if _, ok := s.(upserter); ok {
		t.Fatal("fallbackStore should not satisfy upserter; test premise broken")
	}

	r := &Run{ID: "r-fb", Project: "p", User: "u", Phase: "Pending", CreatedAt: time.Now().UTC()}
	// First upsert → CreateRun path.
	if err := UpsertRun(ctx, s, r); err != nil {
		t.Fatalf("fallback insert: %v", err)
	}
	// Second upsert of same ID → ErrExists → UpdateRun path.
	r.Phase = "Running"
	if err := UpsertRun(ctx, s, r); err != nil {
		t.Fatalf("fallback update: %v", err)
	}
	got, err := s.GetRun(ctx, "r-fb")
	if err != nil || got.Phase != "Running" {
		t.Fatalf("fallback upsert result: %+v %v", got, err)
	}
}

func TestMigrationVersion(t *testing.T) {
	if v, err := migrationVersion("0001_init.sql"); err != nil || v != 1 {
		t.Fatalf("migrationVersion(0001_init.sql) = %d, %v", v, err)
	}
	if v, err := migrationVersion("0042_add_thing.sql"); err != nil || v != 42 {
		t.Fatalf("migrationVersion(0042_...) = %d, %v", v, err)
	}
	if _, err := migrationVersion("bad_name.sql"); err == nil {
		t.Fatal("migrationVersion(bad_name.sql) should error on non-numeric prefix")
	}
}
