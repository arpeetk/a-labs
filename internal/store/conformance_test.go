package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// testStore is the Store conformance suite. Every implementation must pass it;
// the memory and postgres drivers both call it (see memory_test.go and
// postgres_test.go). The factory returns a fresh, empty store per subtest.
func testStore(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	t.Run("ProjectCRUD", func(t *testing.T) { testProjectCRUD(t, newStore(t)) })
	t.Run("RunCRUDAndFilter", func(t *testing.T) { testRunCRUDAndFilter(t, newStore(t)) })
	t.Run("UpsertRun", func(t *testing.T) { testUpsertRun(t, newStore(t)) })
}

func testProjectCRUD(t *testing.T, s Store) {
	ctx := context.Background()

	if _, err := s.GetProject(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProject missing = %v, want ErrNotFound", err)
	}
	p := &Project{
		Name:            "payments-api",
		Repo:            "corp/payments-api",
		EgressAllowlist: []string{"api.github.com", "registry.npmjs.org"},
		CreatedAt:       time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, p); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate CreateProject = %v, want ErrExists", err)
	}
	// Stored copy must be independent of the caller's pointer.
	p.Repo = "mutated"
	got, err := s.GetProject(ctx, "payments-api")
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != "corp/payments-api" {
		t.Errorf("store did not copy on write: %q", got.Repo)
	}
	if len(got.EgressAllowlist) != 2 || got.EgressAllowlist[0] != "api.github.com" {
		t.Errorf("egress allowlist round-trip = %v", got.EgressAllowlist)
	}
	list, err := s.ListProjects(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListProjects = %v, %v", list, err)
	}
}

func testRunCRUDAndFilter(t *testing.T, s Store) {
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Millisecond)
	runs := []*Run{
		{ID: "r-1", Project: "a", User: "alice@x", Phase: "Pending", CreatedAt: base},
		{ID: "r-2", Project: "b", User: "bob@x", Phase: "Running", CreatedAt: base.Add(time.Second)},
		{ID: "r-3", Project: "a", User: "alice@x", Phase: "Succeeded", CreatedAt: base.Add(2 * time.Second)},
	}
	for _, r := range runs {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateRun(ctx, runs[0]); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate run = %v, want ErrExists", err)
	}

	// Newest first.
	all, _ := s.ListRuns(ctx, RunFilter{})
	if len(all) != 3 || all[0].ID != "r-3" {
		t.Fatalf("ListRuns order = %+v", all)
	}
	// Filter by user.
	mine, _ := s.ListRuns(ctx, RunFilter{User: "alice@x"})
	if len(mine) != 2 {
		t.Fatalf("alice runs = %d, want 2", len(mine))
	}
	// Filter by project.
	projA, _ := s.ListRuns(ctx, RunFilter{Project: "a"})
	if len(projA) != 2 {
		t.Fatalf("project a runs = %d, want 2", len(projA))
	}
	// Combined filter.
	both, _ := s.ListRuns(ctx, RunFilter{User: "alice@x", Project: "a"})
	if len(both) != 2 {
		t.Fatalf("alice+a runs = %d, want 2", len(both))
	}

	// Update.
	r2, _ := s.GetRun(ctx, "r-2")
	r2.Phase = "Succeeded"
	r2.PRURL = "http://pr/2"
	if err := s.UpdateRun(ctx, r2); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRun(ctx, "r-2")
	if got.Phase != "Succeeded" || got.PRURL != "http://pr/2" {
		t.Errorf("update not persisted: %+v", got)
	}
	if err := s.UpdateRun(ctx, &Run{ID: "ghost"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing = %v, want ErrNotFound", err)
	}
	if _, err := s.GetRun(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v, want ErrNotFound", err)
	}
}

// testUpsertRun exercises the reconcile-on-boot seam through the free-function
// helper (which type-asserts to the upserter interface). Upsert of a new run
// inserts; of an existing run overwrites without ErrExists.
func testUpsertRun(t *testing.T, s Store) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)

	r := &Run{ID: "r-boot", Project: "p", User: "u", Phase: "Running", CreatedAt: base}
	if err := UpsertRun(ctx, s, r); err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	got, err := s.GetRun(ctx, "r-boot")
	if err != nil || got.Phase != "Running" {
		t.Fatalf("after insert-upsert: %+v %v", got, err)
	}

	r.Phase = "Succeeded"
	r.PRURL = "http://pr/boot"
	if err := UpsertRun(ctx, s, r); err != nil {
		t.Fatalf("upsert overwrite: %v", err)
	}
	got, _ = s.GetRun(ctx, "r-boot")
	if got.Phase != "Succeeded" || got.PRURL != "http://pr/boot" {
		t.Errorf("after overwrite-upsert: %+v", got)
	}

	// An upsert of an existing run must not surface ErrExists.
	if err := UpsertRun(ctx, s, r); errors.Is(err, ErrExists) {
		t.Errorf("upsert of existing run returned ErrExists")
	}
}
