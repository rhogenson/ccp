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
	"gitlab.com/rhogenson/ccp/internal/cp"
	"gitlab.com/rhogenson/ccp/internal/wfs/osfs"
	"gitlab.com/rhogenson/ccp/internal/wfs/sftpfs"
	"gitlab.com/rhogenson/deque"
)

var f = flag.Bool("f", false, "if an existing destination file cannot be opened, remove it and try again")

type measurement struct {
	t time.Time
	i int64
}

type model struct {
	progress progress.Model
	msgs     chan tea.Msg

	srcs []cp.FSPath
	dst  cp.FSPath

	max          int64
	current      atomic.Int64
	measurements deque.Deque[measurement]
	copyingFiles map[string]string
	copyingFile  string
	errs         []string
}

type (
	tickMsg time.Time

	maxMsg       int64
	fileStartMsg struct {
		from, to string
	}
	fileDoneMsg struct {
		name string
		err  error
	}
	doneMsg struct{}
)

func (m *model) listen() tea.Cmd {
	return func() tea.Msg {
		return <-m.msgs
	}
}

func tick() tea.Cmd {
	return tea.Tick(10*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			cp.Copy(m, m.srcs, m.dst, *f)
			return doneMsg{}
		},
		m.listen(),
		tick())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case maxMsg:
		m.max = int64(msg)
		return m, m.listen()
	case fileStartMsg:
		m.copyingFiles[msg.from] = msg.to
		if m.copyingFile == "" {
			m.copyingFile = msg.from
		}
		return m, m.listen()
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
		return m, m.listen()
	case doneMsg:
		return m, tea.Sequence(
			m.progress.SetPercent(float64(m.current.Load())/float64(m.max)),
			tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return nil }),
			tea.Quit)

	case tickMsg:
		n := m.current.Load()
		now := time.Time(msg)
		if m.measurements.Len() == 0 || now.Sub(m.measurements.At(m.measurements.Len()-1).t) > 500*time.Millisecond {
			for m.measurements.Len() > 1 && now.Sub(m.measurements.At(0).t) > 2*time.Minute {
				m.measurements.PopFront()
			}
			m.measurements.PushBack(measurement{now, n})
		}
		cmds := []tea.Cmd{tick()}
		if m.max > 0 {
			cmds = append(cmds, m.progress.SetPercent(float64(n)/float64(m.max)))
		}
		return m, tea.Batch(cmds...)

	case tea.WindowSizeMsg:
		m.progress.Width = msg.Width - 4
		return m, nil
	// FrameMsg is sent when the progress bar wants to animate itself
	case progress.FrameMsg:
		progressModel, cmd := m.progress.Update(msg)
		m.progress = progressModel.(progress.Model)
		return m, cmd
	default:
		return m, nil
	}
}

var warningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render

func (m *model) View() string {
	copying := ""
	if m.copyingFile != "" {
		copying = m.copyingFile + " -> " + m.copyingFiles[m.copyingFile]
	}
	etaStr := "calculating..."
	if m.max > 0 && m.measurements.Len() > 1 {
		first := m.measurements.At(0)
		last := m.measurements.At(m.measurements.Len() - 1)
		deltaT := last.t.Sub(first.t)
		delta := last.i - first.i
		if delta != 0 {
			etaStr = time.Duration(float64(m.max-last.i) / float64(delta) * float64(deltaT)).Round(time.Second).String()
		}
	}
	return "\n" +
		"  " + copying + "\n" +
		"  " + m.progress.View() + "\n" +
		"  " + "ETA: " + etaStr + "\n\n" +
		warningStyle(strings.Join(m.errs, "\n")) + "\n"
}

func (m *model) Max(n int64) {
	m.msgs <- maxMsg(n)
}

func (m *model) Progress(n int64) {
	m.current.Add(n)
}

func (m *model) FileStart(from, to string) {
	m.msgs <- fileStartMsg{from, to}
}

func (m *model) FileDone(name string, err error) {
	m.msgs <- fileDoneMsg{name, err}
}

func splitHostPath(target string) (string, string) {
	i := strings.IndexAny(target, ":/")
	if i < 0 || target[i] == '/' {
		return "", target
	}
	return target[:i], target[i+1:]
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
		host, path := splitHostPath(tgt)
		if host == "" {
			srcs[i] = cp.FSPath{FS: osfs.FS{}, Path: path}
		} else {
			srcs[i] = cp.FSPath{FS: sftpHosts[host], Path: path}
		}
	}
	dstHost, dstPath := splitHostPath(dstTarget)
	var dst cp.FSPath
	if dstHost == "" {
		dst = cp.FSPath{FS: osfs.FS{}, Path: dstPath}
	} else {
		dst = cp.FSPath{FS: sftpHosts[dstHost], Path: dstPath}
	}
	m := &model{
		progress:     progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage()),
		msgs:         make(chan tea.Msg),
		copyingFiles: make(map[string]string),

		srcs: srcs,
		dst:  dst,
	}
	if _, err := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(os.Stderr)).Run(); err != nil {
		return err
	}
	if len(m.errs) > 0 {
		return errors.New("exiting with one or more errors")
	}
	return nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: ccp [OPTION]... SOURCE DEST
  or:  ccp [OPTION]... SOURCE... DIRECTORY

Copy SOURCE to DEST, or multiple SOURCE(s) to DIRECTORY.

`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
