package cp

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

type Progress interface {
	Max(int64)
	Progress(int64)
	FileStart(string)
	FileDone(string, error)
}

func size(files []string) int64 {
	var n int64 = 0
	for _, f := range files {
		filepath.WalkDir(f, func(path string, d fs.DirEntry, err error) error {
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
			case os.ModeSymlink, os.ModeDir:
				n++
			}
			return nil
		})
	}
	return n
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, os.ErrNotExist)
}

func copyRegularFile(src, dst string, force bool, progress Progress) (err error) {
	progress.FileStart(src)

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	stat, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, stat.Mode().Perm())
	if err != nil {
		if !force || !fileExists(dst) {
			return err
		}
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		out, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, stat.Mode().Perm())
		if err != nil {
			return err
		}
	}
	defer out.Close()
	for {
		n, err := io.CopyN(out, in, 1024*1024)
		if n > 0 {
			progress.Progress(n)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	progress.Progress(1)
	return nil
}

func copySpecialFile(src string, d fs.DirEntry, dst string, force bool, progress Progress) error {
	switch d.Type() {
	case fs.ModeSymlink:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			if !force || !fileExists(dst) {
				return err
			}
			if err := os.RemoveAll(dst); err != nil {
				return err
			}
			if err := os.Symlink(target, dst); err != nil {
				return err
			}
		}
		progress.Progress(1)
	case fs.ModeDir:
		stat, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.Mkdir(dst, stat.Mode().Perm()); err != nil {
			if !force || !fileExists(dst) {
				return err
			}
			if err := os.RemoveAll(dst); err != nil {
				return err
			}
			if err := os.Mkdir(dst, stat.Mode().Perm()); err != nil {
				return err
			}
		}
		progress.Progress(1)
	default:
		return fmt.Errorf("%s: unknown file type %s", src, d.Type())
	}
	return nil
}

func Copy(srcs []string, dstRoot string, force bool, progress Progress) {
	go func() {
		progress.Max(size(srcs))
	}()
	const maxConcurrency = 10
	// sem acts as a semaphore to limit the number of concurrent file copies
	sem := make(chan struct{}, maxConcurrency)
	for _, srcRoot := range srcs {
		filepath.WalkDir(srcRoot, func(src string, d fs.DirEntry, err error) error {
			if err != nil {
				progress.FileDone(src, err)
				return nil
			}
			relPath, err := filepath.Rel(srcRoot, src)
			if err != nil {
				progress.FileDone(src, err)
				return nil
			}
			dst := filepath.Join(dstRoot, relPath)
			if d.Type().IsRegular() {
				sem <- struct{}{}
				go func() {
					defer func() { <-sem }()
					err := copyRegularFile(src, dst, force, progress)
					progress.FileDone(src, err)
				}()
			} else {
				if err := copySpecialFile(src, d, dst, force, progress); err != nil {
					progress.FileDone(src, err)
					return nil
				}
			}
			return nil
		})
	}
	for range maxConcurrency {
		sem <- struct{}{}
	}
}
