package session_test

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"claude-remote/internal/session"
	"claude-remote/internal/session/fakepty"
)

func newManager(max int) (*session.Manager, *fakepty.Factory) {
	f := &fakepty.Factory{}
	m := session.NewManager(func(session.Meta) session.StartFunc { return f.StartFunc() }, 1024, max)
	return m, f
}

func TestManagerCreatesIndependentSessions(t *testing.T) {
	m, f := newManager(5)
	defer m.CloseAll()
	a, err := m.Create("proj", "/w", "first", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.Create("proj", "/w", "second", "abc")
	if a.ID == b.ID || len(a.ID) != 12 {
		t.Fatalf("ids %q %q", a.ID, b.ID)
	}
	if f.Count() != 2 {
		t.Fatalf("processes started: %d", f.Count())
	}
	subA, _, _ := a.S.Subscribe()
	subB, _, _ := b.S.Subscribe()
	// Output of one session must not appear in the other.
	f.Last().Emit([]byte("only-b"))
	select {
	case ev := <-subB.C:
		if string(ev.Data) != "only-b" {
			t.Fatalf("got %q", ev.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no output")
	}
	select {
	case ev := <-subA.C:
		t.Fatalf("leak into other session: %q", ev.Data)
	case <-time.After(150 * time.Millisecond):
	}
	if got := m.List(); len(got) != 2 || got[0].ID != a.ID {
		t.Fatalf("list order wrong: %v", got)
	}
}

func TestManagerLimit(t *testing.T) {
	m, _ := newManager(2)
	defer m.CloseAll()
	m.Create("p", "/w", "", "")
	m.Create("p", "/w", "", "")
	if _, err := m.Create("p", "/w", "", ""); !errors.Is(err, session.ErrTooManySessions) {
		t.Fatalf("got %v", err)
	}
}

func TestManagerCloseKillsProcess(t *testing.T) {
	m, f := newManager(3)
	e, _ := m.Create("p", "/w", "", "")
	p := f.Last()
	if !m.Close(e.ID) {
		t.Fatal("close reported missing")
	}
	if !p.IsClosed() {
		t.Fatal("process not killed")
	}
	if _, ok := m.Get(e.ID); ok {
		t.Fatal("session still registered")
	}
	if m.Close(e.ID) {
		t.Fatal("second close succeeded")
	}
	// Room again after closing.
	if _, err := m.Create("p", "/w", "", ""); err != nil {
		t.Fatal(err)
	}
	m.CloseAll()
}

func TestStartFailureVisibleInInfo(t *testing.T) {
	f := &fakepty.Factory{Err: &session.StartError{Kind: session.KindCommandNotFound, Err: errors.New("nope")}}
	m := session.NewManager(func(session.Meta) session.StartFunc { return f.StartFunc() }, 1024, 3)
	defer m.CloseAll()
	e, err := m.Create("p", "/w", "", "")
	if err != nil {
		t.Fatal(err)
	}
	info := e.S.Info()
	if info.State != session.Exited || info.ErrKind != session.KindCommandNotFound {
		t.Fatalf("info %+v", info)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestRecorderReceivesOutput(t *testing.T) {
	m, f := newManager(3)
	defer m.CloseAll()
	buf := &syncBuf{}
	m.SetRecorderFactory(func(session.Meta) io.Writer { return buf })
	m.Create("p", "/w", "", "")
	f.Last().Emit([]byte("recorded text"))
	deadline := time.Now().Add(2 * time.Second)
	for buf.String() != "recorded text" {
		if time.Now().After(deadline) {
			t.Fatalf("recorder got %q", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEvictsExitedSessionWhenFull(t *testing.T) {
	m, f := newManager(2)
	defer m.CloseAll()
	a, _ := m.Create("p", "/w", "a", "")
	b, _ := m.Create("p", "/w", "b", "")
	// Both running: no room, nothing is closed behind the user's back.
	if _, err := m.Create("p", "/w", "c", ""); !errors.Is(err, session.ErrTooManySessions) {
		t.Fatalf("got %v", err)
	}
	// Once the oldest has exited it makes room.
	f.Nth(0).Exit(0)
	deadline := time.Now().Add(2 * time.Second)
	for a.S.Info().State != session.Exited {
		if time.Now().After(deadline) {
			t.Fatal("exit not noticed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.Create("p", "/w", "c", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(a.ID); ok {
		t.Fatal("exited session was not evicted")
	}
	if _, ok := m.Get(b.ID); !ok {
		t.Fatal("running session was evicted")
	}
}
