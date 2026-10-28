// Package fakepty is an in-memory PTY for tests that must not depend on ConPTY.
package fakepty

import (
	"io"
	"sync"

	"claude-remote/internal/session"
)

type PTY struct {
	out    chan []byte
	exited chan struct{}
	closed chan struct{}

	mu       sync.Mutex
	rest     []byte
	written  []byte
	cols     int
	rows     int
	code     int
	exitOnce sync.Once
	closeOne sync.Once
}

func New(cols, rows int) *PTY {
	return &PTY{
		out:    make(chan []byte, 4096),
		exited: make(chan struct{}),
		closed: make(chan struct{}),
		cols:   cols,
		rows:   rows,
	}
}

// Emit makes b available to the next Read, as if the process printed it.
func (p *PTY) Emit(b []byte) { p.out <- append([]byte(nil), b...) }

// Exit simulates the process ending with the given code.
func (p *PTY) Exit(code int) {
	p.exitOnce.Do(func() {
		p.mu.Lock()
		p.code = code
		p.mu.Unlock()
		close(p.exited)
	})
}

func (p *PTY) Written() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.written...)
}

func (p *PTY) Size() (cols, rows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cols, p.rows
}

func (p *PTY) IsClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

func (p *PTY) Read(b []byte) (int, error) {
	p.mu.Lock()
	if len(p.rest) > 0 {
		n := copy(b, p.rest)
		p.rest = p.rest[n:]
		p.mu.Unlock()
		return n, nil
	}
	p.mu.Unlock()
	var chunk []byte
	select {
	case chunk = <-p.out:
	case <-p.exited:
		select {
		case chunk = <-p.out:
		default:
			return 0, io.EOF
		}
	case <-p.closed:
		return 0, io.EOF
	}
	n := copy(b, chunk)
	if n < len(chunk) {
		p.mu.Lock()
		p.rest = chunk[n:]
		p.mu.Unlock()
	}
	return n, nil
}

func (p *PTY) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.written = append(p.written, b...)
	return len(b), nil
}

func (p *PTY) Resize(cols, rows int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cols, p.rows = cols, rows
	return nil
}

func (p *PTY) Wait() (int, error) {
	select {
	case <-p.exited:
	case <-p.closed:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, nil
}

func (p *PTY) Close() error {
	p.closeOne.Do(func() { close(p.closed) })
	return nil
}

// Factory builds a session.StartFunc and remembers every PTY it launched.
type Factory struct {
	mu   sync.Mutex
	ptys []*PTY
	Err  error // if set, Start fails
}

func (f *Factory) Start(cols, rows int) (*PTY, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	p := New(cols, rows)
	f.ptys = append(f.ptys, p)
	return p, nil
}

// StartFunc adapts the factory for session.New.
func (f *Factory) StartFunc() session.StartFunc {
	return func(cols, rows int) (session.PTY, error) {
		p, err := f.Start(cols, rows)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
}

// Count returns how many processes were started.
func (f *Factory) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ptys)
}

// Last returns the most recently started PTY (nil if none).
func (f *Factory) Last() *PTY {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ptys) == 0 {
		return nil
	}
	return f.ptys[len(f.ptys)-1]
}
