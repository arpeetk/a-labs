package blob_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/summiteight/wren/internal/blob"
)

// exampleStore is test scaffolding that exists only to make the doc example
// runnable. It is NOT the promised implementation: real S3-compatible / GCS
// Stores (and MinIO for e2e) land with the post-launch checkpointer work.
type exampleStore struct {
	objects map[string][]byte
}

func newExampleStore() *exampleStore { return &exampleStore{objects: map[string][]byte{}} }

func (s *exampleStore) Put(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.objects[key] = b
	return nil
}

func (s *exampleStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := s.objects[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *exampleStore) List(_ context.Context, prefix string) ([]blob.Object, error) {
	var out []blob.Object
	for k, b := range s.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, blob.Object{Key: k, Size: int64(len(b)), Modified: time.Time{}})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// A checkpointer snapshots the workspace under "checkpoints/"; on resume,
// hydrate lists the run's checkpoints and reads back the latest bundle.
func ExampleStore() {
	ctx := context.Background()
	var s blob.Store = newExampleStore() // scoped to "runs/r-8f3a2c/" by the impl

	_ = s.Put(ctx, "checkpoints/ck-000042.bundle", strings.NewReader("git bundle bytes"))

	cks, _ := s.List(ctx, "checkpoints/")
	latest := cks[len(cks)-1]

	rc, err := s.Get(ctx, latest.Key)
	if err != nil {
		return
	}
	defer rc.Close()
	restored, _ := io.ReadAll(rc)

	fmt.Printf("restored %s (%d bytes): %s\n", latest.Key, latest.Size, restored)
	// Output: restored checkpoints/ck-000042.bundle (16 bytes): git bundle bytes
}
