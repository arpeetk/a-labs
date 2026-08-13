// Package store is the control plane's persistence layer: Projects and Runs.
//
// It defines the Store interface plus two implementations: in-memory (the
// default; tests and local development) and Postgres (postgres.go, pgx/v5 —
// spec §5.2). Callers depend only on the interface.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned when a requested object does not exist.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when creating an object whose key already exists.
var ErrExists = errors.New("already exists")

// ErrLeaseLost means an outbox worker tried to complete work it no longer
// owns. Leases deliberately expire so another apiserver replica can recover a
// process that died after claiming an operation.
var ErrLeaseLost = errors.New("operation lease lost")

// Project is a registered repo and its run defaults (spec §4).
type Project struct {
	Name             string    `json:"name"` // unique id, e.g. "payments-api"
	Repo             string    `json:"repo"` // GitHub "owner/repo"
	DefaultHarness   string    `json:"defaultHarness"`
	HarnessImage     string    `json:"harnessImage"`
	DefaultModel     string    `json:"defaultModel"`
	RuntimeClass     string    `json:"runtimeClass"`
	CPU              string    `json:"cpu"`
	Memory           string    `json:"memory"`
	Disk             string    `json:"disk"`
	CheckpointBucket string    `json:"checkpointBucket"`
	EgressAllowlist  []string  `json:"egressAllowlist"`
	Namespace        string    `json:"namespace"`
	CreatedAt        time.Time `json:"createdAt"`
}

// Run is the control-plane record of an agent run; a mirror of the AgentRun CR
// plus the submission metadata.
type Run struct {
	ID             string         `json:"id"`
	Project        string         `json:"project"`
	User           string         `json:"user"`
	Prompt         string         `json:"prompt"`
	Harness        string         `json:"harness"`
	Model          string         `json:"model"`
	BaseRef        string         `json:"baseRef"`
	Interactive    bool           `json:"interactive"`
	Runtime        string         `json:"runtime"`
	Namespace      string         `json:"namespace"`
	Phase          string         `json:"phase"`
	PRURL          string         `json:"prUrl,omitempty"`
	RestartCount   int32          `json:"restartCount"`
	LastCheckpoint *RunCheckpoint `json:"lastCheckpoint,omitempty"`
	Conditions     []RunCondition `json:"conditions,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
}

type RunCheckpoint struct {
	ID            string    `json:"id,omitempty"`
	URI           string    `json:"uri,omitempty"`
	At            time.Time `json:"at,omitempty"`
	SHA256        string    `json:"sha256,omitempty"`
	SizeBytes     int64     `json:"sizeBytes,omitempty"`
	FormatVersion int32     `json:"formatVersion,omitempty"`
	Trigger       string    `json:"trigger,omitempty"`
}

type RunCondition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime,omitempty"`
}

// RunFilter narrows ListRuns. An empty filter returns all runs.
type RunFilter struct {
	User    string // exact user match (for scope=mine)
	Project string // exact project match
}

// RunEvent is one immutable item in a run's durable, ordered audit stream.
// Source+SourceID is the idempotency key: gateway replays and repeated CR
// reconciliation insert exactly once without relying on best-effort timing.
type RunEvent struct {
	ID        int64           `json:"id"`
	RunID     string          `json:"runId"`
	Source    string          `json:"source"`
	SourceID  string          `json:"sourceId"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}

const (
	OperationPending    = "pending"
	OperationProcessing = "processing"
	OperationCompleted  = "completed"
	OperationFailed     = "failed"
	OperationLaunchRun  = "launch_run"
)

// Operation is a transactionally-enqueued external effect. A worker claims it
// with a finite lease; success or retry is conditional on that lease owner so
// two HA apiservers cannot both advance the same item.
type Operation struct {
	ID          string          `json:"id"`
	RunID       string          `json:"runId"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	State       string          `json:"state"`
	Attempts    int             `json:"attempts"`
	AvailableAt time.Time       `json:"availableAt"`
	LeaseOwner  string          `json:"leaseOwner,omitempty"`
	LeaseUntil  time.Time       `json:"leaseUntil,omitempty"`
	LastError   string          `json:"lastError,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

// Store persists Projects and Runs.
type Store interface {
	CreateProject(ctx context.Context, p *Project) error
	GetProject(ctx context.Context, name string) (*Project, error)
	ListProjects(ctx context.Context) ([]*Project, error)

	CreateRun(ctx context.Context, r *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context, f RunFilter) ([]*Run, error)
	UpdateRun(ctx context.Context, r *Run) error
	// DeleteRun removes a run's store record (ErrNotFound if unknown). The
	// AgentRun CR and its owned pod/PVC are deleted separately by the launcher
	// (`wren run rm`, WS-15 Part C).
	DeleteRun(ctx context.Context, id string) error
}

// Durable is the production control-plane extension implemented by Memory and
// Postgres. It is separate from Store so third-party Store implementations keep
// source compatibility; helper functions in durable.go retain safe fallbacks.
type Durable interface {
	CreateRunWithOperation(ctx context.Context, run *Run, op *Operation, event *RunEvent) error
	ClaimOperations(ctx context.Context, worker string, limit int, lease time.Duration) ([]*Operation, error)
	CompleteOperation(ctx context.Context, worker, id string, event *RunEvent) error
	RetryOperation(ctx context.Context, worker, id, lastError string, availableAt time.Time) error
	FailOperation(ctx context.Context, worker, id, lastError string, run *Run, event *RunEvent) error
	AppendRunEvent(ctx context.Context, event *RunEvent) (bool, error)
	ListRunEvents(ctx context.Context, runID string, afterID int64, limit int) ([]*RunEvent, error)
	UpsertRunWithEvent(ctx context.Context, run *Run, event *RunEvent) error
}

// Health is implemented by stores that can verify their backing dependency.
type Health interface {
	Ping(context.Context) error
}
