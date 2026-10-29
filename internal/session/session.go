// Package session owns the single persistent PTY process, its output ring
// buffer and the set of connected subscribers.
package session

import (
	"errors"
	"io"
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

// Why a process could not be started; shown to the user in plain words.
const (
	KindCommandNotFound = "command_not_found"
	KindWorkdirMissing  = "workdir_missing"
	KindStartFailed     = "start_failed"
)

// StartError classifies a launch failure.
type StartError struct {
	Kind string
	Err  error
}

func (e *StartError) Error() string { return e.Kind + ": " + e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

func errKind(err error) string {
	var se *StartError
	if errors.As(err, &se) {
		return se.Kind
	}
	return KindStartFailed
}

// Event is delivered to subscribers.
type Event struct {
	Data     []byte // terminal output
	Reset    bool   // session was restarted; clear the terminal
	Exited   bool   // process ended
	ExitCode int
	ErrKind  string // set with Exited when the process never started

	// Pulse is set when what the session is doing changed (see SetActivity).
	Pulse      string
	PulseSince time.Time
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
	errKind  string
	activity string      // "working", "waiting" or "idle" as reported by hooks; "" if unknown
	actSince time.Time   // when activity last changed
	lastOut  time.Time   // last time the process printed anything
	rec      chan []byte // optional full-output recorder queue (--record)
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
	ErrKind  string

	Activity      string
	ActivitySince time.Time
}

// Info is a point-in-time summary for listings.
type Info struct {
	State    State
	ExitCode int
	ErrKind  string

	Activity      string // see SetActivity
	ActivitySince time.Time
	LastOutput    time.Time
}

func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Info{State: s.state, ExitCode: s.exitCode, ErrKind: s.errKind, Activity: s.activity, ActivitySince: s.actSince, LastOutput: s.lastOut}
}

// SetActivity records what Claude Code reported through its hooks
// ("working", "waiting" for the user, "idle") and tells subscribers when it
// changes. An empty state means "unknown".
func (s *Session) SetActivity(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.activity == state {
		return
	}
	s.activity, s.actSince = state, time.Now()
	if state != "" {
		s.broadcast(Event{Pulse: state, PulseSince: s.actSince})
	}
}

// SetRecorder makes the session copy all process output to w. Writes happen
// on their own goroutine through a bounded queue, so a slow or full disk can
// never stall the terminal (chunks are dropped when the queue is full). If w
// is an io.Closer it is closed with the session.
func (s *Session) SetRecorder(w io.Writer) {
	ch := make(chan []byte, 1024)
	go func() {
		for chunk := range ch {
			w.Write(chunk)
		}
		if c, ok := w.(io.Closer); ok {
			c.Close()
		}
	}()
	s.mu.Lock()
	s.rec = ch
	s.mu.Unlock()
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
	return b, Snapshot{Replay: s.ring.Replay(), State: s.state, ExitCode: s.exitCode, ErrKind: s.errKind, Activity: s.activity, ActivitySince: s.actSince}, nil
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
		s.state, s.exitCode, s.errKind = Exited, -1, errKind(err)
		s.broadcast(Event{Exited: true, ExitCode: -1, ErrKind: s.errKind})
		return err
	}
	s.gen++
	s.pty = p
	s.state, s.exitCode, s.errKind = Running, 0, ""
	s.activity, s.actSince = "", time.Now()
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
				s.lastOut = time.Now()
				s.ring.Write(chunk)
				if s.rec != nil {
					select {
					case s.rec <- chunk:
					default: // recorder is behind; drop rather than block the terminal
					}
				}
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
	rec := s.rec
	s.rec = nil
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
	if rec != nil {
		close(rec) // the writer goroutine drains the queue, then closes the file
	}
}
