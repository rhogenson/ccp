package wfs

import (
	"io"
	"io/fs"
	"path"
)

type ReadLinkFS interface {
	fs.FS

	ReadLink(string) (string, error)
	Lstat(string) (fs.FileInfo, error)
}

func ReadLink(fsys fs.FS, name string) (string, error) {
	sym, ok := fsys.(ReadLinkFS)
	if !ok {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
	}
	return sym.ReadLink(name)
}

func Lstat(fsys fs.FS, name string) (fs.FileInfo, error) {
	sym, ok := fsys.(ReadLinkFS)
	if !ok {
		return fs.Stat(fsys, name)
	}
	return sym.Lstat(name)
}

type FS interface {
	fs.FS

	Create(string, fs.FileMode) (io.WriteCloser, error)
	Remove(string) error
	Mkdir(string) error
	Symlink(string, string) error
	Chmod(string, fs.FileMode) error
}

type MkdirModeFS interface {
	FS

	MkdirMode(string, fs.FileMode) error
}

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
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return err
	}
	for _, f := range entries {
		name := path.Join(dir, f.Name())
		if f.IsDir() {
			err = removeDir(fsys, name)
		} else {
			err = fsys.Remove(name)
		}
		if err != nil {
			return err
		}
	}
	return fsys.Remove(dir)
}

func RemoveAll(fsys FS, path string) error {
	stat, err := Lstat(fsys, path)
	if err != nil {
		return err
	}
	if stat.IsDir() {
		return removeDir(fsys, path)
	}
	return fsys.Remove(path)
}
