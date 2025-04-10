package cp

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sync"

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

func size(srcs []FSPath) int64 {
	var n int64 = 0
	for _, src := range srcs {
		fs.WalkDir(src.FS, src.Path, func(_ string, d fs.DirEntry, err error) error {
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

func fileExists(path FSPath) bool {
	_, err := wfs.Lstat(path.FS, path.Path)
	return !errors.Is(err, fs.ErrNotExist)
}

type copier struct {
	p   Progress
	sem chan struct{}

	force bool
}

func (c *copier) g(fn func()) {
	c.sem <- struct{}{}
	go func() {
		defer func() { <-c.sem }()
		fn()
	}()
}

func (c *copier) openWithRetry(path FSPath, fn func() error) error {
	if err := fn(); err == nil || !c.force || !fileExists(path) {
		return err
	}
	if err := wfs.RemoveAll(path.FS, path.Path); err != nil {
		return err
	}
	return fn()
}

func (c *copier) copyRegularFile(src, dst FSPath) error {
	c.p.FileStart(src.String(), dst.String())

	in, err := src.FS.Open(src.Path)
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
		out, err = dst.FS.Create(dst.Path, stat.Mode().Perm())
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

func (c *copier) copySymlink(src FSPath, d fs.DirEntry, dst FSPath) error {
	target, err := wfs.ReadLink(src.FS, src.Path)
	if err != nil {
		return err
	}
	if err := c.openWithRetry(dst, func() error {
		return dst.FS.Symlink(target, dst.Path)
	}); err != nil {
		return err
	}
	c.p.Progress(1)
	return nil
}

func (c *copier) copyDir(src FSPath, d fs.DirEntry, dst FSPath) error {
	stat, err := d.Info()
	if err != nil {
		return err
	}
	hasWritePerm := stat.Mode()&0300 == 0300
	if err := c.openWithRetry(dst, func() error {
		if hasWritePerm {
			return wfs.MkdirMode(dst.FS, dst.Path, stat.Mode().Perm())
		} else {
			return dst.FS.Mkdir(dst.Path)
		}
	}); err != nil {
		return err
	}
	entries, err := fs.ReadDir(src.FS, src.Path)
	wg := new(sync.WaitGroup)
	<-c.sem
	for _, entry := range entries {
		wg.Add(1)
		c.g(func() {
			defer wg.Done()
			src := FSPath{src.FS, path.Join(src.Path, entry.Name())}
			if err := c.copyFile(src, entry, FSPath{dst.FS, path.Join(dst.Path, entry.Name())}); err != nil {
				c.p.FileDone(src.String(), err)
			}
		})
	}
	wg.Wait()
	c.sem <- struct{}{}
	if err != nil {
		return err
	}
	if !hasWritePerm {
		if err := dst.FS.Chmod(dst.Path, stat.Mode().Perm()); err != nil {
			return err
		}
	}
	c.p.Progress(1)
	return nil
}

func (c *copier) copyFile(src FSPath, d fs.DirEntry, dst FSPath) error {
	switch d.Type() {
	case 0: // regular file
		return c.copyRegularFile(src, dst)
	case fs.ModeDir:
		return c.copyDir(src, d, dst)
	case fs.ModeSymlink:
		return c.copySymlink(src, d, dst)
	default:
		return fmt.Errorf("%s: unknown file type %s", src, d.Type())
	}
}

func Copy(progress Progress, srcs []FSPath, dstRoot FSPath, force bool) {
	go func() {
		progress.Max(size(srcs))
	}()

	dstIsDir := true
	if len(srcs) == 1 {
		stat, err := fs.Stat(dstRoot.FS, dstRoot.Path)
		dstIsDir = err == nil && stat.IsDir()
	}

	const maxConcurrency = 10
	// sem acts as a semaphore to limit the number of concurrent file copies
	sem := make(chan struct{}, maxConcurrency)
	c := &copier{
		p:     progress,
		sem:   sem,
		force: force,
	}
	wg := new(sync.WaitGroup)
	for _, srcRoot := range srcs {
		wg.Add(1)
		c.g(func() {
			defer wg.Done()
			dstRoot := dstRoot
			if dstIsDir {
				dstRoot.Path = path.Join(dstRoot.Path, path.Base(srcRoot.Path))
			}
			stat, err := fs.Stat(srcRoot.FS, srcRoot.Path)
			if err != nil {
				progress.FileDone(srcRoot.String(), err)
				return
			}
			if err := c.copyFile(srcRoot, fs.FileInfoToDirEntry(stat), dstRoot); err != nil {
				progress.FileDone(srcRoot.String(), err)
				return
			}
		})
	}
	wg.Wait()
}
