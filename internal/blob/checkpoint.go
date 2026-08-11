package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const CheckpointFormatVersion = 1

// CheckpointManifest is the atomic publication marker for an immutable
// workspace archive. Restore ignores archives without a valid manifest.
type CheckpointManifest struct {
	FormatVersion int       `json:"formatVersion"`
	ID            string    `json:"id"`
	RunID         string    `json:"runId"`
	ArchiveKey    string    `json:"archiveKey"`
	SHA256        string    `json:"sha256"`
	SizeBytes     int64     `json:"sizeBytes"`
	CreatedAt     time.Time `json:"createdAt"`
	Trigger       string    `json:"trigger"`
	ManifestKey   string    `json:"manifestKey"`
	Warning       string    `json:"warning,omitempty"`
}

// PublishCheckpoint archives and verifies the workspace before publishing its
// manifest. The manifest Put is the commit point: interrupted archive writes
// are never visible to restore.
func PublishCheckpoint(ctx context.Context, store Store, workspace, runID, trigger string, retain int, now time.Time) (CheckpointManifest, error) {
	if runID == "" {
		return CheckpointManifest{}, errors.New("blob: checkpoint run id is required")
	}
	if retain <= 0 {
		retain = 5
	}
	now = now.UTC()
	id := fmt.Sprintf("ck-%d", now.UnixNano())
	var archive bytes.Buffer
	if err := Archive(&archive, workspace); err != nil {
		return CheckpointManifest{}, fmt.Errorf("blob: checkpoint archive: %w", err)
	}
	payload := append([]byte(nil), archive.Bytes()...)
	sum := sha256.Sum256(payload)
	m := CheckpointManifest{
		FormatVersion: CheckpointFormatVersion,
		ID:            id, RunID: runID, Trigger: trigger, CreatedAt: now,
		ArchiveKey:  "checkpoints/objects/" + id + ".tar.gz",
		ManifestKey: "checkpoints/" + id + ".json",
		SHA256:      hex.EncodeToString(sum[:]), SizeBytes: int64(len(payload)),
	}
	if err := store.Put(ctx, m.ArchiveKey, bytes.NewReader(payload)); err != nil {
		return CheckpointManifest{}, fmt.Errorf("blob: checkpoint put archive: %w", err)
	}
	if err := verifyArchive(ctx, store, m); err != nil {
		return CheckpointManifest{}, fmt.Errorf("blob: checkpoint read-back verification: %w", err)
	}
	manifestBytes, err := json.Marshal(m)
	if err != nil {
		return CheckpointManifest{}, fmt.Errorf("blob: checkpoint marshal manifest: %w", err)
	}
	if err := store.Put(ctx, m.ManifestKey, bytes.NewReader(manifestBytes)); err != nil {
		return CheckpointManifest{}, fmt.Errorf("blob: checkpoint publish manifest: %w", err)
	}
	if err := pruneCheckpoints(ctx, store, retain); err != nil {
		// Publication already committed. Retention failure is observable but must
		// not make a safe pause destroy its still-live workspace.
		m.Warning = err.Error()
	}
	return m, nil
}

func verifyArchive(ctx context.Context, store Store, m CheckpointManifest) error {
	rc, err := store.Get(ctx, m.ArchiveKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return err
	}
	if n != m.SizeBytes || hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return fmt.Errorf("archive %s integrity mismatch: got size=%d sha256=%s, want size=%d sha256=%s", m.ArchiveKey, n, hex.EncodeToString(h.Sum(nil)), m.SizeBytes, m.SHA256)
	}
	return nil
}

// LoadCheckpoint validates a manifest and its immutable archive. When key is
// empty it selects the newest valid manifest, falling back over corruption.
// With an explicit key it never falls back.
func LoadCheckpoint(ctx context.Context, store Store, key string) (CheckpointManifest, io.ReadCloser, error) {
	if key != "" {
		return loadManifest(ctx, store, key)
	}
	objs, err := store.List(ctx, "checkpoints/")
	if err != nil {
		return CheckpointManifest{}, nil, err
	}
	sort.SliceStable(objs, func(i, j int) bool { return objs[i].Modified.After(objs[j].Modified) })
	var invalid []string
	for _, obj := range objs {
		if !strings.HasSuffix(obj.Key, ".json") {
			continue
		}
		m, rc, err := loadManifest(ctx, store, obj.Key)
		if err == nil {
			return m, rc, nil
		}
		invalid = append(invalid, obj.Key+": "+err.Error())
	}
	// Migration compatibility for WS-21 snapshots that predate manifests.
	for _, obj := range objs {
		if strings.HasSuffix(obj.Key, ".tar.gz") && !strings.Contains(obj.Key, "/objects/") {
			rc, err := store.Get(ctx, obj.Key)
			if err == nil {
				return CheckpointManifest{ID: strings.TrimSuffix(strings.TrimPrefix(obj.Key, "checkpoints/"), ".tar.gz"), ArchiveKey: obj.Key, ManifestKey: obj.Key, SizeBytes: obj.Size, CreatedAt: obj.Modified, Trigger: "legacy"}, rc, nil
			}
		}
	}
	if len(invalid) > 0 {
		return CheckpointManifest{}, nil, fmt.Errorf("blob: no valid checkpoint (%s)", strings.Join(invalid, "; "))
	}
	return CheckpointManifest{}, nil, ErrNotFound
}

func loadManifest(ctx context.Context, store Store, key string) (CheckpointManifest, io.ReadCloser, error) {
	rc, err := store.Get(ctx, key)
	if err != nil {
		return CheckpointManifest{}, nil, err
	}
	data, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		return CheckpointManifest{}, nil, readErr
	}
	var m CheckpointManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return CheckpointManifest{}, nil, fmt.Errorf("decode manifest %s: %w", key, err)
	}
	if m.FormatVersion != CheckpointFormatVersion || m.ID == "" || m.ArchiveKey == "" || m.SHA256 == "" || m.SizeBytes < 0 {
		return CheckpointManifest{}, nil, fmt.Errorf("invalid checkpoint manifest %s", key)
	}
	if m.ManifestKey == "" {
		m.ManifestKey = key
	}
	if err := verifyArchive(ctx, store, m); err != nil {
		return CheckpointManifest{}, nil, err
	}
	archive, err := store.Get(ctx, m.ArchiveKey)
	if err != nil {
		return CheckpointManifest{}, nil, err
	}
	return m, archive, nil
}

func pruneCheckpoints(ctx context.Context, store Store, retain int) error {
	objs, err := store.List(ctx, "checkpoints/")
	if err != nil {
		return fmt.Errorf("checkpoint retention list: %w", err)
	}
	var manifests []Object
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".json") {
			manifests = append(manifests, o)
		}
	}
	sort.SliceStable(manifests, func(i, j int) bool { return manifests[i].Modified.After(manifests[j].Modified) })
	if len(manifests) <= retain {
		return nil
	}
	var errs []error
	for _, o := range manifests[retain:] {
		m, rc, err := loadManifest(ctx, store, o.Key)
		if rc != nil {
			_ = rc.Close()
		}
		if err := store.Delete(ctx, o.Key); err != nil {
			errs = append(errs, err)
		}
		// Remove the publication marker first. A crash between deletes leaves an
		// invisible orphan archive, never a visible manifest with missing data.
		if err == nil {
			if err := store.Delete(ctx, m.ArchiveKey); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
