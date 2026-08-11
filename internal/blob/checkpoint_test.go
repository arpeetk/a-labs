package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/summiteight/wren/internal/blob"
)

func checkpointFixture(t *testing.T) (context.Context, *blob.MountStore, string) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "work.txt"), []byte("accepted work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return context.Background(), blob.NewMountStore(t.TempDir(), "runs/r-checkpoint"), workspace
}

func TestCheckpointPublishAndExactRestore(t *testing.T) {
	ctx, store, workspace := checkpointFixture(t)
	m, err := blob.PublishCheckpoint(ctx, store, workspace, "r-checkpoint", "pause", 5, time.Now())
	if err != nil {
		t.Fatalf("PublishCheckpoint: %v", err)
	}
	if m.FormatVersion != 1 || m.SHA256 == "" || m.SizeBytes == 0 || !strings.HasSuffix(m.ManifestKey, ".json") {
		t.Fatalf("manifest = %+v", m)
	}

	loaded, rc, err := blob.LoadCheckpoint(ctx, store, m.ManifestKey)
	if err != nil {
		t.Fatalf("LoadCheckpoint exact: %v", err)
	}
	defer rc.Close()
	if loaded.ID != m.ID || loaded.SHA256 != m.SHA256 {
		t.Fatalf("loaded = %+v, want %+v", loaded, m)
	}
	restored := t.TempDir()
	if err := blob.Unarchive(rc, restored); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(restored, "work.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "accepted work\n" {
		t.Fatalf("restored = %q", got)
	}
}

func TestCheckpointInterruptedPublicationIsInvisible(t *testing.T) {
	ctx, store, _ := checkpointFixture(t)
	if err := store.Put(ctx, "checkpoints/objects/orphan.tar.gz", strings.NewReader("partial")); err != nil {
		t.Fatal(err)
	}
	_, rc, err := blob.LoadCheckpoint(ctx, store, "")
	if rc != nil {
		rc.Close()
	}
	if !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("LoadCheckpoint orphan = %v, want ErrNotFound", err)
	}
}

func TestCheckpointLatestFallsBackButExactNeverDoes(t *testing.T) {
	ctx, store, workspace := checkpointFixture(t)
	older, err := blob.PublishCheckpoint(ctx, store, workspace, "r-checkpoint", "periodic", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(workspace, "work.txt"), []byte("new state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newer, err := blob.PublishCheckpoint(ctx, store, workspace, "r-checkpoint", "periodic", 5, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, newer.ArchiveKey, strings.NewReader("corrupt")); err != nil {
		t.Fatal(err)
	}

	got, rc, err := blob.LoadCheckpoint(ctx, store, "")
	if err != nil {
		t.Fatalf("latest valid fallback: %v", err)
	}
	rc.Close()
	if got.ID != older.ID {
		t.Fatalf("fallback ID = %s, want %s", got.ID, older.ID)
	}
	if _, rc, err := blob.LoadCheckpoint(ctx, store, newer.ManifestKey); err == nil {
		if rc != nil {
			rc.Close()
		}
		t.Fatal("exact corrupt checkpoint unexpectedly loaded")
	}
}

func TestCheckpointRetentionPrunesManifestArchivePairs(t *testing.T) {
	ctx, store, workspace := checkpointFixture(t)
	for i := 0; i < 4; i++ {
		time.Sleep(2 * time.Millisecond)
		if _, err := blob.PublishCheckpoint(ctx, store, workspace, "r-checkpoint", "periodic", 2, time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	objects, err := store.List(ctx, "checkpoints/")
	if err != nil {
		t.Fatal(err)
	}
	var manifests, archives int
	for _, o := range objects {
		if strings.HasSuffix(o.Key, ".json") {
			manifests++
		}
		if strings.Contains(o.Key, "/objects/") && strings.HasSuffix(o.Key, ".tar.gz") {
			archives++
		}
	}
	if manifests != 2 || archives != 2 {
		t.Fatalf("retained manifests=%d archives=%d, want 2/2", manifests, archives)
	}
}

type corruptReadbackStore struct {
	blob.Store
}

func (s corruptReadbackStore) Put(ctx context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if strings.Contains(key, "/objects/") {
		data = []byte("changed after put")
	}
	return s.Store.Put(ctx, key, bytes.NewReader(data))
}

func TestCheckpointReadbackMismatchNeverPublishesManifest(t *testing.T) {
	ctx, base, workspace := checkpointFixture(t)
	store := corruptReadbackStore{Store: base}
	if _, err := blob.PublishCheckpoint(ctx, store, workspace, "r-checkpoint", "pause", 5, time.Now()); err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("PublishCheckpoint = %v, want integrity mismatch", err)
	}
	objects, err := base.List(ctx, "checkpoints/")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objects {
		if strings.HasSuffix(o.Key, ".json") {
			t.Fatalf("manifest committed after failed verification: %s", o.Key)
		}
	}
}

func TestMountStoreDeleteIsIdempotentAndRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMountStore(t.TempDir(), "runs/r-a")
	if err := store.Put(ctx, "checkpoint", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "checkpoint"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "checkpoint"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if err := store.Delete(ctx, "../r-b/secret"); err == nil {
		t.Fatal("traversal Delete unexpectedly succeeded")
	}
}
