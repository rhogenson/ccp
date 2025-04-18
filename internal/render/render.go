// Package render renders to the terminal using ANSI escape sequences.
package render

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"unicode/utf8"
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
	w              bufio.Writer
	prevLines      int
	width          int
	partialLineLen int
}

// New creates a new Renderer
func New() *Renderer {
	r := &Renderer{}
	r.w.Reset(os.Stderr)
	return r
}

// Clear clears the screen before rendering a new frame.
func (r *Renderer) Clear(width int) {
	r.width = width
	if r.prevLines > 0 {
		fmt.Fprintf(&r.w, "\033[%dA", r.prevLines)
	}
	r.w.WriteString("\r")
	r.prevLines = 0
	r.partialLineLen = 0
}

func truncate(b []byte, width int) ([]byte, int) {
	n := 0
	for i := 0; i < len(b); {
		if bytes.HasPrefix(b[i:], []byte("\033[")) {
			// An escape sequence usually starts with [, then has one or two numbers
			// separated by semicolon, and ends with some terminating character. To
			// try and munch the whole sequence, skip over any numbers and
			// semicolon here.
			for i += 2; i < len(b)-1 && ('0' <= b[i] && b[i] <= '9' || b[i] == ';'); i++ {
			}
			// Skip the terminating character.
			i++
			continue
		}
		if n+1 > width {
			return b[:i], n
		}
		_, runeWidth := utf8.DecodeRune(b[i:])
		i += runeWidth
		n++
	}
	return b, n
}

// Write implements io.Writer.
func (r *Renderer) Write(buf []byte) (int, error) {
	totalBytes := 0
	for len(buf) > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			line, lineWidth := truncate(buf, r.width-r.partialLineLen)
			r.partialLineLen += lineWidth
			if n, err := r.w.Write(line); err != nil {
				return totalBytes + n, err
			}
			totalBytes += len(buf)
			return totalBytes, nil
		}
		line, _ := truncate(buf[:i], r.width)
		buf = buf[i+1:]
		if n, err := r.w.Write(line); err != nil {
			return totalBytes + n, err
		}
		totalBytes += i
		if _, err := r.w.WriteString("\033[K\n"); err != nil {
			return totalBytes, err
		}
		totalBytes++
		r.prevLines++
		r.partialLineLen = 0
	}
	return totalBytes, nil
}

// Flush flushes the internal buffer to stdout. Flush should be called at the
// end of every frame.
func (r *Renderer) Flush() {
	r.w.WriteString("\033[J")
	r.w.Flush()
}
