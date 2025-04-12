// Package cp implements a concurrent file copy over the abstract [wfs.FS]
// interface. It reports progress and errors using the [Progress] interface.
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

// Progress is used to asynchronously report status updates and errors to the
// main program.
type Progress interface {
	// Max sets the total number of bytes to be copied. It's expected that
	// this will only be called once in the program lifetime.
	Max(int64)
	// Progress reports that n additional bytes have been copied.
	Progress(n int64)
	// FileStart reports that src is currently being copied to dst. Only
	// called for regular files, not directories or symlinks.
	FileStart(src, dst string)
	// FileDone is called when a regular file has finished copying
	// successfully, or when there was an error copying a file.
	FileDone(src string, err error)
}

// An FSPath is an abstraction over a file path that can point to multiple
// different backing filesystems.
type FSPath struct {
	// FS is the backing file system where Path is valid.
	FS   wfs.FS
	Path string
}

func (p FSPath) String() string {
	if fsys, ok := p.FS.(*sftpfs.FS); ok {
		return fsys.User + "@" + fsys.Host + ":" + p.Path
	}
	return p.Path
}

// These helper functions are useful to prevent mismatches between filesystem
// and path. For example it's too easy to write
//
//  src.FS.Open(dst.Path)

func (p FSPath) walkDir(fn fs.WalkDirFunc) error {
	return fs.WalkDir(p.FS, p.Path, fn)
}

func (p FSPath) stat() (fs.FileInfo, error) {
	return fs.Stat(p.FS, p.Path)
}

func (p FSPath) lstat() (fs.FileInfo, error) {
	return wfs.Lstat(p.FS, p.Path)
}

func (p FSPath) removeAll() error {
	return wfs.RemoveAll(p.FS, p.Path)
}

func (p FSPath) open() (fs.File, error) {
	return p.FS.Open(p.Path)
}

func (p FSPath) create(mode fs.FileMode) (io.WriteCloser, error) {
	return p.FS.Create(p.Path, mode)
}

func (p FSPath) readLink() (string, error) {
	return wfs.ReadLink(p.FS, p.Path)
}

func (p FSPath) symlinkFrom(target string) error {
	return p.FS.Symlink(target, p.Path)
}

func (p FSPath) mkdir() error {
	return p.FS.Mkdir(p.Path)
}

func (p FSPath) mkdirMode(mode fs.FileMode) error {
	return wfs.MkdirMode(p.FS, p.Path, mode)
}

func (p FSPath) chmod(mode fs.FileMode) error {
	return p.FS.Chmod(p.Path, mode)
}

func size(srcs []FSPath) int64 {
	var n int64 = 0
	for _, src := range srcs {
		src.walkDir(func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			switch d.Type() {
			case 0: // regular file
				stat, err := d.Info()
				if err != nil {
					return nil
				}
				// The "+ 1" is a fudge factor to make sure that
				// the total number of bytes won't be zero.
				n += stat.Size() + 1
			case fs.ModeSymlink, fs.ModeDir:
				n++
			}
			return nil
		})
	}
	return n
}

func (p FSPath) exists() bool {
	_, err := p.lstat()
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
	if err := path.removeAll(); err != nil {
		return err
	}
	return fn()
}

func (c *copier) copyRegularFile(src, dst FSPath) error {
	c.p.FileStart(src.String(), dst.String())

	in, err := src.open()
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
		out, err = dst.create(stat.Mode().Perm())
		return err
	}); err != nil {
		return err
	}
	for {
		// io.CopyN will use cool stuff like copy_file_range as long as
		// the underlying types are *os.File
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
	target, err := src.readLink()
	if err != nil {
		return err
	}
	if err := c.openWithRetry(dst, func() error {
		return dst.symlinkFrom(target)
	}); err != nil {
		return err
	}
	c.p.Progress(1)
	return nil
}

// Copy copies srcs into dstRoot, reporting progress using the [Progress]
// interface. If force is specified and an existing destination file cannot be
// opened, Copy will remove it and try again.
func Copy(progress Progress, srcs []FSPath, dstRoot FSPath, force bool) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		progress.Max(size(srcs))
	}()
	defer func() { <-done }()

	dstIsDir := true
	if len(srcs) == 1 {
		stat, err := dstRoot.stat()
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
	dstRoot.Path = path.Clean(dstRoot.Path)
	for _, srcRoot := range srcs {
		dstRoot := dstRoot
		if dstIsDir {
			// If the destination is a directory, copy into the
			// existing directory.
			dstRoot.Path = path.Join(dstRoot.Path, path.Base(srcRoot.Path))
		}
		srcRoot.Path = path.Clean(srcRoot.Path)
		if srcRoot == dstRoot {
			progress.FileDone(srcRoot.String(), fmt.Errorf("%q and %q are the same file", srcRoot, dstRoot))
			continue
		}
		srcRoot.walkDir(func(srcPath string, d fs.DirEntry, err error) error {
			src := FSPath{srcRoot.FS, srcPath}
			dst := FSPath{dstRoot.FS, path.Join(dstRoot.Path, strings.TrimPrefix(srcPath, srcRoot.Path))}
			if err != nil {
				progress.FileDone(src.String(), err)
				return nil
			}
			switch d.Type() {
			case 0: // regular file
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
					return fs.SkipDir
				}
				hasWritePerm := stat.Mode()&0300 == 0300
				if err := c.openWithRetry(dst, func() error {
					if hasWritePerm {
						return dst.mkdirMode(stat.Mode().Perm())
					} else {
						// If a directory doesn't have
						// write permissions, we won't
						// be able to create any files
						// inside of it if we create it
						// with the correct permissions
						// now. So instead create it
						// with some default
						// permissions, and append it to
						// roDirs to be processed later.
						return dst.mkdir()
					}
				}); err != nil {
					progress.FileDone(src.String(), err)
					return fs.SkipDir
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
	// Wait for all jobs to complete.
	for range maxConcurrency {
		sem <- struct{}{}
	}
	// Iterate backwards so that directory contents are processed before the
	// parent directory itself.
	for _, d := range slices.Backward(roDirs) {
		if err := d.path.chmod(d.mode); err != nil {
			progress.FileDone(d.path.String(), err)
			continue
		}
		progress.Progress(1)
	}
}
