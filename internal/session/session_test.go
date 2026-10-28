package session_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"claude-remote/internal/session"
	"claude-remote/internal/session/fakepty"
)

func newSession(t *testing.T, ring int) (*session.Session, *fakepty.Factory) {
	t.Helper()
	f := &fakepty.Factory{}
	s := session.New(f.StartFunc(), ring, 80, 24)
	t.Cleanup(s.Close)
	return s, f
}

func next(t *testing.T, sub *session.Subscriber) session.Event {
	t.Helper()
	select {
	case ev := <-sub.C:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return session.Event{}
	}
}

func TestLazyStart(t *testing.T) {
	s, f := newSession(t, 1024)
	if f.Count() != 0 {
		t.Fatal("process started before first use")
	}
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	if f.Count() != 1 {
		t.Fatalf("expected one process, got %d", f.Count())
	}
}

func TestBroadcastToAllSubscribers(t *testing.T) {
	s, f := newSession(t, 1024)
	s.Ensure()
	a, _, _ := s.Subscribe()
	b, _, _ := s.Subscribe()
	f.Last().Emit([]byte("hello"))
	if got := string(next(t, a).Data); got != "hello" {
		t.Fatalf("a got %q", got)
	}
	if got := string(next(t, b).Data); got != "hello" {
		t.Fatalf("b got %q", got)
	}
}

func TestNewSubscriberGetsBufferedOutput(t *testing.T) {
	s, f := newSession(t, 1024)
	s.Ensure()
	a, _, _ := s.Subscribe()
	f.Last().Emit([]byte("before"))
	next(t, a)
	_, snap, _ := s.Subscribe()
	if !strings.HasSuffix(string(snap.Replay), "before") {
		t.Fatalf("replay %q", snap.Replay)
	}
}

func TestSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	s, f := newSession(t, 1<<20)
	s.Ensure()
	slow, _, _ := s.Subscribe() // never reads
	fast, _, _ := s.Subscribe()

	done := make(chan int)
	go func() {
		n := 0
		for ev := range fast.C {
			n += len(ev.Data)
			if n >= 1000 {
				break
			}
		}
		done <- n
	}()
	// Far more events than the subscriber buffer holds.
	for i := 0; i < 1000; i++ {
		f.Last().Emit([]byte("x"))
		time.Sleep(50 * time.Microsecond)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fast subscriber starved by slow one")
	}
	select {
	case <-slow.Done():
	case <-time.After(time.Second):
		t.Fatal("slow subscriber was not disconnected")
	}
}

func TestInputAndResizeReachPTY(t *testing.T) {
	s, f := newSession(t, 1024)
	s.Ensure()
	if err := s.Write([]byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	if got := string(f.Last().Written()); got != "ls\r" {
		t.Fatalf("written %q", got)
	}
	if err := s.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	if c, r := f.Last().Size(); c != 100 || r != 30 {
		t.Fatalf("size %dx%d", c, r)
	}
	if err := s.Resize(0, 30); err == nil {
		t.Fatal("invalid size accepted")
	}
}

func TestExitNotifiesAndDoesNotAutoRestart(t *testing.T) {
	s, f := newSession(t, 1024)
	s.Ensure()
	sub, _, _ := s.Subscribe()
	f.Last().Emit([]byte("bye"))
	f.Last().Exit(3)
	var sawData, sawExit bool
	for !sawExit {
		ev := next(t, sub)
		sawData = sawData || string(ev.Data) == "bye"
		if ev.Exited {
			sawExit = true
			if ev.ExitCode != 3 {
				t.Fatalf("exit code %d", ev.ExitCode)
			}
		}
	}
	if !sawData {
		t.Fatal("output before exit was lost")
	}
	s.Ensure()
	if f.Count() != 1 {
		t.Fatal("process was restarted without an explicit request")
	}
	_, snap, _ := s.Subscribe()
	if snap.State != session.Exited || snap.ExitCode != 3 {
		t.Fatalf("snapshot state %v code %d", snap.State, snap.ExitCode)
	}
}

func TestRestartReplacesProcessAndClearsBuffer(t *testing.T) {
	s, f := newSession(t, 1024)
	s.Ensure()
	sub, _, _ := s.Subscribe()
	old := f.Last()
	old.Emit([]byte("old output"))
	next(t, sub)

	if err := s.Restart(); err != nil {
		t.Fatal(err)
	}
	if !next(t, sub).Reset {
		t.Fatal("expected reset event")
	}
	if f.Count() != 2 || !old.IsClosed() {
		t.Fatalf("count=%d oldClosed=%v", f.Count(), old.IsClosed())
	}
	f.Last().Emit([]byte("new"))
	if got := string(next(t, sub).Data); got != "new" {
		t.Fatalf("got %q", got)
	}
	_, snap, _ := s.Subscribe()
	if strings.Contains(string(snap.Replay), "old output") {
		t.Fatal("old output survived restart")
	}
}

func TestRestartAfterExit(t *testing.T) {
	s, f := newSession(t, 1024)
	s.Ensure()
	sub, _, _ := s.Subscribe()
	f.Last().Exit(0)
	for !next(t, sub).Exited {
	}
	if err := s.Restart(); err != nil {
		t.Fatal(err)
	}
	if f.Count() != 2 {
		t.Fatal("restart did not start a process")
	}
}

func TestStartFailureReported(t *testing.T) {
	f := &fakepty.Factory{Err: errors.New("boom")}
	s := session.New(f.StartFunc(), 1024, 80, 24)
	defer s.Close()
	if err := s.Ensure(); err == nil {
		t.Fatal("expected error")
	}
}

func TestCloseKillsProcessAndDisconnects(t *testing.T) {
	s, f := newSession(t, 1024)
	s.Ensure()
	sub, _, _ := s.Subscribe()
	s.Close()
	if !f.Last().IsClosed() {
		t.Fatal("process not closed")
	}
	select {
	case <-sub.Done():
	case <-time.After(time.Second):
		t.Fatal("subscriber not disconnected")
	}
	if _, _, err := s.Subscribe(); err == nil {
		t.Fatal("subscribe after close succeeded")
	}
}
