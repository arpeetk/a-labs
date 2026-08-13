package coreapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
	"github.com/summiteight/wren/internal/store"
)

const (
	outboxLease       = 30 * time.Second
	maxLaunchAttempts = 12
)

// RunOutboxWorker continuously drains durable external effects until ctx is
// canceled. It dispatches immediately at startup, which is what recovers work
// left behind by a previous apiserver process before accepting new traffic.
func (s *Service) RunOutboxWorker(ctx context.Context, worker string, interval time.Duration, report func(error)) {
	if interval <= 0 {
		interval = time.Second
	}
	if report == nil {
		report = func(error) {}
	}
	drain := func() {
		for {
			n, err := s.DispatchPending(ctx, worker, 32)
			if err != nil && !errors.Is(err, context.Canceled) {
				report(err)
			}
			if n < 32 || ctx.Err() != nil {
				return
			}
		}
	}
	drain()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			drain()
		}
	}
}

// DispatchPending claims and executes up to limit durable effects. Claiming is
// safe across replicas (Postgres uses SKIP LOCKED + a lease); handlers are
// idempotent, so a worker crash after the external effect but before ack is
// recovered by replay rather than duplication.
func (s *Service) DispatchPending(ctx context.Context, worker string, limit int) (int, error) {
	ops, err := store.ClaimOperations(ctx, s.store, worker, limit, outboxLease)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, op := range ops {
		if err := s.dispatchOperation(ctx, worker, op); err != nil {
			errs = append(errs, fmt.Errorf("operation %s: %w", op.ID, err))
		}
	}
	return len(ops), errors.Join(errs...)
}

func (s *Service) dispatchOperation(ctx context.Context, worker string, op *store.Operation) error {
	if op.Kind != store.OperationLaunchRun {
		return s.failOperation(ctx, worker, op, fmt.Errorf("unknown operation kind %q", op.Kind))
	}
	var run wrenv1.AgentRun
	if err := json.Unmarshal(op.Payload, &run); err != nil {
		return s.failOperation(ctx, worker, op, fmt.Errorf("decode launch payload: %w", err))
	}
	if err := s.launcher.EnsureNamespace(ctx, run.Namespace); err != nil {
		return s.retryOrFail(ctx, worker, op, fmt.Errorf("ensure namespace: %w", err))
	}
	if err := s.launcher.CreateRun(ctx, &run); err != nil {
		return s.retryOrFail(ctx, worker, op, fmt.Errorf("ensure AgentRun: %w", err))
	}
	event := &store.RunEvent{
		RunID: op.RunID, Source: "outbox", SourceID: op.ID, Type: "run.launch_accepted",
		Payload: mustJSON(map[string]any{"attempts": op.Attempts}), CreatedAt: s.now().UTC(),
	}
	return store.CompleteOperation(ctx, s.store, worker, op.ID, event)
}

func (s *Service) retryOrFail(ctx context.Context, worker string, op *store.Operation, cause error) error {
	if permanentLaunchError(cause) || op.Attempts >= maxLaunchAttempts {
		return s.failOperation(ctx, worker, op, cause)
	}
	delay := time.Duration(math.Pow(2, float64(min(op.Attempts-1, 6)))) * time.Second
	if err := store.RetryOperation(ctx, s.store, worker, op.ID, cause.Error(), s.now().Add(delay)); err != nil {
		return fmt.Errorf("schedule retry after %v: %w", cause, err)
	}
	return cause
}

func (s *Service) failOperation(ctx context.Context, worker string, op *store.Operation, cause error) error {
	run, err := s.store.GetRun(ctx, op.RunID)
	if err != nil {
		return fmt.Errorf("load run for terminal operation failure: %w", err)
	}
	run.Phase = string(wrenv1.PhaseFailed)
	run.Conditions = append(run.Conditions, store.RunCondition{
		Type: "LaunchReady", Status: "False", Reason: "LaunchFailed",
		Message: cause.Error(), LastTransitionTime: s.now().UTC(),
	})
	event := &store.RunEvent{
		RunID: op.RunID, Source: "outbox", SourceID: op.ID + "/failed", Type: "run.launch_failed",
		Payload: mustJSON(map[string]any{"error": cause.Error(), "attempts": op.Attempts}), CreatedAt: s.now().UTC(),
	}
	if err := store.FailOperation(ctx, s.store, worker, op.ID, cause.Error(), run, event); err != nil {
		return fmt.Errorf("persist terminal launch failure: %w", err)
	}
	return cause
}

func permanentLaunchError(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsUnauthorized(err)
}
