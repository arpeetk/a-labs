// Package blob defines the object-store contract for a run's durable data:
// workspace checkpoints and the mirrored session transcript.
//
// A Store is scoped to one run's prefix (e.g. "runs/r-8f3a2c/"); every key is
// relative to that prefix, so a run can never read or overwrite another run's
// objects. RunPrefix derives that per-run prefix from a run's checkpoint
// bucket value. The checkpointer sidecar Puts tar.gz snapshots of the
// workspace tree under "checkpoints/" (e.g. "checkpoints/ck-000042.tar.gz" —
// a tar.gz of the workspace directory, including ".git" and any uncommitted
// edits; NOT a git bundle, which would only capture committed refs) and
// transcript fragments under "transcript/"; hydrate's checkpoint-restore path
// Lists "checkpoints/" and Gets the latest one.
//
// MountStore is the live-proven implementation used with the GCS FUSE CSI
// driver. Periodic snapshots, restore, and bounded retention ship when a
// checkpoint mount is configured. Transcript mirroring ("transcript/") is not
// yet implemented.
package blob

import (
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"time"
)

// RunPrefix derives the per-run object-key prefix for a checkpoint bucket
// value. bucket is "gs://<bucket-name>[/<base-prefix>]" or bare
// "<bucket-name>[/<base-prefix>]" (the scheme, if any, and the bucket name
// itself are not part of the returned prefix — only the base-prefix path
// segment, if present, matters here; the bucket name is handled separately by
// whatever mounts/addresses the bucket). The result is
// path.Join(basePrefix, "runs", runID), so two runs sharing the same bucket
// (the default: every run's checkpoint bucket defaults to the same
// "gs://wren-ckpt" unless overridden) never see each other's objects.
func RunPrefix(bucket, runID string) string {
	b := strings.TrimPrefix(bucket, "gs://")
	basePrefix := ""
	if i := strings.IndexByte(b, '/'); i >= 0 {
		basePrefix = b[i+1:]
	}
	return path.Join(basePrefix, "runs", runID)
}

// ErrNotFound is returned by Get when no object exists at the key.
var ErrNotFound = errors.New("blob: object not found")

// Object is one entry under the run's prefix, as returned by List.
type Object struct {
	// Key is the object key, relative to the run's prefix (e.g.
	// "checkpoints/ck-000042.tar.gz").
	Key string
	// Size is the object length in bytes.
	Size int64
	// Modified is the last-modified time reported by the backend; resume uses
	// it to pick the latest checkpoint.
	Modified time.Time
}

// Store is a minimal object store scoped to a single run's prefix. It carries
// only the operations needed to publish, restore, and prune durable objects.
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

	// Delete removes an object. Missing objects are tolerated so retention and
	// cleanup remain idempotent across retries.
	Delete(ctx context.Context, key string) error
}
