package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

var _ Durable = (*Postgres)(nil)

func (p *Postgres) CreateRunWithOperation(ctx context.Context, run *Run, op *Operation, event *RunEvent) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin durable run creation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := insertRunTx(ctx, tx, run); err != nil {
		return err
	}
	now := op.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	available := op.AvailableAt.UTC()
	if available.IsZero() {
		available = now
	}
	state := op.State
	if state == "" {
		state = OperationPending
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_operations
			(id, run_id, kind, payload, state, available_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)`,
		op.ID, op.RunID, op.Kind, []byte(op.Payload), state, available, now)
	if isUniqueViolation(err) {
		return ErrExists
	}
	if err != nil {
		return fmt.Errorf("insert outbox operation: %w", err)
	}
	if _, err := insertEventTx(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertRunTx(ctx context.Context, tx pgx.Tx, run *Run) error {
	checkpoint, conditions, err := marshalRunStatus(run)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO runs (
			id, project, "user", prompt, harness, model, base_ref,
			interactive, runtime, namespace, phase, pr_url, restart_count, last_checkpoint, conditions, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		run.ID, run.Project, run.User, run.Prompt, run.Harness, run.Model, run.BaseRef,
		run.Interactive, run.Runtime, run.Namespace, run.Phase, run.PRURL, run.RestartCount, checkpoint, conditions, run.CreatedAt)
	if isUniqueViolation(err) {
		return ErrExists
	}
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

const claimedOperationCols = `
	op.id, op.run_id, op.kind, op.payload, op.state, op.attempts, op.available_at,
	op.lease_owner, op.lease_until, op.last_error, op.created_at, op.updated_at`

func scanOperation(row pgx.Row) (*Operation, error) {
	var op Operation
	var leaseUntil *time.Time
	if err := row.Scan(&op.ID, &op.RunID, &op.Kind, &op.Payload, &op.State,
		&op.Attempts, &op.AvailableAt, &op.LeaseOwner, &leaseUntil,
		&op.LastError, &op.CreatedAt, &op.UpdatedAt); err != nil {
		return nil, err
	}
	op.AvailableAt = op.AvailableAt.UTC()
	op.CreatedAt = op.CreatedAt.UTC()
	op.UpdatedAt = op.UpdatedAt.UTC()
	if leaseUntil != nil {
		op.LeaseUntil = leaseUntil.UTC()
	}
	return &op, nil
}

func (p *Postgres) ClaimOperations(ctx context.Context, worker string, limit int, lease time.Duration) ([]*Operation, error) {
	if limit <= 0 {
		limit = 1
	}
	leaseSeconds := int64(lease / time.Second)
	if leaseSeconds < 1 {
		leaseSeconds = 1
	}
	rows, err := p.pool.Query(ctx, `
		WITH ready AS (
			SELECT id FROM outbox_operations
			WHERE (state = 'pending' AND available_at <= now())
			   OR (state = 'processing' AND lease_until <= now())
			ORDER BY created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE outbox_operations AS op SET
			state = 'processing', lease_owner = $2,
			lease_until = now() + ($3 * interval '1 second'),
			attempts = op.attempts + 1, updated_at = now()
		FROM ready WHERE op.id = ready.id
		RETURNING `+claimedOperationCols, limit, worker, leaseSeconds)
	if err != nil {
		return nil, fmt.Errorf("claim outbox operations: %w", err)
	}
	defer rows.Close()
	out := []*Operation{}
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan claimed operation: %w", err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read claimed operations: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (p *Postgres) CompleteOperation(ctx context.Context, worker, id string, event *RunEvent) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete operation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `
		UPDATE outbox_operations SET state='completed', lease_owner='',
			lease_until=NULL, last_error='', updated_at=now()
		WHERE id=$1 AND state='processing' AND lease_owner=$2`, id, worker)
	if err != nil {
		return fmt.Errorf("complete operation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	if _, err := insertEventTx(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) RetryOperation(ctx context.Context, worker, id, lastError string, availableAt time.Time) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE outbox_operations SET state='pending', available_at=$3,
			lease_owner='', lease_until=NULL, last_error=$4, updated_at=now()
		WHERE id=$1 AND state='processing' AND lease_owner=$2`,
		id, worker, availableAt.UTC(), lastError)
	if err != nil {
		return fmt.Errorf("retry operation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (p *Postgres) FailOperation(ctx context.Context, worker, id, lastError string, run *Run, event *RunEvent) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fail operation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := updateRunTx(ctx, tx, run); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE outbox_operations SET state='failed', lease_owner='',
			lease_until=NULL, last_error=$3, updated_at=now()
		WHERE id=$1 AND state='processing' AND lease_owner=$2`, id, worker, lastError)
	if err != nil {
		return fmt.Errorf("fail operation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	if _, err := insertEventTx(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) AppendRunEvent(ctx context.Context, event *RunEvent) (bool, error) {
	inserted, err := insertEvent(ctx, p.pool, event)
	if err != nil {
		return false, err
	}
	return inserted, nil
}

type eventQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func insertEvent(ctx context.Context, q eventQuerier, event *RunEvent) (bool, error) {
	if event == nil {
		return false, nil
	}
	created := event.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	err := q.QueryRow(ctx, `
		INSERT INTO run_events (run_id, source, source_id, event_type, payload, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (run_id, source, source_id) DO NOTHING
		RETURNING id, created_at`, event.RunID, event.Source, event.SourceID,
		event.Type, []byte(event.Payload), created).Scan(&event.ID, &event.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23503" {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("insert run event: %w", err)
	}
	event.CreatedAt = event.CreatedAt.UTC()
	return true, nil
}

func insertEventTx(ctx context.Context, tx pgx.Tx, event *RunEvent) (bool, error) {
	return insertEvent(ctx, tx, event)
}

func (p *Postgres) ListRunEvents(ctx context.Context, runID string, afterID int64, limit int) ([]*RunEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var exists bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1)`, runID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check run for events: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id, run_id, source, source_id, event_type, payload, created_at
		FROM run_events WHERE run_id=$1 AND id>$2 ORDER BY id LIMIT $3`, runID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list run events: %w", err)
	}
	defer rows.Close()
	out := []*RunEvent{}
	for rows.Next() {
		var event RunEvent
		if err := rows.Scan(&event.ID, &event.RunID, &event.Source, &event.SourceID,
			&event.Type, &event.Payload, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan run event: %w", err)
		}
		event.CreatedAt = event.CreatedAt.UTC()
		out = append(out, &event)
	}
	return out, rows.Err()
}

func (p *Postgres) UpsertRunWithEvent(ctx context.Context, run *Run, event *RunEvent) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin run projection: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := upsertRunTx(ctx, tx, run); err != nil {
		return err
	}
	if _, err := insertEventTx(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func upsertRunTx(ctx context.Context, tx pgx.Tx, run *Run) error {
	checkpoint, conditions, err := marshalRunStatus(run)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO runs (
			id, project, "user", prompt, harness, model, base_ref,
			interactive, runtime, namespace, phase, pr_url, restart_count, last_checkpoint, conditions, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (id) DO UPDATE SET
			project=EXCLUDED.project, "user"=EXCLUDED."user", prompt=EXCLUDED.prompt,
			harness=EXCLUDED.harness, model=EXCLUDED.model, base_ref=EXCLUDED.base_ref,
			interactive=EXCLUDED.interactive, runtime=EXCLUDED.runtime,
			namespace=EXCLUDED.namespace, phase=EXCLUDED.phase, pr_url=EXCLUDED.pr_url,
			restart_count=EXCLUDED.restart_count, last_checkpoint=EXCLUDED.last_checkpoint,
			conditions=EXCLUDED.conditions`,
		run.ID, run.Project, run.User, run.Prompt, run.Harness, run.Model, run.BaseRef,
		run.Interactive, run.Runtime, run.Namespace, run.Phase, run.PRURL,
		run.RestartCount, checkpoint, conditions, run.CreatedAt)
	if err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}
	return nil
}

func updateRunTx(ctx context.Context, tx pgx.Tx, run *Run) error {
	checkpoint, conditions, err := marshalRunStatus(run)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE runs SET project=$2, "user"=$3, prompt=$4, harness=$5, model=$6,
			base_ref=$7, interactive=$8, runtime=$9, namespace=$10, phase=$11,
			pr_url=$12, restart_count=$13, last_checkpoint=$14, conditions=$15,
			created_at=$16 WHERE id=$1`, run.ID, run.Project, run.User, run.Prompt,
		run.Harness, run.Model, run.BaseRef, run.Interactive, run.Runtime,
		run.Namespace, run.Phase, run.PRURL, run.RestartCount, checkpoint,
		conditions, run.CreatedAt)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
