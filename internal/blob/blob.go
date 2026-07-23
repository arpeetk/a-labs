// Package blob defines the object-store contract for a run's durable data:
// workspace checkpoints and the mirrored session transcript (spec §5.5).
//
// A Store is scoped to one run's prefix (e.g. "runs/r-8f3a2c/"); every key is
// relative to that prefix, so a run can never read or overwrite another run's
// objects. The checkpointer sidecar Puts git-aware checkpoint bundles under
// "checkpoints/" and transcript fragments under "transcript/"; hydrate's
// checkpoint-restore path Lists "checkpoints/" and Gets the latest bundle.
//
// This package is the socket only: v0.1 ships no implementation and takes no
// checkpoints (the workspace.checkpoint.* CRD fields are accepted but no-op,
// and crash-resume is PVC reattach + resume-mode — spec §5.5 v0.1 status).
// Intended implementations: S3-compatible object storage and GCS, both behind
// this one contract, with MinIO standing in for e2e so the checkpoint path can
// be exercised in-cluster without cloud credentials.
package blob

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned by Get when no object exists at the key.
var ErrNotFound = errors.New("blob: object not found")

// Object is one entry under the run's prefix, as returned by List.
type Object struct {
	// Key is the object key, relative to the run's prefix (e.g.
	// "checkpoints/ck-000042.bundle").
	Key string
	// Size is the object length in bytes.
	Size int64
	// Modified is the last-modified time reported by the backend; resume uses
	// it to pick the latest checkpoint.
	Modified time.Time
}

// Store is a minimal object store scoped to a single run's prefix. It carries
// exactly what spec §5.5 needs and nothing more; lifecycle/expiry of old
// checkpoints is bucket policy (spec §6), not part of this contract.
type Store interface {
	// Put writes the object at key (relative to the run's prefix), replacing
	// any object already there. The caller retains ownership of r.
	Put(ctx context.Context, key string, r io.Reader) error

	// Get opens the object at key for reading; the caller must close it.
	// It returns ErrNotFound when the key does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// List returns the objects under prefix (relative to the run's prefix) in
	// key order. An empty prefix lists everything in the run's prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
}
