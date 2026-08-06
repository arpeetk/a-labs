package blob

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Archive walks srcDir and writes a gzip-compressed tar stream of its full
// tree (including dotfiles like ".git") to dst, preserving relative paths,
// regular files, and directories. Symlinks are followed: a broken symlink
// (its target does not resolve) is silently skipped rather than failing the
// whole archive — the workspace is agent-written repo content, not a place
// symlink edge cases need special handling.
func Archive(dst io.Writer, srcDir string) error {
	gz := gzip.NewWriter(dst)
	tw := tar.NewWriter(gz)

	err := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, statErr := os.Stat(p) // follows the link
			if statErr != nil {
				return nil // broken symlink: skip
			}
			info = resolved
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("blob: archive header for %q: %w", rel, err)
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("blob: archive write header for %q: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("blob: archive open %q: %w", rel, err)
		}
		defer f.Close()
		if _, err := io.Copy(tw, f); err != nil {
			return fmt.Errorf("blob: archive copy %q: %w", rel, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("blob: close tar writer: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("blob: close gzip writer: %w", err)
	}
	return nil
}

// Unarchive is Archive's inverse: it reads a gzip-compressed tar stream from
// src and writes its entries into destDir, which the caller guarantees is
// empty. Any entry whose cleaned path would escape destDir is rejected —
// mirroring MountStore.resolve's escape check: don't trust the archive.
func Unarchive(src io.Reader, destDir string) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("blob: unarchive gzip reader: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("blob: unarchive tar read: %w", err)
		}
		target, err := resolveArchivePath(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("blob: unarchive mkdir %q: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("blob: unarchive mkdir parent of %q: %w", hdr.Name, err)
			}
			if err := writeArchiveFile(target, hdr, tr); err != nil {
				return err
			}
		default:
			// Anything else (symlinks, devices, ...) is not expected in a
			// workspace archive; skip rather than fail the whole restore.
		}
	}
}

func writeArchiveFile(target string, hdr *tar.Header, r io.Reader) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, hdr.FileInfo().Mode().Perm())
	if err != nil {
		return fmt.Errorf("blob: unarchive create %q: %w", hdr.Name, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("blob: unarchive write %q: %w", hdr.Name, err)
	}
	return f.Close()
}

// resolveArchivePath maps a tar entry name to an absolute path under destDir,
// rejecting any name that would escape it (e.g. "../../etc/passwd").
func resolveArchivePath(destDir, name string) (string, error) {
	clean := filepath.Join(destDir, filepath.FromSlash(name))
	if clean != destDir && !strings.HasPrefix(clean, destDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("blob: archive entry %q escapes destination", name)
	}
	return clean, nil
}
