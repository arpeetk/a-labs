package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MountStore implements Store over an ordinary POSIX filesystem tree — the
// concrete backing for a run whose durable storage is a mounted bucket. On GKE
// the mount is a GCS bucket surfaced by the Cloud Storage FUSE CSI driver, so
// all GCS-specific concerns (auth, the object API) live entirely in Google's
// CSI sidecar and never in this Go code: from here it is just files (spec §5.5,
// WS-18). The same type serves any future POSIX-mounted backend (S3 via
// s3fs/goofys, a local temp dir in tests) unchanged.
//
// A MountStore is rooted at base/prefix and every key is resolved relative to
// that root, mirroring the Store contract's per-run prefix isolation: two runs
// pointed at the same bucket with different prefixes cannot see or clobber each
// other's objects. Keys that would escape the root via ".." are rejected —
// the prefix boundary holds by construction, not by trusting the caller.
type MountStore struct {
	root string
}

// NewMountStore returns a Store rooted at base joined with prefix. base is the
// mount path (e.g. "/mnt/checkpoints"); prefix is the run's subdirectory (e.g.
// "runs/r-8f3a2c"). Keys passed to Put/Get/List are relative to that root.
func NewMountStore(base, prefix string) *MountStore {
	return &MountStore{root: filepath.Join(base, filepath.FromSlash(prefix))}
}

// resolve maps a slash-separated key to an absolute path under the root,
// rejecting any key that would escape the root. This is the prefix-isolation
// pin: a key like "../other-run/secret" must never resolve outside root, so the
// per-run boundary is enforced here, not assumed of callers.
func (m *MountStore) resolve(key string) (string, error) {
	clean := filepath.Join(m.root, filepath.FromSlash(key))
	// filepath.Join already Cleans, collapsing "..". Confirm the result is still
	// within root (or is root itself) before touching the filesystem.
	if clean != m.root && !strings.HasPrefix(clean, m.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("blob: key %q escapes store root", key)
	}
	return clean, nil
}

// Put writes the object at key, creating parent directories as needed and
// replacing any existing object. The caller retains ownership of r.
func (m *MountStore) Put(ctx context.Context, key string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := m.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("blob: create parent dirs for %q: %w", key, err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("blob: create %q: %w", key, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("blob: write %q: %w", key, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("blob: close %q: %w", key, err)
	}
	return nil
}

// Get opens the object at key for reading; the caller must close it. A missing
// key maps to ErrNotFound so callers branch on errors.Is(err, ErrNotFound)
// rather than on a filesystem-specific error.
func (m *MountStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := m.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errorsIsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("blob: open %q: %w", key, err)
	}
	return f, nil
}

// List returns the objects under prefix (relative to the store root) in key
// order. An empty prefix lists everything under the root. A prefix that names
// no directory yet returns an empty list, not an error — nothing has been
// written there.
func (m *MountStore) List(ctx context.Context, prefix string) ([]Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start, err := m.resolve(prefix)
	if err != nil {
		return nil, err
	}
	var out []Object
	err = filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errorsIsNotExist(err) && path == start {
				return filepath.SkipAll // prefix dir absent: nothing written yet.
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(m.root, path)
		if err != nil {
			return err
		}
		out = append(out, Object{
			Key:      filepath.ToSlash(rel),
			Size:     info.Size(),
			Modified: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("blob: list %q: %w", prefix, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// errorsIsNotExist centralizes the not-exist check so Get and List agree on it.
// errors.Is (not ==) so a wrapped fs.ErrNotExist still maps to ErrNotFound.
func errorsIsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
