package blob_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/summiteight/wren/internal/blob"
)

// buildTree lays out a small directory tree: nested dirs, a ".git"-shaped
// path, and an empty dir.
func buildTree(t *testing.T, root string) {
	t.Helper()
	dirs := []string{
		".git",
		".git/objects",
		"src/pkg",
		"empty",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	files := map[string]string{
		"README.md":            "hello\n",
		".git/HEAD":            "ref: refs/heads/main\n",
		".git/objects/abc123":  "blob data",
		"src/pkg/main.go":      "package main\n",
		"src/pkg/main_test.go": "package main\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// listTree returns every path (relative to root, slash-separated) present
// under root, plus whether it's a directory, for structural comparison.
func listTree(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		out[filepath.ToSlash(rel)] = d.IsDir()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func TestArchiveUnarchiveRoundTrip(t *testing.T) {
	src := t.TempDir()
	buildTree(t, src)

	var buf bytes.Buffer
	if err := blob.Archive(&buf, src); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	dst := t.TempDir()
	if err := blob.Unarchive(&buf, dst); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}

	wantTree := listTree(t, src)
	gotTree := listTree(t, dst)
	if len(wantTree) != len(gotTree) {
		t.Fatalf("tree entry count mismatch: want %d, got %d (want=%v got=%v)", len(wantTree), len(gotTree), wantTree, gotTree)
	}
	for rel, wantDir := range wantTree {
		gotDir, ok := gotTree[rel]
		if !ok {
			t.Errorf("missing entry %q after round-trip", rel)
			continue
		}
		if gotDir != wantDir {
			t.Errorf("entry %q: dir mismatch want=%v got=%v", rel, wantDir, gotDir)
		}
	}

	// Content match for every regular file.
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		want, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Errorf("read restored %q: %v", rel, err)
			return nil
		}
		if !bytes.Equal(want, got) {
			t.Errorf("content mismatch for %q: want %q, got %q", rel, want, got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk src for content check: %v", err)
	}

	// The empty dir must survive the round trip.
	if fi, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("empty dir did not survive round-trip: %v", err)
	}
}

func TestUnarchiveRejectsPathEscape(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "../../etc/passwd",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     4,
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}

	dst := t.TempDir()
	if err := blob.Unarchive(&buf, dst); err == nil {
		t.Fatal("Unarchive: want error for path-escaping entry, got nil")
	}
}

func TestArchiveDeterministicEntryCount(t *testing.T) {
	src := t.TempDir()
	buildTree(t, src)

	var buf bytes.Buffer
	if err := blob.Archive(&buf, src); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("archive contained no entries")
	}
}

func TestArchivePreservesSymlinkWithoutReadingTarget(t *testing.T) {
	src := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("must-not-enter-checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "agent-link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := blob.Archive(&buf, src); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := tar.NewReader(gz).Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Typeflag != tar.TypeSymlink || hdr.Linkname != outside || hdr.Size != 0 {
		t.Fatalf("symlink header = type %d target %q size %d", hdr.Typeflag, hdr.Linkname, hdr.Size)
	}
}

func TestArchiveUnarchiveSafeRelativeSymlink(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "real", "file"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real/file", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := blob.Archive(&buf, src); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := blob.Unarchive(&buf, dst); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil || target != "real/file" {
		t.Fatalf("restored link = %q, %v", target, err)
	}
}

func TestUnarchiveRejectsUnsafeSymlink(t *testing.T) {
	for _, target := range []string{"/mnt/checkpoints/runs/other-run/checkpoint", "../../outside"} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := blob.Unarchive(&buf, t.TempDir()); err == nil {
			t.Errorf("Unarchive accepted unsafe symlink target %q", target)
		}
	}
}

func TestUnarchiveRejectsWriteThroughEscapingSymlink(t *testing.T) {
	dst := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dst, "pivot")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "pivot/escaped", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	if err := blob.Unarchive(&buf, dst); err == nil {
		t.Fatal("Unarchive accepted a write through an escaping symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("outside file exists after rejected extraction: %v", err)
	}
}
