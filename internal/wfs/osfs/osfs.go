package osfs

import (
	"io"
	"io/fs"
	"os"

	"gitlab.com/rhogenson/ccp/internal/wfs"
)

var (
	_ wfs.FS          = FS{}
	_ wfs.MkdirModeFS = FS{}
	_ wfs.ReadLinkFS  = FS{}
	_ fs.StatFS       = FS{}
)

type FS struct{}

func (FS) Open(name string) (fs.File, error) {
	return os.Open(name)
}

func (FS) Stat(name string) (fs.FileInfo, error) {
	return os.Stat(name)
}

func (FS) Lstat(name string) (fs.FileInfo, error) {
	return os.Lstat(name)
}

func (FS) ReadLink(name string) (string, error) {
	return os.Readlink(name)
}

func (FS) Create(name string, perm fs.FileMode) (io.WriteCloser, error) {
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
}

func (FS) Remove(name string) error {
	return os.Remove(name)
}

func (FS) Mkdir(name string) error {
	return os.Mkdir(name, 0700)
}

func (FS) MkdirMode(name string, mode fs.FileMode) error {
	return os.Mkdir(name, mode)
}

func (FS) Symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}

func (FS) Chmod(name string, mode fs.FileMode) error {
	return os.Chmod(name, mode)
}
