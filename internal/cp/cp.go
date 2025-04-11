package cp

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"

	"gitlab.com/rhogenson/ccp/internal/wfs"
	"gitlab.com/rhogenson/ccp/internal/wfs/sftpfs"
)

type Progress interface {
	Max(int64)
	Progress(int64)
	FileStart(string, string)
	FileDone(string, error)
}

type FSPath struct {
	FS   wfs.FS
	Path string
}

func (p FSPath) String() string {
	if fsys, ok := p.FS.(*sftpfs.FS); ok {
		return fsys.User + "@" + fsys.Host + ":" + p.Path
	}
	return p.Path
}

func (p FSPath) WalkDir(fn fs.WalkDirFunc) error {
	return fs.WalkDir(p.FS, p.Path, fn)
}

func (p FSPath) Stat() (fs.FileInfo, error) {
	return fs.Stat(p.FS, p.Path)
}

func (p FSPath) Lstat() (fs.FileInfo, error) {
	return wfs.Lstat(p.FS, p.Path)
}

func (p FSPath) RemoveAll() error {
	return wfs.RemoveAll(p.FS, p.Path)
}

func (p FSPath) Open() (fs.File, error) {
	return p.FS.Open(p.Path)
}

func (p FSPath) Create(mode fs.FileMode) (io.WriteCloser, error) {
	return p.FS.Create(p.Path, mode)
}

func (p FSPath) ReadLink() (string, error) {
	return wfs.ReadLink(p.FS, p.Path)
}

func (p FSPath) SymlinkFrom(target string) error {
	return p.FS.Symlink(target, p.Path)
}

func (p FSPath) Mkdir() error {
	return p.FS.Mkdir(p.Path)
}

func (p FSPath) MkdirMode(mode fs.FileMode) error {
	return wfs.MkdirMode(p.FS, p.Path, mode)
}

func (p FSPath) Chmod(mode fs.FileMode) error {
	return p.FS.Chmod(p.Path, mode)
}

func size(srcs []FSPath) int64 {
	var n int64 = 0
	for _, src := range srcs {
		src.WalkDir(func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			switch d.Type() {
			case 0: // regular file
				stat, err := d.Info()
				if err != nil {
					return nil
				}
				n += 1 + stat.Size()
			case fs.ModeSymlink, fs.ModeDir:
				n++
			}
			return nil
		})
	}
	return n
}

func (p FSPath) exists() bool {
	_, err := p.Lstat()
	return !errors.Is(err, fs.ErrNotExist)
}

type copier struct {
	p     Progress
	force bool
}

func (c *copier) openWithRetry(path FSPath, fn func() error) error {
	if err := fn(); err == nil || !c.force || !path.exists() {
		return err
	}
	if err := path.RemoveAll(); err != nil {
		return err
	}
	return fn()
}

func (c *copier) copyRegularFile(src, dst FSPath) error {
	c.p.FileStart(src.String(), dst.String())

	in, err := src.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	stat, err := in.Stat()
	if err != nil {
		return err
	}
	var out io.WriteCloser
	if err := c.openWithRetry(dst, func() error {
		var err error
		out, err = dst.Create(stat.Mode().Perm())
		return err
	}); err != nil {
		return err
	}
	for {
		n, err := io.CopyN(out, in, 1024*1024)
		if n > 0 {
			c.p.Progress(n)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	c.p.Progress(1)
	c.p.FileDone(src.String(), nil)
	return nil
}

func (c *copier) copySymlink(src FSPath, dst FSPath) error {
	target, err := src.ReadLink()
	if err != nil {
		return err
	}
	if err := c.openWithRetry(dst, func() error {
		return dst.SymlinkFrom(target)
	}); err != nil {
		return err
	}
	c.p.Progress(1)
	return nil
}

func Copy(progress Progress, srcs []FSPath, dstRoot FSPath, force bool) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		progress.Max(size(srcs))
	}()
	defer func() { <-done }()

	dstIsDir := true
	if len(srcs) == 1 {
		stat, err := dstRoot.Stat()
		dstIsDir = err == nil && stat.IsDir()
	}

	const maxConcurrency = 10
	// sem acts as a semaphore to limit the number of concurrent file copies
	sem := make(chan struct{}, maxConcurrency)
	c := &copier{
		p:     progress,
		force: force,
	}
	type roDir struct {
		path FSPath
		mode fs.FileMode
	}
	var roDirs []roDir
	for _, srcRoot := range srcs {
		dstRoot := dstRoot
		if dstIsDir {
			dstRoot.Path = path.Join(dstRoot.Path, path.Base(srcRoot.Path))
		}
		srcRoot.Path = path.Clean(srcRoot.Path)
		srcRoot.WalkDir(func(srcPath string, d fs.DirEntry, err error) error {
			src := FSPath{srcRoot.FS, srcPath}
			dst := FSPath{dstRoot.FS, path.Join(dstRoot.Path, strings.TrimPrefix(srcPath, srcRoot.Path))}
			if err != nil {
				progress.FileDone(src.String(), err)
				return nil
			}
			switch d.Type() {
			case 0:
				sem <- struct{}{}
				go func() {
					defer func() { <-sem }()
					if err := c.copyRegularFile(src, dst); err != nil {
						progress.FileDone(src.String(), err)
					}
				}()
			case fs.ModeDir:
				stat, err := d.Info()
				if err != nil {
					progress.FileDone(src.String(), err)
					return nil
				}
				hasWritePerm := stat.Mode()&0300 == 0300
				if err := c.openWithRetry(dst, func() error {
					if hasWritePerm {
						return dst.MkdirMode(stat.Mode().Perm())
					} else {
						return dst.Mkdir()
					}
				}); err != nil {
					progress.FileDone(src.String(), err)
					return nil
				}
				if hasWritePerm {
					progress.Progress(1)
				} else {
					roDirs = append(roDirs, roDir{dst, stat.Mode().Perm()})
				}
			case fs.ModeSymlink:
				if err := c.copySymlink(src, dst); err != nil {
					progress.FileDone(src.String(), err)
				}
			default:
				progress.FileDone(src.String(), fmt.Errorf("%s: unknown file type %s", src, d.Type()))
			}
			return nil
		})
	}
	for range maxConcurrency {
		sem <- struct{}{}
	}
	for _, d := range slices.Backward(roDirs) {
		if err := d.path.Chmod(d.mode); err != nil {
			progress.FileDone(d.path.String(), err)
			continue
		}
		progress.Progress(1)
	}
}
