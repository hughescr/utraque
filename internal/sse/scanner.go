// Package sse is a generic Server-Sent Events frame codec: a Scanner that reads
// event/data frames off an io.Reader and a FrameWriter that writes them back.
//
// It follows the WHATWG event-stream parsing rules closely enough for a proxy:
// lines are split on LF, CR, or CRLF; a field is "name: value" with one optional
// leading space stripped from the value; a line beginning with a colon is a
// comment; multiple data: lines within one frame are joined with "\n"; and a
// blank line dispatches the accumulated frame. It imports nothing beyond the
// standard library so every translator can share it without an import cycle.
package sse

import (
	"bufio"
	"bytes"
	"io"
)

// DefaultMaxLineBytes bounds a single logical SSE line (one field). A line
// longer than this fails the scan rather than growing an unbounded buffer on a
// hostile or corrupt upstream.
const DefaultMaxLineBytes = 8 << 20 // 8 MiB

// Frame is one dispatched SSE event. Event is the value of the last event:
// field (empty when the frame carried none — the event-stream default is the
// "message" type, which callers may substitute). Data is the concatenation of
// every data: field in the frame, joined by "\n", with no trailing newline.
type Frame struct {
	Event string
	Data  []byte
}

// Scanner reads SSE frames from an io.Reader. It is single-goroutine: call
// Scan to advance, Frame to read the current frame, and Err after Scan returns
// false to distinguish a clean EOF (nil) from a read or size error.
type Scanner struct {
	r       *bufio.Reader
	maxLine int

	// dispatched frame, valid after Scan returns true
	frame Frame

	// pending carries an EOF/error observed alongside a final newline-less line,
	// so that line is returned first and the terminal error on the next call.
	pending error

	err  error
	done bool
}

// NewScanner builds a Scanner over r with the default line bound.
func NewScanner(r io.Reader) *Scanner {
	return &Scanner{r: bufio.NewReader(r), maxLine: DefaultMaxLineBytes}
}

// SetMaxLineBytes overrides the per-line size bound. A value <= 0 restores the
// default. It must be called before the first Scan.
func (s *Scanner) SetMaxLineBytes(n int) {
	if n <= 0 {
		n = DefaultMaxLineBytes
	}
	s.maxLine = n
}

// Scan advances to the next complete frame, returning true when one is
// available (read it with Frame). It returns false at end of stream or on
// error; call Err to tell them apart. A trailing frame not terminated by a
// blank line before EOF is still dispatched if it accumulated any field, so a
// truncated stream never silently drops its last event.
func (s *Scanner) Scan() bool {
	if s.done {
		return false
	}

	var (
		event    string
		data     []byte
		haveData bool
		haveAny  bool
	)

	dispatch := func() bool {
		s.frame = Frame{Event: event, Data: data}
		return haveAny
	}

	for {
		line, err := s.readLine()
		if err != nil {
			s.done = true
			if err == io.EOF {
				// Flush a frame that was mid-accumulation when the stream ended.
				if haveAny {
					return dispatch()
				}
				return false
			}
			s.err = err
			return false
		}

		// Blank line: dispatch whatever we have. An empty accumulation (a run of
		// blank lines, or comment-only frames) dispatches nothing and we keep
		// reading.
		if len(line) == 0 {
			if haveAny {
				return dispatch()
			}
			continue
		}

		// Comment line: begins with a colon. Ignored entirely, but it is content,
		// so a comment-only frame followed by a blank line still dispatches
		// nothing (haveAny stays false).
		if line[0] == ':' {
			continue
		}

		name, value := splitField(line)
		switch string(name) {
		case "event":
			event = string(value)
			haveAny = true
		case "data":
			if haveData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			haveData = true
			haveAny = true
		case "id", "retry":
			// Parsed and ignored: a proxy neither resumes nor honours reconnection
			// timing, but these are valid fields and must not corrupt the frame.
			haveAny = true
		default:
			// Unknown field name: ignored per the event-stream rules.
		}
	}
}

// Frame returns the frame dispatched by the last Scan that returned true.
func (s *Scanner) Frame() Frame { return s.frame }

// Err returns the first non-EOF read or size error, or nil for a clean end.
func (s *Scanner) Err() error { return s.err }

// readLine returns one logical line with its terminator removed. LF, CRLF, and
// a lone CR each terminate a line, so a field value can never contain CR or LF —
// which is what makes the codec a faithful round trip. It bounds the line length
// at maxLine. A final line not followed by a terminator before EOF is returned
// once, with the EOF delivered on the next call so the line is never lost.
func (s *Scanner) readLine() ([]byte, error) {
	if s.pending != nil {
		err := s.pending
		s.pending = nil
		return nil, err
	}
	var buf []byte
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			if len(buf) > 0 {
				// Deliver the newline-less final line now, its terminator next call.
				s.pending = err
				return buf, nil
			}
			return nil, err
		}
		switch b {
		case '\n':
			return buf, nil
		case '\r':
			// CRLF is one terminator: swallow a following LF, else put the byte
			// back so a lone CR ends the line here.
			nb, err2 := s.r.ReadByte()
			switch {
			case err2 != nil:
				s.pending = err2
			case nb != '\n':
				_ = s.r.UnreadByte()
			}
			return buf, nil
		default:
			buf = append(buf, b)
			if len(buf) > s.maxLine {
				return nil, errLineTooLong
			}
		}
	}
}

// splitField splits a line into (name, value) at the first colon, stripping a
// single optional leading space from value. A line with no colon is a field
// whose name is the whole line and whose value is empty.
func splitField(line []byte) (name, value []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return line, nil
	}
	name = line[:i]
	value = line[i+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return name, value
}
