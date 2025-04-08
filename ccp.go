package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"gitlab.com/rhogenson/deque"
)

type measurement struct {
	t time.Time
	i int64
}

type model struct {
	progress progress.Model

	max          int64
	measurements deque.Deque[measurement]
}

func (m *model) Init() tea.Cmd {
	return nil
}

type (
	progressMsg int
	doneMsg     struct{}
)

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.progress.Width = msg.Width - 4
		return m, nil
	case progressMsg:
		n := int64(msg)
		if m.measurements.Len() > 0 {
			n += m.measurements.At(m.measurements.Len() - 1).i
		}
		if m.measurements.Len() > 0 && m.measurements.At(m.measurements.Len()-1).i > n {
			m.measurements.Reset()
		}
		now := time.Now()
		for m.measurements.Len() > 2 && now.Sub(m.measurements.At(0).t) > 2*time.Minute {
			m.measurements.PopFront()
		}
		m.measurements.PushBack(measurement{now, n})
		return m, m.progress.SetPercent(float64(n) / float64(m.max))
	case doneMsg:
		return m, tea.Sequence(
			m.progress.SetPercent(1),
			tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return nil }),
			tea.Quit)
	// FrameMsg is sent when the progress bar wants to animate itself
	case progress.FrameMsg:
		progressModel, cmd := m.progress.Update(msg)
		m.progress = progressModel.(progress.Model)
		return m, cmd
	default:
		return m, nil
	}
}

func (m *model) View() string {
	etaStr := "calculating..."
	if m.measurements.Len() > 1 {
		first := m.measurements.At(0)
		last := m.measurements.At(m.measurements.Len() - 1)
		deltaT := last.t.Sub(first.t)
		delta := last.i - first.i
		if delta != 0 {
			etaStr = time.Duration(float64(m.max-last.i) / float64(delta) * float64(deltaT)).Round(time.Second).String()
		}
	}
	return "\n" +
		"  " + m.progress.View() + "\n" +
		"  " + "ETA: " + etaStr + "\n"
}

func copy(out, in *os.File, progress func(int64)) error {
	for {
		n, err := io.CopyN(out, in, 1024*1024)
		if n > 0 {
			progress(n)
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
	return nil
}

func run() error {
	args := flag.Args()
	if len(args) != 2 {
		return errors.New("usage error")
	}
	in, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer in.Close()
	stat, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(args[1], os.O_WRONLY|os.O_CREATE|os.O_TRUNC, stat.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	p := tea.NewProgram(&model{
		progress: progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage()),
		max:      stat.Size() + 1,
	}, tea.WithInput(nil), tea.WithOutput(os.Stderr))
	var copyErr error
	go func() {
		if copyErr = copy(out, in, func(n int64) { p.Send(progressMsg(n)) }); copyErr != nil {
			p.Send(tea.QuitMsg{})
			return
		}
		p.Send(doneMsg{})
	}()
	_, teaErr := p.Run()
	if copyErr != nil {
		return copyErr
	}
	return teaErr
}

func main() {
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
