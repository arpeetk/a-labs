package blob_test

import (
	"testing"

	"github.com/summiteight/wren/internal/blob"
)

func TestRunPrefix(t *testing.T) {
	cases := []struct {
		name   string
		bucket string
		runID  string
		want   string
	}{
		{"gs bare bucket", "gs://wren-ckpt", "r-8f3a2c", "runs/r-8f3a2c"},
		{"gs bucket with base prefix", "gs://wren-ckpt/ckpts", "r-8f3a2c", "ckpts/runs/r-8f3a2c"},
		{"bare bucket no scheme", "wren-ckpt", "r-8f3a2c", "runs/r-8f3a2c"},
		{"bare bucket with base prefix", "wren-ckpt/ckpts", "r-8f3a2c", "ckpts/runs/r-8f3a2c"},
		{"nested base prefix", "gs://wren-ckpt/a/b", "r-1", "a/b/runs/r-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := blob.RunPrefix(tc.bucket, tc.runID)
			if got != tc.want {
				t.Errorf("RunPrefix(%q, %q) = %q, want %q", tc.bucket, tc.runID, got, tc.want)
			}
		})
	}
}
