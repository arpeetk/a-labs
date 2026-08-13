package blob

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Archive walks srcDir and writes a gzip-compressed tar stream of its full
// tree (including dotfiles like ".git") to dst, preserving relative paths,
// regular files, directories, and safe relative symlinks. Symlinks are archived
// as links and never followed: the workspace is controlled by the untrusted
// harness, while the checkpointer can see mounts the harness cannot. Following
// a workspace link here would let the harness make the trusted checkpointer
// copy data from those mounts into its own checkpoint.
func Archive(dst io.Writer, srcDir string) error {
	gz := gzip.NewWriter(dst)
	tw := tar.NewWriter(gz)

	writer := archiveWriter{root: srcDir, tar: tw}
	if err := filepath.WalkDir(srcDir, writer.writeEntry); err != nil {
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

type archiveWriter struct {
	root string
	tar  *tar.Writer
}

func (w archiveWriter) writeEntry(path string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	rel, err := filepath.Rel(w.root, path)
	if err != nil || rel == "." {
		return err
	}
	info, err := entry.Info()
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return w.writeSymlink(path, rel, info)
	}
	if err := w.writeHeader(rel, info, ""); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	return w.writeFile(path, rel)
}

func (w archiveWriter) writeSymlink(path, rel string, info fs.FileInfo) error {
	target, err := os.Readlink(path)
	if err != nil {
		return fmt.Errorf("blob: archive read symlink %q: %w", rel, err)
	}
	// Tooling may create disposable absolute links outside the workspace. Skip
	// links that cannot safely be restored; never follow them across the trusted
	// checkpointer boundary.
	if err := validateArchiveSymlink(rel, target); err != nil {
		return nil
	}
	return w.writeHeader(rel, info, target)
}

func (w archiveWriter) writeHeader(rel string, info fs.FileInfo, link string) error {
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("blob: archive header for %q: %w", rel, err)
	}
	hdr.Name = filepath.ToSlash(rel)
	if info.IsDir() {
		hdr.Name += "/"
	}
	if err := w.tar.WriteHeader(hdr); err != nil {
		return fmt.Errorf("blob: archive write header for %q: %w", rel, err)
	}
	return nil
}

func (w archiveWriter) writeFile(path, rel string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("blob: archive open %q: %w", rel, err)
	}
	if _, err := io.Copy(w.tar, f); err != nil {
		_ = f.Close()
		return fmt.Errorf("blob: archive copy %q: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("blob: archive close %q: %w", rel, err)
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
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("blob: open archive destination: %w", err)
	}
	defer root.Close()

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("blob: unarchive tar read: %w", err)
		}
		if err := extractArchiveEntry(root, hdr, tr); err != nil {
			return err
		}
	}
}

func extractArchiveEntry(root *os.Root, hdr *tar.Header, tr io.Reader) error {
	target, err := resolveArchivePath(hdr.Name)
	if err != nil {
		return err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("blob: unarchive mkdir %q: %w", hdr.Name, err)
		}
	case tar.TypeReg:
		if err := root.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("blob: unarchive mkdir parent of %q: %w", hdr.Name, err)
		}
		return writeArchiveFile(root, target, hdr, tr)
	case tar.TypeSymlink:
		if err := validateArchiveSymlink(target, hdr.Linkname); err != nil {
			return fmt.Errorf("blob: unarchive symlink %q: %w", hdr.Name, err)
		}
		if err := root.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("blob: unarchive mkdir parent of symlink %q: %w", hdr.Name, err)
		}
		if err := root.Symlink(hdr.Linkname, target); err != nil {
			return fmt.Errorf("blob: unarchive create symlink %q: %w", hdr.Name, err)
		}
	default:
		// Hard links, devices, and other special entries are not valid
		// workspace content. Ignore them without creating anything.
	}
	return nil
}

// validateArchiveSymlink accepts only relative links whose cleaned destination
// remains inside the extraction root. os.Root is the load-bearing boundary: it
// also rejects a later archive entry that tries to traverse through a symlink.
func validateArchiveSymlink(linkPath, linkTarget string) error {
	if filepath.IsAbs(linkTarget) {
		return fmt.Errorf("absolute target %q is not allowed", linkTarget)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), filepath.FromSlash(linkTarget)))
	if !filepath.IsLocal(resolved) {
		return fmt.Errorf("target %q escapes destination", linkTarget)
	}
	return nil
}

func writeArchiveFile(root *os.Root, target string, hdr *tar.Header, r io.Reader) error {
	f, err := root.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, hdr.FileInfo().Mode().Perm())
	if err != nil {
		return fmt.Errorf("blob: unarchive create %q: %w", hdr.Name, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("blob: unarchive write %q: %w", hdr.Name, err)
	}
	return f.Close()
}

// resolveArchivePath converts a portable tar name into a local relative path.
// The caller then uses it only with os.Root, which enforces the same boundary
// even when an earlier entry created a symlink in the destination tree.
func resolveArchivePath(name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("blob: archive entry %q escapes destination", name)
	}
	return clean, nil
}
