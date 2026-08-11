// Package store is the control plane's persistence layer: Projects and Runs.
//
// It defines the Store interface plus two implementations: in-memory (the
// default; tests and local development) and Postgres (postgres.go, pgx/v5 —
// spec §5.2). Callers depend only on the interface.
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
