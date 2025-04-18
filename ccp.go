// The ccp ("cute copy") command copies files and directories while showing a
// colorful progress bar. It supports SFTP remote file copies similar to scp.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/lipgloss"
	"github.com/rhogenson/ccp/internal/cp"
	"github.com/rhogenson/ccp/internal/render"
	"github.com/rhogenson/ccp/internal/wfs/osfs"
	"github.com/rhogenson/ccp/internal/wfs/sftpfs"
	"github.com/rhogenson/deque"
	"golang.org/x/term"
)

var f = flag.Bool("f", false, "if an existing destination file cannot be opened, remove it and try again")

var warningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render

type measurement struct {
	t time.Time
	i int64
}

// progressUpdater implements the cp.Progress interface.
type progressUpdater struct {
	mu          sync.Mutex
	max         int64  // Total bytes to copy
	current     int64  // Current bytes copied
	copyingFrom string // File currently being copied
	copyingTo   string
	errs        []error // Any errors encountered
}

func (pu *progressUpdater) Max(n int64) {
	pu.mu.Lock()
	defer pu.mu.Unlock()
	pu.max = n
}

func (pu *progressUpdater) Progress(n int64) {
	pu.mu.Lock()
	defer pu.mu.Unlock()
	pu.current += n
}

func abbreviatePath(p string) string {
	parts := strings.Split(p, string(filepath.Separator))
	for i := 1; i < len(parts)-1; i++ {
		part := parts[i]
		_, n := utf8.DecodeRuneInString(part)
		parts[i] = part[:n]
	}
	return strings.Join(parts, string(filepath.Separator))
}

func (pu *progressUpdater) FileStart(from, to string) {
	pu.mu.Lock()
	defer pu.mu.Unlock()
	pu.copyingFrom = from
	pu.copyingTo = to
}

func (pu *progressUpdater) Error(err error) {
	pu.mu.Lock()
	defer pu.mu.Unlock()
	pu.errs = append(pu.errs, err)
}

// splitHostPath splits an scp target into host and path, e.g. user@host:/path/
// If the user wants to copy a local file that has a colon in it, they can
// qualify it with the directory name, e.g. ./file:with:colons.
func splitHostPath(target string) (string, string) {
	i := strings.IndexAny(target, ":/")
	if i < 0 || target[i] == '/' {
		return "", target
	}
	return target[:i], target[i+1:]
}

func toFSPath(target string, sftpHosts map[string]*sftpfs.FS) cp.FSPath {
	host, path := splitHostPath(target)
	if host == "" {
		return cp.FSPath{FS: osfs.FS{}, Path: path}
	}
	if path == "" {
		path = "."
	}
	return cp.FSPath{FS: sftpHosts[host], Path: path}
}

func run() error {
	args := flag.Args()
	if len(args) < 2 {
		return errors.New("usage error")
	}
	srcTargets, dstTarget := args[:len(args)-1], args[len(args)-1]
	sftpHosts := make(map[string]*sftpfs.FS)
	for _, tgt := range append(srcTargets, dstTarget) {
		host, _ := splitHostPath(tgt)
		if host == "" || sftpHosts[host] != nil {
			continue
		}
		fs, err := sftpfs.Dial(host)
		if err != nil {
			return err
		}
		defer fs.Close()
		sftpHosts[host] = fs
	}
	srcs := make([]cp.FSPath, len(srcTargets))
	for i, tgt := range srcTargets {
		srcs[i] = toFSPath(tgt, sftpHosts)
	}
	dst := toFSPath(dstTarget, sftpHosts)

	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage())
	doneCh := make(chan struct{})
	measurements := new(deque.Deque[measurement])
	eta := time.Duration(-1)

	currentProgress := new(progressUpdater)
	go func() {
		defer close(doneCh)
		cp.Copy(currentProgress, srcs, dst, *f) // Where the magic happens
	}()

	frameTimer := time.NewTicker(time.Second / 30)
	defer frameTimer.Stop()
	etaTimer := time.NewTicker(500 * time.Millisecond)
	defer etaTimer.Stop()
	done := false
	renderer := render.New()
	for !done {
		select {
		case now := <-etaTimer.C:
			currentProgress.mu.Lock()
			current := currentProgress.current
			max := currentProgress.max
			currentProgress.mu.Unlock()

			for measurements.Len() > 1 && now.Sub(measurements.At(0).t) > 2*time.Minute {
				measurements.PopFront()
			}
			measurements.PushBack(measurement{now, current})

			if max > 0 {
				first := measurements.At(0)
				if delta := current - first.i; delta != 0 {
					deltaT := now.Sub(first.t)
					eta = time.Duration(float64(max-current) / float64(delta) * float64(deltaT))
				}
			}
			continue
		case <-doneCh:
			done = true
		case <-frameTimer.C:
		}

		width, _, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			width = 80
		}
		bar.Width = width - 4

		currentProgress.mu.Lock()
		current := currentProgress.current
		max := currentProgress.max
		copyingFrom := currentProgress.copyingFrom
		copyingTo := currentProgress.copyingTo
		errs := currentProgress.errs
		currentProgress.mu.Unlock()

		renderer.Clear(width)
		copyingFile := ""
		if copyingFrom != "" {
			copyingFile = copyingFrom + " -> " + copyingTo
			if len(copyingFile)+4 > width {
				copyingFile = copyingFrom + " -> " + abbreviatePath(copyingTo)
				if len(copyingFile)+4 > width {
					copyingFile = abbreviatePath(copyingFrom) + " -> " + abbreviatePath(copyingTo)
				}
			}
		}
		progress := 0.
		if max > 0 {
			progress = float64(current) / float64(max)
		}
		etaStr := "..."
		if eta >= 0 {
			etaStr = eta.Round(time.Second).String()
		}
		fmt.Fprintf(renderer, `
  %s
  %s
  ETA: %s

`,
			copyingFile,
			bar.ViewAs(progress),
			etaStr)
		for _, e := range errs {
			fmt.Fprintln(renderer, warningStyle(e.Error()))
		}
		renderer.Flush()
	}
	if len(currentProgress.errs) > 0 {
		return errors.New("exiting with one or more errors")
	}
	return nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: ccp [OPTION]... SOURCE TARGET
  or:  ccp [OPTION]... SOURCE... TARGET

Copy SOURCE to TARGET, or multiple SOURCE(s) to a directory TARGET.
Uses SFTP for remote file copies.

ccp will ask for passwords or passphrases if they are needed
for authentication.

The source and target may be specified as a local pathname or a remote
host with optional path in the form [user@]host:[path]. Local file names
can be made explicit using absolute or relative pathnames to avoid ccp
treating file names containing `+"`"+`:' as host specifiers.

Options:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
