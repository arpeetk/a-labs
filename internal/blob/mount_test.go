package blob_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/summiteight/wren/internal/blob"
)

func TestMountStore_PutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMountStore(t.TempDir(), "runs/r-abc")

	if err := s.Put(ctx, "checkpoints/ck-000042.bundle", strings.NewReader("git bundle bytes")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := s.Get(ctx, "checkpoints/ck-000042.bundle")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "git bundle bytes" {
		t.Errorf("round-trip = %q, want %q", got, "git bundle bytes")
	}
}

func TestMountStore_PutReplaces(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMountStore(t.TempDir(), "runs/r-abc")

	if err := s.Put(ctx, "k", strings.NewReader("first")); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	if err := s.Put(ctx, "k", strings.NewReader("second")); err != nil {
		t.Fatalf("Put second: %v", err)
	}
	rc, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "second" {
		t.Errorf("after replace = %q, want %q", got, "second")
	}
}

func TestMountStore_GetNotFound(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMountStore(t.TempDir(), "runs/r-abc")

	_, err := s.Get(ctx, "missing")
	if !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestMountStore_List(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMountStore(t.TempDir(), "runs/r-abc")

	for _, k := range []string{"checkpoints/ck-2", "checkpoints/ck-1", "transcript/t-1"} {
		if err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}

	// Prefix scopes and results come back in key order.
	cks, err := s.List(ctx, "checkpoints/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var keys []string
	for _, o := range cks {
		keys = append(keys, o.Key)
		if o.Size != int64(len(o.Key)) {
			t.Errorf("Object %q Size = %d, want %d", o.Key, o.Size, len(o.Key))
		}
		if o.Modified.IsZero() {
			t.Errorf("Object %q Modified is zero", o.Key)
		}
	}
	want := []string{"checkpoints/ck-1", "checkpoints/ck-2"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("List(checkpoints/) = %v, want %v", keys, want)
	}

	// Empty prefix lists everything under the root.
	all, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List(\"\") returned %d objects, want 3", len(all))
	}
}

func TestMountStore_ListAbsentPrefix(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMountStore(t.TempDir(), "runs/r-abc")

	// Nothing written yet: listing a never-created prefix is empty, not an error.
	objs, err := s.List(ctx, "checkpoints/")
	if err != nil {
		t.Fatalf("List absent prefix: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("List absent prefix = %v, want empty", objs)
	}
}

// TestMountStore_PrefixIsolation pins the per-run boundary: two stores at
// different prefixes under one base cannot see each other's objects, and a key
// crafted to escape the root via ".." is rejected rather than resolving into a
// sibling run's tree.
func TestMountStore_PrefixIsolation(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	a := blob.NewMountStore(base, "runs/r-aaa")
	b := blob.NewMountStore(base, "runs/r-bbb")

	if err := a.Put(ctx, "secret", strings.NewReader("a-only")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := b.Get(ctx, "secret"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("run b saw run a's object: err = %v, want ErrNotFound", err)
	}

	// A traversal key must not reach run b's tree even though it shares the base.
	if err := b.Put(ctx, "victim", strings.NewReader("b-only")); err != nil {
		t.Fatalf("Put victim: %v", err)
	}
	if _, err := a.Get(ctx, "../r-bbb/victim"); err == nil {
		t.Error("traversal key resolved outside the store root; want rejection")
	}
	if err := a.Put(ctx, "../r-bbb/victim", strings.NewReader("clobbered")); err == nil {
		t.Error("traversal Put escaped the store root; want rejection")
	}
	// Confirm run b's object is intact after the attempted clobber.
	rc, err := b.Get(ctx, "victim")
	if err != nil {
		t.Fatalf("Get victim: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "b-only" {
		t.Errorf("run b's object was clobbered: %q", got)
	}
}

func TestMountStore_PutContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := blob.NewMountStore(t.TempDir(), "runs/r-abc")
	if err := s.Put(ctx, "k", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Put with canceled ctx = %v, want context.Canceled", err)
	}
}

// TestMountStore_NestedPut confirms Put creates missing parent directories.
func TestMountStore_NestedPut(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	s := blob.NewMountStore(base, "runs/r-abc")
	if err := s.Put(ctx, "a/b/c/deep.txt", strings.NewReader("deep")); err != nil {
		t.Fatalf("Put nested: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "runs", "r-abc", "a", "b", "c", "deep.txt")); err != nil {
		t.Errorf("nested file not created on disk: %v", err)
	}
}
