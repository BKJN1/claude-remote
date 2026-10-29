//go:build windows

package machine

import (
	"testing"
	"time"
	"unsafe"
)

// These tests feed the handler synthetic window messages. Nothing touches the
// real display, session or power state.

func send(s *screenState, ps powerBroadcastSetting) {
	s.onMessage(wmPowerBroadcast, pbtPowerSetting, uintptr(unsafe.Pointer(&ps)))
}

func TestDisplayStateFollowsPowerSettingMessages(t *testing.T) {
	s := newScreenState()
	if s.snapshot().Display != "unknown" {
		t.Fatal("must start unknown")
	}
	for data, want := range map[uint32]string{1: "on", 0: "off", 2: "dim"} {
		ps := powerBroadcastSetting{guid: guidConsoleDisplayState, length: 4, data: data}
		send(s, ps)
		if got := s.snapshot().Display; got != want {
			t.Fatalf("data %d: %q, want %q", data, got, want)
		}
	}
	// An unknown value changes nothing.
	send(s, powerBroadcastSetting{guid: guidConsoleDisplayState, length: 4, data: 9})
	if s.snapshot().Display != "dim" {
		t.Fatal("an unknown display value changed the state")
	}
	// A truncated payload is ignored rather than read.
	send(s, powerBroadcastSetting{guid: guidConsoleDisplayState, length: 1, data: 0})
	if s.snapshot().Display != "dim" {
		t.Fatal("a short payload was trusted")
	}
	s.onMessage(wmPowerBroadcast, pbtPowerSetting, 0) // null pointer must not crash
}

func TestLidAndOtherSettingsAreSeparate(t *testing.T) {
	s := newScreenState()
	send(s, powerBroadcastSetting{guid: guidLidSwitchState, length: 4, data: 0})
	if got := s.snapshot(); got.Lid != "closed" || got.Display != "unknown" {
		t.Fatalf("%+v", got)
	}
	send(s, powerBroadcastSetting{guid: guidLidSwitchState, length: 4, data: 1})
	if s.snapshot().Lid != "open" {
		t.Fatal("lid not reopened")
	}
	// Some other power setting GUID must be ignored.
	other := powerBroadcastSetting{length: 4, data: 0}
	send(s, other)
	if s.snapshot().Display != "unknown" {
		t.Fatal("foreign setting changed the display state")
	}
}

func TestSessionLockEvents(t *testing.T) {
	s := newScreenState()
	if s.snapshot().LockedKnown {
		t.Fatal("known before any event")
	}
	s.onMessage(wmWtsSessionChange, wtsSessionLock, 0)
	if got := s.snapshot(); !got.LockedKnown || !got.Locked {
		t.Fatalf("%+v", got)
	}
	s.onMessage(wmWtsSessionChange, wtsSessionUnlock, 0)
	if got := s.snapshot(); !got.LockedKnown || got.Locked {
		t.Fatalf("%+v", got)
	}
	s.onMessage(wmWtsSessionChange, 0x1, 0) // a different session event
	if s.snapshot().Locked {
		t.Fatal("unrelated session event changed the lock state")
	}
}

func TestSuspendAndResumeAreRemembered(t *testing.T) {
	s := newScreenState()
	before := time.Now()
	s.onMessage(wmPowerBroadcast, pbtApmSuspend, 0)
	s.onMessage(wmPowerBroadcast, pbtApmResumeAuto, 0)
	got := s.snapshot()
	if got.SuspendedAt.Before(before) || got.ResumedAt.Before(before) {
		t.Fatalf("%+v", got)
	}
}

func TestStatusCarriesScreenFields(t *testing.T) {
	h := New()
	h.screen.onMessage(wmPowerBroadcast, pbtApmSuspend, 0)
	send(h.screen, powerBroadcastSetting{guid: guidConsoleDisplayState, length: 4, data: 0})
	st, err := h.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Display != "off" || st.SleptAt == nil {
		t.Fatalf("%+v", st)
	}
	if st.IdleSec < -1 {
		t.Fatalf("idle %d", st.IdleSec)
	}
}

// On a real desktop Windows reports the display state right after the window
// registers for it. Tolerant: a headless runner may never get one.
func TestRealEventWindowReportsTheDisplay(t *testing.T) {
	h := New()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.screen.snapshot().Display == "unknown" {
		time.Sleep(50 * time.Millisecond)
	}
	got := h.screen.snapshot()
	t.Logf("display=%s lid=%s messages=%d steps=%v", got.Display, got.Lid, h.screen.messagesSeen(), h.screen.stepsSeen())
	switch got.Display {
	case "unknown", "on", "dim", "off":
	default:
		t.Fatalf("bad value %q", got.Display)
	}
}

func (s *screenState) messagesSeen() int { s.mu.Lock(); defer s.mu.Unlock(); return s.messages }
func (s *screenState) stepsSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.steps...)
}
