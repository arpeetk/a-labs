package store

import (
	"context"
	"testing"
	"time"
)

// legacyStore deliberately exposes only the base contract to prove that the
// compatibility helpers remain safe for third-party stores during migration.
type legacyStore struct{ Store }

func TestDurableHelpersDelegateAndFallback(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	run := &Run{ID: "r-helper", Phase: "Pending", CreatedAt: now}
	op := &Operation{ID: "launch/r-helper", RunID: run.ID, Kind: OperationLaunchRun, AvailableAt: now, CreatedAt: now}
	event := &RunEvent{RunID: run.ID, Source: "test", SourceID: "submitted", Type: "run.submitted", CreatedAt: now}

	durable := NewMemory()
	if err := CreateRunWithOperation(ctx, durable, run, op, event); err != nil {
		t.Fatal(err)
	}
	claimed, err := ClaimOperations(ctx, durable, "worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if err := RetryOperation(ctx, durable, "worker", op.ID, "again", now); err != nil {
		t.Fatal(err)
	}
	claimed, err = ClaimOperations(ctx, durable, "worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 2 {
		t.Fatalf("retry claim = %+v, %v", claimed, err)
	}
	if err := CompleteOperation(ctx, durable, "worker", op.ID, nil); err != nil {
		t.Fatal(err)
	}
	if inserted, err := AppendRunEvent(ctx, durable, &RunEvent{RunID: run.ID, Source: "test", SourceID: "next", Type: "next"}); err != nil || !inserted {
		t.Fatalf("append inserted=%t err=%v", inserted, err)
	}
	if events, err := ListRunEvents(ctx, durable, run.ID, 0, 10); err != nil || len(events) != 2 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	run.Phase = "Running"
	if err := UpsertRunWithEvent(ctx, durable, run, nil); err != nil {
		t.Fatal(err)
	}

	legacy := legacyStore{Store: NewMemory()}
	legacyRun := &Run{ID: "r-legacy", Phase: "Pending", CreatedAt: now}
	if err := CreateRunWithOperation(ctx, legacy, legacyRun, op, event); err != nil {
		t.Fatal(err)
	}
	if operations, err := ClaimOperations(ctx, legacy, "worker", 1, time.Minute); err != nil || operations != nil {
		t.Fatalf("legacy claim = %+v, %v", operations, err)
	}
	if err := CompleteOperation(ctx, legacy, "worker", op.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := RetryOperation(ctx, legacy, "worker", op.ID, "again", now); err == nil {
		t.Fatal("legacy retry should reject absent outbox")
	}
	if err := FailOperation(ctx, legacy, "worker", op.ID, "failed", legacyRun, nil); err == nil {
		t.Fatal("legacy failure should reject absent outbox")
	}
	if inserted, err := AppendRunEvent(ctx, legacy, event); err != nil || inserted {
		t.Fatalf("legacy append inserted=%t err=%v", inserted, err)
	}
	if events, err := ListRunEvents(ctx, legacy, legacyRun.ID, 0, 10); err != nil || len(events) != 0 {
		t.Fatalf("legacy events=%+v err=%v", events, err)
	}
	legacyRun.Phase = "Succeeded"
	if err := UpsertRunWithEvent(ctx, legacy, legacyRun, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := legacy.GetRun(ctx, legacyRun.ID); err != nil || got.Phase != "Succeeded" {
		t.Fatalf("legacy upsert = %+v, %v", got, err)
	}
	if err := durable.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}
