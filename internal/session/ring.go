package session

import "bytes"

// Ring keeps the most recent cap bytes of terminal output. Not safe for
// concurrent use; Session serialises access.
type Ring struct {
	buf     []byte
	start   int  // index of the oldest byte
	n       int  // bytes stored
	dropped bool // true once old output has been overwritten
}

func NewRing(capacity int) *Ring { return &Ring{buf: make([]byte, capacity)} }

func (r *Ring) Write(p []byte) {
	c := len(r.buf)
	if len(p) >= c {
		copy(r.buf, p[len(p)-c:])
		r.start, r.n, r.dropped = 0, c, r.dropped || len(p) > c || r.n > 0
		return
	}
	end := (r.start + r.n) % c
	k := copy(r.buf[end:], p)
	copy(r.buf, p[k:])
	r.n += len(p)
	if r.n > c {
		r.start = (r.start + r.n - c) % c
		r.n = c
		r.dropped = true
	}
}

// Snapshot returns the stored bytes, oldest first, as a fresh slice.
func (r *Ring) Snapshot() []byte {
	out := make([]byte, r.n)
	k := copy(out, r.buf[r.start:min(r.start+r.n, len(r.buf))])
	copy(out[k:], r.buf[:r.n-k])
	return out
}

func (r *Ring) Reset() { r.start, r.n, r.dropped = 0, 0, false }

// resetSeq is RIS (full terminal reset). It also leaves the alternate screen.
const resetSeq = "\x1bc"

// Replay returns what a newly connected client should be fed.
//
// The ring may start in the middle of an escape sequence (or UTF-8 char),
// which would make the client's terminal swallow or garble the following
// text. Once old output has been overwritten we therefore drop everything up
// to and including the first newline: escape sequences never contain a raw
// newline, so the remainder starts on a clean boundary. The stream is
// prefixed with RIS so the client starts from a known state. Known
// limitation: SGR colours/modes set in the dropped part are lost until the
// application next sets them, and a full-screen TUI is repainted on the next
// redraw (the client also sends a resize, which makes most TUIs redraw).
func (r *Ring) Replay() []byte {
	data := r.Snapshot()
	if r.dropped {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	return append([]byte(resetSeq), data...)
}
