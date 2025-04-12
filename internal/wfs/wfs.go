// Package wfs implements a "writable file system" in the spirit of [fs.FS].
package wfs

import (
	"errors"
	"io"
	"io/fs"
	"path"
)

// ReadLinkFS is backported from the latest go master.
type ReadLinkFS interface {
	fs.FS

	ReadLink(string) (string, error)
	Lstat(string) (fs.FileInfo, error)
}

// ReadLink returns the destination of the named symbolic link.
//
// If fsys does not implement [ReadLinkFS], then ReadLink returns an error.
func ReadLink(fsys fs.FS, name string) (string, error) {
	sym, ok := fsys.(ReadLinkFS)
	if !ok {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
	}
	return sym.ReadLink(name)
}

// Lstat returns an [fs.FileInfo] describing the named file.
// If the file is a symbolic link, the returned [fs.FileInfo] describes the
// symbolic link. Lstat makes no attempt to follow the link.
//
// If fsys does not implement [ReadLinkFS], then Lstat is identical
// to [fs.Stat].
func Lstat(fsys fs.FS, name string) (fs.FileInfo, error) {
	sym, ok := fsys.(ReadLinkFS)
	if !ok {
		return fs.Stat(fsys, name)
	}
	return sym.Lstat(name)
}

// An FS provides access to a writable hierarchical file system.
type FS interface {
	fs.FS

	Create(string, fs.FileMode) (io.WriteCloser, error)
	Remove(string) error
	Mkdir(string) error
	Symlink(string, string) error
	Chmod(string, fs.FileMode) error
}

// A MkdirModeFS is a file system with a mkdir method that accepts a file mode.
type MkdirModeFS interface {
	FS

	MkdirMode(string, fs.FileMode) error
}

// MkdirMode creates a directory with the given file permission. If fsys
// implements [MkdirModeFS], MkdirMode calls fsys.MkdirMode. Otherwise,
// MkdirMode calls Mkdir and then Chmod to set the mode.
func MkdirMode(fsys FS, name string, mode fs.FileMode) error {
	if fsys, ok := fsys.(MkdirModeFS); ok {
		return fsys.MkdirMode(name, mode)
	}
	if err := fsys.Mkdir(name); err != nil {
		return err
	}
	return fsys.Chmod(name, mode)
}

func removeDir(fsys FS, dir string) error {
	entries, readErr := fs.ReadDir(fsys, dir)
	var err error
	for _, d := range entries {
		if err1 := removeAll(fsys, path.Join(dir, d.Name()), d); err == nil {
			err = err1
		}
	}
	if err == nil {
		err = readErr
	}
	err1 := fsys.Remove(dir)
	if err1 == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		err = err1
	}
	return err
}

func removeAll(fsys FS, path string, d fs.DirEntry) error {
	err := fsys.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if !d.IsDir() {
		return err
	}
	return removeDir(fsys, path)
}

// RemoveAll removes path and any children it contains. It removes everything it
// can but returns the first error it encounters. If the path does not exist,
// RemoveAll returns nil (no error).
func RemoveAll(fsys FS, path string) error {
	// Simple case: if Remove works, we're done.
	err := fsys.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	// Otherwise, is this a directory we need to recurse into?
	dir, serr := Lstat(fsys, path)
	if serr != nil {
		if errors.Is(serr, fs.ErrNotExist) {
			return nil
		}
		return serr
	}
	if !dir.IsDir() {
		// Not a directory; return the error from Remove.
		return err
	}
	return removeDir(fsys, path)
}
