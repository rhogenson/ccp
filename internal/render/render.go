// Package render renders to the terminal using ANSI escape sequences.
package render

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
)

// Renderer updates a terminal UI. Typical usage looks like
//
//	r := render.New()
//	// Game loop
//	for {
//	    // Update state
//
//	    r.Clear()
//	    fmt.Fprintf(r, "Render UI by writing to r using io.Writer")
//	    r.Flush()
//	}
type Renderer struct {
	w         bufio.Writer
	prevLines int
}

// New creates a new Renderer
func New() *Renderer {
	r := &Renderer{}
	r.w.Reset(os.Stderr)
	return r
}

// Clear clears the screen before rendering a new frame.
func (r *Renderer) Clear() {
	if r.prevLines > 0 {
		fmt.Fprintf(&r.w, "\033[%dA", r.prevLines)
	}
	r.prevLines = 0
	r.w.WriteString("\r")
}

// Write implements io.Writer.
func (r *Renderer) Write(buf []byte) (int, error) {
	totalBytes := 0
	for len(buf) > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			n, err := r.w.Write(buf)
			totalBytes += n
			return totalBytes, err
		}
		line := buf[:i]
		buf = buf[i+1:]
		n, err := r.w.Write(line)
		totalBytes += n
		if err != nil {
			return totalBytes, err
		}
		if _, err := r.w.WriteString("\033[K\n"); err != nil {
			return totalBytes, err
		}
		totalBytes++
		r.prevLines++
	}
	return totalBytes, nil
}

// Flush flushes the internal buffer to stdout. Flush should be called at the
// end of every frame.
func (r *Renderer) Flush() {
	r.w.WriteString("\033[J")
	r.w.Flush()
}
