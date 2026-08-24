package sse

import (
	"bufio"
	"io"
)

// Flusher is the flush half of http.Flusher / bufio.Writer, so a FrameWriter
// can push a frame down the wire the instant it is written. It is optional: a
// FrameWriter over a plain io.Writer simply does not flush.
type Flusher interface {
	Flush() error
}

// FrameWriter writes SSE frames to an io.Writer. It is the mirror of Scanner:
// each frame is "event: <name>\n" (omitted when name is empty) followed by one
// "data: <line>\n" per line of the payload, then a terminating blank line.
type FrameWriter struct {
	w   io.Writer
	bw  *bufio.Writer
	fl  Flusher
	err error
}

// NewFrameWriter builds a FrameWriter over w. If w is an http.Flusher (or any
// Flusher), Flush forwards to it so frames are not held in a kernel or handler
// buffer; a bufio.Writer is layered on to coalesce the small per-line writes of
// a single frame into one syscall.
func NewFrameWriter(w io.Writer) *FrameWriter {
	bw := bufio.NewWriter(w)
	fw := &FrameWriter{w: w, bw: bw}
	if fl, ok := w.(Flusher); ok {
		fw.fl = fl
	}
	return fw
}

// WriteFrame writes one frame: an optional event line, a data line per line of
// data, and the terminating blank line. A data payload with embedded newlines
// is split into multiple data: fields, which Scanner rejoins with "\n". An
// empty payload still emits a single empty "data:" line so the frame is
// well-formed and dispatches on the far side.
func (fw *FrameWriter) WriteFrame(event string, data []byte) error {
	if fw.err != nil {
		return fw.err
	}
	if event != "" {
		fw.write("event: ")
		fw.write(event)
		fw.writeByte('\n')
	}
	// Emit one data: field per line segment. The number of segments is
	// (count of '\n') + 1, so an empty payload yields one empty field, and a
	// payload ending in '\n' yields a trailing empty field — both of which
	// Scanner reproduces exactly, keeping the codec a faithful round trip.
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			fw.writeData(data[start:i])
			start = i + 1
		}
	}
	fw.writeData(data[start:])
	fw.writeByte('\n') // terminating blank line
	return fw.err
}

func (fw *FrameWriter) writeData(seg []byte) {
	fw.write("data: ")
	fw.writeBytes(seg)
	fw.writeByte('\n')
}

// Flush pushes buffered frames to the underlying writer and, when it supports
// flushing, on out of any downstream buffer.
func (fw *FrameWriter) Flush() error {
	if fw.err != nil {
		return fw.err
	}
	if err := fw.bw.Flush(); err != nil {
		fw.err = err
		return err
	}
	if fw.fl != nil {
		if err := fw.fl.Flush(); err != nil {
			fw.err = err
			return err
		}
	}
	return nil
}

// Err returns the first write error, sticky after it occurs.
func (fw *FrameWriter) Err() error { return fw.err }

func (fw *FrameWriter) write(s string) {
	if fw.err != nil {
		return
	}
	if _, err := fw.bw.WriteString(s); err != nil {
		fw.err = err
	}
}

func (fw *FrameWriter) writeBytes(b []byte) {
	if fw.err != nil {
		return
	}
	if _, err := fw.bw.Write(b); err != nil {
		fw.err = err
	}
}

func (fw *FrameWriter) writeByte(b byte) {
	if fw.err != nil {
		return
	}
	if err := fw.bw.WriteByte(b); err != nil {
		fw.err = err
	}
}
