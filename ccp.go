package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gitlab.com/rhogenson/ccp/internal/cp"
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

	srcs []string
	dst  string

	max          int64
	current      atomic.Int64
	measurements deque.Deque[measurement]
	copyingFiles map[string]bool
	copyingFile  string
	errs         []string
}

type (
	tickMsg struct{}

	maxMsg       int64
	fileStartMsg string
	fileDoneMsg  struct {
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
	return tea.Tick(10*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			cp.Copy(m.srcs, m.dst, *f, m)
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
		name := string(msg)
		m.copyingFiles[name] = true
		if m.copyingFile == "" {
			m.copyingFile = name
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
		now := time.Now()
		for m.measurements.Len() > 1 && now.Sub(m.measurements.At(0).t) > 2*time.Minute {
			m.measurements.PopFront()
		}
		n := m.current.Load()
		m.measurements.PushBack(measurement{now, n})
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
		copying = "Copying " + m.copyingFile + "..."
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

func (m *model) FileStart(name string) {
	m.msgs <- fileStartMsg(name)
}

func (m *model) FileDone(name string, err error) {
	m.msgs <- fileDoneMsg{name, err}
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

	args := flag.Args()
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage error")
		os.Exit(2)
	}
	srcs, dst := args[:len(args)-1], args[len(args)-1]
	if len(srcs) == 1 {
		if stat, err := os.Stat(dst); err == nil && stat.IsDir() {
			dst = filepath.Join(dst, filepath.Base(srcs[0]))
		}
	}
	m := &model{
		progress:     progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage()),
		msgs:         make(chan tea.Msg),
		copyingFiles: make(map[string]bool),

		srcs: srcs,
		dst:  dst,
	}
	if _, err := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(os.Stderr)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(m.errs) > 0 {
		os.Exit(1)
	}
}
