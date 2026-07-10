package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestProjectCRUD(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	if _, err := m.GetProject(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProject missing = %v", err)
	}
	p := &Project{Name: "payments-api", Repo: "corp/payments-api"}
	if err := m.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateProject(ctx, p); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate CreateProject = %v, want ErrExists", err)
	}
	// Stored copy must be independent of the caller's pointer.
	p.Repo = "mutated"
	got, err := m.GetProject(ctx, "payments-api")
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != "corp/payments-api" {
		t.Errorf("store did not copy on write: %q", got.Repo)
	}
	list, err := m.ListProjects(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListProjects = %v, %v", list, err)
	}
}

func TestRunCRUDAndFilter(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	base := time.Now()
	runs := []*Run{
		{ID: "r-1", Project: "a", User: "alice@x", Phase: "Pending", CreatedAt: base},
		{ID: "r-2", Project: "b", User: "bob@x", Phase: "Running", CreatedAt: base.Add(time.Second)},
		{ID: "r-3", Project: "a", User: "alice@x", Phase: "Succeeded", CreatedAt: base.Add(2 * time.Second)},
	}
	for _, r := range runs {
		if err := m.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.CreateRun(ctx, runs[0]); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate run = %v", err)
	}

	// Newest first.
	all, _ := m.ListRuns(ctx, RunFilter{})
	if len(all) != 3 || all[0].ID != "r-3" {
		t.Fatalf("ListRuns order = %+v", all)
	}
	// Filter by user.
	mine, _ := m.ListRuns(ctx, RunFilter{User: "alice@x"})
	if len(mine) != 2 {
		t.Fatalf("alice runs = %d, want 2", len(mine))
	}
	// Filter by project.
	projA, _ := m.ListRuns(ctx, RunFilter{Project: "a"})
	if len(projA) != 2 {
		t.Fatalf("project a runs = %d, want 2", len(projA))
	}

	// Update.
	r2, _ := m.GetRun(ctx, "r-2")
	r2.Phase = "Succeeded"
	r2.PRURL = "http://pr/2"
	if err := m.UpdateRun(ctx, r2); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetRun(ctx, "r-2")
	if got.Phase != "Succeeded" || got.PRURL != "http://pr/2" {
		t.Errorf("update not persisted: %+v", got)
	}
	if err := m.UpdateRun(ctx, &Run{ID: "ghost"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}
	if _, err := m.GetRun(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v", err)
	}
}
