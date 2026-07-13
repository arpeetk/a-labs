// Package store is the control plane's persistence layer: Projects and Runs.
//
// It defines the Store interface plus an in-memory implementation used for
// tests and local development. A Cloud SQL (Postgres) implementation lands
// later (spec §5.2); callers depend only on the interface.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a requested object does not exist.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when creating an object whose key already exists.
var ErrExists = errors.New("already exists")

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
	ID           string    `json:"id"`
	Project      string    `json:"project"`
	User         string    `json:"user"`
	Prompt       string    `json:"prompt"`
	Harness      string    `json:"harness"`
	Model        string    `json:"model"`
	BaseRef      string    `json:"baseRef"`
	Interactive  bool      `json:"interactive"`
	Runtime      string    `json:"runtime"`
	Namespace    string    `json:"namespace"`
	Phase        string    `json:"phase"`
	PRURL        string    `json:"prUrl,omitempty"`
	RestartCount int32     `json:"restartCount"`
	CreatedAt    time.Time `json:"createdAt"`
}

// RunFilter narrows ListRuns. An empty filter returns all runs.
type RunFilter struct {
	User    string // exact user match (for scope=mine)
	Project string // exact project match
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
}
