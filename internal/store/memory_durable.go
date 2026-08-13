package store

import (
	"context"
	"sort"
	"time"
)

var _ Durable = (*Memory)(nil)

func (m *Memory) CreateRunWithOperation(_ context.Context, run *Run, op *Operation, event *RunEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[run.ID]; ok {
		return ErrExists
	}
	if _, ok := m.operations[op.ID]; ok {
		return ErrExists
	}
	runCopy := cloneRun(run)
	m.runs[run.ID] = &runCopy
	opCopy := cloneOperation(op)
	if opCopy.State == "" {
		opCopy.State = OperationPending
	}
	if opCopy.AvailableAt.IsZero() {
		opCopy.AvailableAt = time.Now().UTC()
	}
	if opCopy.CreatedAt.IsZero() {
		opCopy.CreatedAt = opCopy.AvailableAt
	}
	opCopy.UpdatedAt = opCopy.CreatedAt
	m.operations[op.ID] = &opCopy
	m.appendEventLocked(event)
	return nil
}

func (m *Memory) ClaimOperations(_ context.Context, worker string, limit int, lease time.Duration) ([]*Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 1
	}
	now := time.Now().UTC()
	ready := make([]*Operation, 0, len(m.operations))
	for _, op := range m.operations {
		pending := op.State == OperationPending && !op.AvailableAt.After(now)
		expired := op.State == OperationProcessing && !op.LeaseUntil.After(now)
		if pending || expired {
			ready = append(ready, op)
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].CreatedAt.Equal(ready[j].CreatedAt) {
			return ready[i].ID < ready[j].ID
		}
		return ready[i].CreatedAt.Before(ready[j].CreatedAt)
	})
	if len(ready) > limit {
		ready = ready[:limit]
	}
	out := make([]*Operation, 0, len(ready))
	for _, op := range ready {
		op.State = OperationProcessing
		op.LeaseOwner = worker
		op.LeaseUntil = now.Add(lease)
		op.Attempts++
		op.UpdatedAt = now
		cp := cloneOperation(op)
		out = append(out, &cp)
	}
	return out, nil
}

func (m *Memory) CompleteOperation(_ context.Context, worker, id string, event *RunEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[id]
	if !ok || op.State != OperationProcessing || op.LeaseOwner != worker {
		return ErrLeaseLost
	}
	op.State = OperationCompleted
	op.LeaseOwner = ""
	op.LeaseUntil = time.Time{}
	op.LastError = ""
	op.UpdatedAt = time.Now().UTC()
	m.appendEventLocked(event)
	return nil
}

func (m *Memory) RetryOperation(_ context.Context, worker, id, lastError string, availableAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[id]
	if !ok || op.State != OperationProcessing || op.LeaseOwner != worker {
		return ErrLeaseLost
	}
	op.State = OperationPending
	op.AvailableAt = availableAt.UTC()
	op.LeaseOwner = ""
	op.LeaseUntil = time.Time{}
	op.LastError = lastError
	op.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *Memory) FailOperation(_ context.Context, worker, id, lastError string, run *Run, event *RunEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[id]
	if !ok || op.State != OperationProcessing || op.LeaseOwner != worker {
		return ErrLeaseLost
	}
	if _, ok := m.runs[run.ID]; !ok {
		return ErrNotFound
	}
	runCopy := cloneRun(run)
	m.runs[run.ID] = &runCopy
	op.State = OperationFailed
	op.LeaseOwner = ""
	op.LeaseUntil = time.Time{}
	op.LastError = lastError
	op.UpdatedAt = time.Now().UTC()
	m.appendEventLocked(event)
	return nil
}

func (m *Memory) AppendRunEvent(_ context.Context, event *RunEvent) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[event.RunID]; !ok {
		return false, ErrNotFound
	}
	return m.appendEventLocked(event), nil
}

func (m *Memory) appendEventLocked(event *RunEvent) bool {
	if event == nil {
		return false
	}
	for _, prior := range m.events[event.RunID] {
		if prior.Source == event.Source && prior.SourceID == event.SourceID {
			return false
		}
	}
	m.nextEventID++
	cp := cloneEvent(event)
	cp.ID = m.nextEventID
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	m.events[event.RunID] = append(m.events[event.RunID], &cp)
	event.ID, event.CreatedAt = cp.ID, cp.CreatedAt
	return true
}

func (m *Memory) ListRunEvents(_ context.Context, runID string, afterID int64, limit int) ([]*RunEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[runID]; !ok {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	out := make([]*RunEvent, 0, limit)
	for _, event := range m.events[runID] {
		if event.ID <= afterID {
			continue
		}
		cp := cloneEvent(event)
		out = append(out, &cp)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (m *Memory) UpsertRunWithEvent(_ context.Context, run *Run, event *RunEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := cloneRun(run)
	m.runs[run.ID] = &cp
	m.appendEventLocked(event)
	return nil
}

func cloneOperation(op *Operation) Operation {
	cp := *op
	cp.Payload = append([]byte(nil), op.Payload...)
	return cp
}

func cloneEvent(event *RunEvent) RunEvent {
	cp := *event
	cp.Payload = append([]byte(nil), event.Payload...)
	return cp
}
