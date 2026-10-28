// Package session owns the single persistent PTY process, its output ring
// buffer and the set of connected subscribers.
package session

import (
	"errors"
	"sync"
	"time"
)

// PTY is the pseudo-terminal abstraction; ConPTY on Windows, a fake in tests.
// Close must be idempotent and must kill the whole process tree.
type PTY interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	// Wait blocks until the process exits and returns its exit code.
	Wait() (int, error)
	Close() error
}

// StartFunc launches the configured command. The command line and working
// directory are bound at construction time and never come from clients.
type StartFunc func(cols, rows int) (PTY, error)

// State of the child process.
type State int

const (
	NotStarted State = iota
	Running
	Exited
)

// Event is delivered to subscribers.
type Event struct {
	Data     []byte // terminal output
	Reset    bool   // session was restarted; clear the terminal
	Exited   bool   // process ended
	ExitCode int
}

const (
	subBuffer   = 256
	readBufSize = 32 * 1024
	// How long to let the reader drain remaining output after the process exits.
	drainWait = 300 * time.Millisecond
)

var ErrClosed = errors.New("session closed")

type Session struct {
	start StartFunc

	mu       sync.Mutex
	ring     *Ring
	subs     map[*Subscriber]struct{}
	pty      PTY
	gen      int // incremented per process start; stale goroutines compare it
	state    State
	exitCode int
	cols     int
	rows     int
	closed   bool
}

func New(start StartFunc, ringSize, cols, rows int) *Session {
	return &Session{
		start: start,
		ring:  NewRing(ringSize),
		subs:  make(map[*Subscriber]struct{}),
		cols:  cols,
		rows:  rows,
	}
}

// Subscriber receives broadcast events. A subscriber that cannot keep up is
// dropped (Done is closed) rather than blocking the others.
type Subscriber struct {
	C    chan Event
	done chan struct{}
	once sync.Once
	s    *Session
}

func (b *Subscriber) Done() <-chan struct{} { return b.done }

// Close unregisters the subscriber. Safe to call multiple times.
func (b *Subscriber) Close() {
	b.s.mu.Lock()
	delete(b.s.subs, b)
	b.s.mu.Unlock()
	b.once.Do(func() { close(b.done) })
}

// Snapshot is the initial state handed to a new subscriber.
type Snapshot struct {
	Replay   []byte
	State    State
	ExitCode int
}

// Subscribe registers a subscriber and returns the replay atomically, so no
// output is lost or duplicated between the replay and live events.
func (s *Session) Subscribe() (*Subscriber, Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, Snapshot{}, ErrClosed
	}
	b := &Subscriber{C: make(chan Event, subBuffer), done: make(chan struct{}), s: s}
	s.subs[b] = struct{}{}
	return b, Snapshot{Replay: s.ring.Replay(), State: s.state, ExitCode: s.exitCode}, nil
}

// broadcast must be called with s.mu held.
func (s *Session) broadcast(ev Event) {
	for b := range s.subs {
		select {
		case b.C <- ev:
		default:
			delete(s.subs, b)
			b.once.Do(func() { close(b.done) })
		}
	}
}

// Ensure starts the process on first use (lazy start). After the process has
// exited it does nothing: restarting requires an explicit Restart.
func (s *Session) Ensure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.state != NotStarted {
		return nil
	}
	return s.startLocked()
}

// Restart kills the current process (if any) and starts a fresh one.
func (s *Session) Restart() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	old := s.pty
	s.pty = nil
	s.gen++ // orphan the old goroutines so they cannot touch the new state
	s.ring.Reset()
	s.broadcast(Event{Reset: true})
	err := s.startLocked()
	s.mu.Unlock()
	if old != nil {
		old.Close() // outside the lock: killing a process tree may block
	}
	return err
}

func (s *Session) startLocked() error {
	p, err := s.start(s.cols, s.rows)
	if err != nil {
		s.state, s.exitCode = Exited, -1
		s.broadcast(Event{Exited: true, ExitCode: -1})
		return err
	}
	s.gen++
	s.pty = p
	s.state, s.exitCode = Running, 0
	go s.run(p, s.gen)
	return nil
}

func (s *Session) run(p PTY, gen int) {
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, readBufSize)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				s.mu.Lock()
				if s.gen != gen {
					s.mu.Unlock()
					return
				}
				s.ring.Write(chunk)
				s.broadcast(Event{Data: chunk})
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	code, _ := p.Wait()
	select {
	case <-readerDone:
	case <-time.After(drainWait):
	}
	p.Close()
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second): // a wedged Read must not hang cleanup forever
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen {
		return
	}
	s.pty = nil
	s.state, s.exitCode = Exited, code
	s.broadcast(Event{Exited: true, ExitCode: code})
}

// Write sends client input to the process. Input is dropped if it is not running.
func (s *Session) Write(data []byte) error {
	s.mu.Lock()
	p := s.pty
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	_, err := p.Write(data)
	return err
}

// Resize records the size (the most recent caller wins) and applies it.
func (s *Session) Resize(cols, rows int) error {
	if cols < 1 || rows < 1 || cols > 1000 || rows > 1000 {
		return errors.New("invalid terminal size")
	}
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	p := s.pty
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.Resize(cols, rows)
}

// Close stops the process tree and disconnects all subscribers.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	p := s.pty
	s.pty = nil
	s.gen++
	for b := range s.subs {
		delete(s.subs, b)
		b.once.Do(func() { close(b.done) })
	}
	s.mu.Unlock()
	if p != nil {
		p.Close()
	}
}
