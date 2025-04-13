// The ccp ("cute copy") command copies files and directories while showing a
// colorful progress bar. It supports SFTP remote file copies similar to scp.
//
// The architecture is a mix of classical goroutines and the bubbletea-style
// "Elm architecture". Trying to do a recursive concurrent file copy using the
// Elm architecture would make Update a massive bottleneck, so that part is
// performed in a background goroutine that periodically sends updates to the
// main program using the [cp.Progress] interface.
//
// The Elm architecture doesn't seem to fit well with Go's concurrency model in
// my opinion. You even have articles like
// https://charm.sh/blog/commands-in-bubbletea/ saying that you should "never"
// use goroutines in a Bubble Tea program, which IMO is just absurd and throwing
// out one of the best parts of Go. Ideally a UI library would leverage the
// strengths of Go's concurrency model instead of trying to force some
// architecture from a different language. For example, [tea.Tick] is
// inconvenient because the user has to remember to call Tick again inside
// Update, otherwise it only runs once. Instead it could have just leveraged the
// standard library [time.Ticker] with
//
//	go func() {
//	    for t := range time.NewTicker(time.Second).C {
//	        program.Send(tickMsg(t))
//	    }
//	}()
//
// It's too limiting that a [tea.Cmd] can only return a single [tea.Msg].
// Instead, in the true spirit of Go's CSP model, a tea.Cmd should be able to
// send multiple messages on a channel. Thanks for reading my rant.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/rhogenson/container/deque"
	"github.com/rhogenson/ccp/internal/cp"
	"github.com/rhogenson/ccp/internal/wfs/osfs"
	"github.com/rhogenson/ccp/internal/wfs/sftpfs"
)

var f = flag.Bool("f", false, "if an existing destination file cannot be opened, remove it and try again")

type measurement struct {
	t time.Time
	i int64
}

type model struct {
	progress progress.Model

	// max is the total bytes (plus fudge factor) to copy.
	max int64
	// current holds the current number of copied bytes.
	current atomic.Int64
	// Every 500 milliseconds, the current progress is appended to
	// measurements for calculating ETA.
	measurements deque.Deque[measurement]
	// copyingFiles holds the files currently being copied. Keys are source
	// paths and values are the corresponding destination paths.
	copyingFiles map[string]string
	// copyingFile is an arbitrary entry from copyingFiles that we're
	// currently showing to the user. Tracked in the state so that it
	// doesn't change every time we update the view.
	copyingFile string
	// eta is the estimated time to completion, or -1 if we don't have
	// enough samples.
	eta time.Duration
	// errs are the errors encountered during operation.
	errs []string
	// done indicates whether the copy is done and we're just waiting for
	// the progress bar to finish animating.
	done bool
}

type (
	// tickMsg is sent every 500 milliseconds.
	tickMsg time.Time

	// maxMsg sets the total bytes to copy. This message is only sent once
	// during the program lifetime after we asynchronously calculate the
	// number of bytes to copy.
	maxMsg int64
	// fileStartMsg is sent whenever we start copying a file.
	fileStartMsg struct {
		from, to string
	}
	// fileDoneMsg is sent whenever we finish copying a file. err indicates
	// any error that was encountered during the copy.
	fileDoneMsg struct {
		name string
		err  error
	}
	// doneMsg is sent when all files are finished copying and it's time
	// to exit.
	doneMsg struct{}
)

func tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Init() tea.Cmd {
	return tick()
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case maxMsg:
		m.max = int64(msg)
	case fileStartMsg:
		m.copyingFiles[msg.from] = msg.to
		if m.copyingFile == "" {
			m.copyingFile = msg.from
		}
	case fileDoneMsg:
		delete(m.copyingFiles, msg.name)
		if m.copyingFile == msg.name {
			m.copyingFile = ""
			for name := range m.copyingFiles {
				m.copyingFile = name
				break
			}
		}
		if msg.err != nil {
			m.errs = append(m.errs, msg.err.Error())
		}
	case doneMsg:
		m.done = true
		var cmd tea.Cmd
		if m.max > 0 {
			cmd = m.progress.SetPercent(float64(m.current.Load()) / float64(m.max))
		}
		if !m.progress.IsAnimating() {
			return m, tea.Quit
		}
		return m, cmd

	case tickMsg:
		n := m.current.Load()
		now := time.Time(msg)

		if m.measurements.Len() == 0 || now.Sub(m.measurements.At(m.measurements.Len()-1).t) > 500*time.Millisecond {
			for m.measurements.Len() > 1 && now.Sub(m.measurements.At(0).t) > 2*time.Minute {
				m.measurements.PopFront()
			}
			m.measurements.PushBack(measurement{now, n})

			if m.max > 0 {
				first := m.measurements.At(0)
				if delta := n - first.i; delta != 0 {
					deltaT := now.Sub(first.t)
					m.eta = time.Duration(float64(m.max-n) / float64(delta) * float64(deltaT))
				}
			}
		}

		cmds := []tea.Cmd{tick()}
		if m.max > 0 {
			cmds = append(cmds, m.progress.SetPercent(float64(n)/float64(m.max)))
		}
		return m, tea.Batch(cmds...)

	// FrameMsg is sent when the progress bar wants to animate itself
	case progress.FrameMsg:
		progressModel, cmd := m.progress.Update(msg)
		m.progress = progressModel.(progress.Model)
		if m.done && !m.progress.IsAnimating() {
			return m, tea.Quit
		}
		return m, cmd
	case tea.WindowSizeMsg:
		m.progress.Width = msg.Width - 4
	}
	return m, nil
}

var warningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render

func (m *model) View() string {
	copying := ""
	if m.copyingFile != "" {
		copying = m.copyingFile + " -> " + m.copyingFiles[m.copyingFile]
	}
	etaStr := "calculating..."
	if m.eta >= 0 {
		etaStr = m.eta.Round(time.Second).String()
	}
	return "\n" +
		"  " + copying + "\n" +
		"  " + m.progress.View() + "\n" +
		"  " + "ETA: " + etaStr + "\n\n" +
		warningStyle(strings.Join(m.errs, "\n")) + "\n"
}

// progressUpdater implements the cp.Progress interface.
type progressUpdater struct {
	p       *tea.Program
	current *atomic.Int64
}

func (pu *progressUpdater) Max(n int64) {
	pu.p.Send(maxMsg(n))
}

func (pu *progressUpdater) Progress(n int64) {
	pu.current.Add(n)
}

func (pu *progressUpdater) FileStart(from, to string) {
	pu.p.Send(fileStartMsg{from, to})
}

func (pu *progressUpdater) FileDone(name string, err error) {
	pu.p.Send(fileDoneMsg{name, err})
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
	m := &model{
		progress:     progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage()),
		copyingFiles: make(map[string]string),
		eta:          -1,
	}
	p := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(os.Stderr))
	go func() {
		cp.Copy(&progressUpdater{p, &m.current}, srcs, dst, *f) // Where the magic happens
		p.Send(doneMsg{})
	}()
	if _, err := p.Run(); err != nil {
		return err
	}
	if len(m.errs) > 0 {
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
