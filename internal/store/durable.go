package store

import (
	"context"
	"fmt"
	"time"
)

// CreateRunWithOperation atomically records a run, its first external effect,
// and its submission event when the store supports the durable extension. The
// fallback preserves compatibility but cannot provide crash recovery between
// the two writes, so production wiring rejects non-durable Postgres stores.
func CreateRunWithOperation(ctx context.Context, s Store, run *Run, op *Operation, event *RunEvent) error {
	if d, ok := s.(Durable); ok {
		return d.CreateRunWithOperation(ctx, run, op, event)
	}
	return s.CreateRun(ctx, run)
}

func ClaimOperations(ctx context.Context, s Store, worker string, limit int, lease time.Duration) ([]*Operation, error) {
	d, ok := s.(Durable)
	if !ok {
		return nil, nil
	}
	return d.ClaimOperations(ctx, worker, limit, lease)
}

func CompleteOperation(ctx context.Context, s Store, worker, id string, event *RunEvent) error {
	d, ok := s.(Durable)
	if !ok {
		return nil
	}
	return d.CompleteOperation(ctx, worker, id, event)
}

func RetryOperation(ctx context.Context, s Store, worker, id, lastError string, availableAt time.Time) error {
	d, ok := s.(Durable)
	if !ok {
		return fmt.Errorf("retry operation %s: store has no durable outbox", id)
	}
	return d.RetryOperation(ctx, worker, id, lastError, availableAt)
}

func FailOperation(ctx context.Context, s Store, worker, id, lastError string, run *Run, event *RunEvent) error {
	d, ok := s.(Durable)
	if !ok {
		return fmt.Errorf("fail operation %s: store has no durable outbox", id)
	}
	return d.FailOperation(ctx, worker, id, lastError, run, event)
}

func AppendRunEvent(ctx context.Context, s Store, event *RunEvent) (bool, error) {
	d, ok := s.(Durable)
	if !ok {
		return false, nil
	}
	return d.AppendRunEvent(ctx, event)
}

func ListRunEvents(ctx context.Context, s Store, runID string, afterID int64, limit int) ([]*RunEvent, error) {
	d, ok := s.(Durable)
	if !ok {
		return []*RunEvent{}, nil
	}
	return d.ListRunEvents(ctx, runID, afterID, limit)
}

func UpsertRunWithEvent(ctx context.Context, s Store, run *Run, event *RunEvent) error {
	if d, ok := s.(Durable); ok {
		return d.UpsertRunWithEvent(ctx, run, event)
	}
	return UpsertRun(ctx, s, run)
}
