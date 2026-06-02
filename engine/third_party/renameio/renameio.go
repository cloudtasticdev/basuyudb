// Package renameio is a small cross-platform drop-in shim for
// github.com/google/renameio v1, whose upstream TempFile is //go:build !windows
// and therefore breaks Windows builds of dependents (e.g. coder/hnsw). BasuyuDB
// ships as a Linux distroless binary; this shim simply lets the same code build
// and test on Windows/macOS dev machines too. It provides the exported surface
// dependents use: TempFile, PendingFile (+Cleanup/CloseAtomicallyReplace),
// WriteFile, TempDir, Symlink. Atomicity is best-effort via os.Rename, which is
// sufficient for the snapshot-file path that uses it.
package renameio

import (
	"os"
	"path/filepath"
)

// TempDir returns a directory suitable for creating a temp file that can be
// atomically renamed to dest.
func TempDir(dest string) string {
	dir := filepath.Dir(dest)
	if dir == "" {
		dir = os.TempDir()
	}
	return dir
}

// PendingFile is a temporary file that can be atomically renamed into place.
type PendingFile struct {
	*os.File
	path string
	done bool
}

// TempFile creates a temp file in dir (or alongside path if dir is empty) that
// will be atomically renamed to path on CloseAtomicallyReplace.
func TempFile(dir, path string) (*PendingFile, error) {
	if dir == "" {
		dir = TempDir(path)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".tmp-renameio-*")
	if err != nil {
		return nil, err
	}
	return &PendingFile{File: f, path: path}, nil
}

// Cleanup closes and removes the temp file if it was not already renamed.
func (t *PendingFile) Cleanup() error {
	if t.done {
		return nil
	}
	name := t.File.Name()
	_ = t.File.Close()
	return os.Remove(name)
}

// CloseAtomicallyReplace flushes, closes, and renames the temp file to its
// destination path.
func (t *PendingFile) CloseAtomicallyReplace() error {
	if err := t.File.Sync(); err != nil {
		_ = t.File.Close()
		return err
	}
	name := t.File.Name()
	if err := t.File.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, t.path); err != nil {
		// On Windows, Rename fails if the destination exists; replace it.
		if removeErr := os.Remove(t.path); removeErr == nil {
			if err2 := os.Rename(name, t.path); err2 != nil {
				return err2
			}
		} else {
			return err
		}
	}
	t.done = true
	return nil
}

// WriteFile atomically writes data to filename.
func WriteFile(filename string, data []byte, perm os.FileMode) error {
	t, err := TempFile("", filename)
	if err != nil {
		return err
	}
	defer t.Cleanup()
	if _, err := t.Write(data); err != nil {
		return err
	}
	if err := t.Chmod(perm); err != nil {
		return err
	}
	return t.CloseAtomicallyReplace()
}

// Symlink atomically creates or replaces a symlink.
func Symlink(oldname, newname string) error {
	tmp := newname + ".tmp-renameio-link"
	_ = os.Remove(tmp)
	if err := os.Symlink(oldname, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, newname)
}
