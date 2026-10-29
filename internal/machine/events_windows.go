//go:build windows

package machine

import (
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows tells a program about the display, the lid, session lock and
// sleep/resume only through window messages, so a hidden message-only window
// runs on its own OS thread. Until the first event arrives a value is "unknown".

var (
	wtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")

	pRegisterClassEx        = user32.NewProc("RegisterClassExW")
	pCreateWindowEx         = user32.NewProc("CreateWindowExW")
	pDefWindowProc          = user32.NewProc("DefWindowProcW")
	pGetMessage             = user32.NewProc("GetMessageW")
	pTranslateMessage       = user32.NewProc("TranslateMessage")
	pDispatchMessage        = user32.NewProc("DispatchMessageW")
	pRegisterPowerSetting   = user32.NewProc("RegisterPowerSettingNotification")
	pGetLastInputInfo       = user32.NewProc("GetLastInputInfo")
	pWTSRegisterSessionNote = wtsapi32.NewProc("WTSRegisterSessionNotification")
	pGetModuleHandle        = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmPowerBroadcast    = 0x0218
	wmWtsSessionChange  = 0x02B1
	pbtApmSuspend       = 0x0004
	pbtApmResumeSuspend = 0x0007
	pbtApmResumeAuto    = 0x0012
	pbtPowerSetting     = 0x8013
	wtsSessionLock      = 0x7
	wtsSessionUnlock    = 0x8
)

var (
	guidConsoleDisplayState = windows.GUID{Data1: 0x6fe69556, Data2: 0x704a, Data3: 0x47a0, Data4: [8]byte{0x8f, 0x24, 0xc2, 0x8d, 0x93, 0x6f, 0xda, 0x47}}
	guidLidSwitchState      = windows.GUID{Data1: 0xba3e0f4d, Data2: 0xb817, Data3: 0x4094, Data4: [8]byte{0xa2, 0xd1, 0xd5, 0x63, 0x79, 0xe6, 0xa0, 0xf3}}
)

// powerBroadcastSetting is POWERBROADCAST_SETTING with a 4-byte payload.
type powerBroadcastSetting struct {
	guid   windows.GUID
	length uint32
	data   uint32
}

// screenState is what the event window learns.
type screenState struct {
	mu          sync.Mutex
	display     string // "unknown", "on", "dim", "off"
	lid         string // "unknown", "open", "closed"
	lockedKnown bool   // a lock/unlock event has been seen
	locked      bool
	suspendedAt time.Time
	resumedAt   time.Time
	steps       []string // start-up diagnostics (which Win32 call failed)
	messages    int      // window messages received
}

func (s *screenState) note(format string, a ...any) {
	s.mu.Lock()
	s.steps = append(s.steps, fmt.Sprintf(format, a...))
	s.mu.Unlock()
}

func newScreenState() *screenState { return &screenState{display: "unknown", lid: "unknown"} }

type screenSnapshot struct {
	Display     string
	Lid         string
	LockedKnown bool
	Locked      bool
	SuspendedAt time.Time
	ResumedAt   time.Time
}

func (s *screenState) snapshot() screenSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return screenSnapshot{s.display, s.lid, s.lockedKnown, s.locked, s.suspendedAt, s.resumedAt}
}

// ptr turns a window-message pointer parameter into an unsafe.Pointer. The
// round trip through a variable keeps go vet's unsafe check quiet: lp is a
// pointer Windows owns for the duration of the call.
func ptr(lp uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&lp)) }

// onMessage updates the state from one window message. It is separate from
// the window procedure so it can be tested with synthetic messages.
func (s *screenState) onMessage(msg uint32, wp, lp uintptr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages++
	switch msg {
	case wmPowerBroadcast:
		switch wp {
		case pbtApmSuspend:
			s.suspendedAt = time.Now()
		case pbtApmResumeSuspend, pbtApmResumeAuto:
			s.resumedAt = time.Now()
		case pbtPowerSetting:
			if lp == 0 {
				return
			}
			ps := (*powerBroadcastSetting)(ptr(lp))
			if ps.length < 4 {
				return
			}
			switch ps.guid {
			case guidConsoleDisplayState:
				switch ps.data {
				case 0:
					s.display = "off"
				case 1:
					s.display = "on"
				case 2:
					s.display = "dim"
				}
			case guidLidSwitchState:
				if ps.data == 0 {
					s.lid = "closed"
				} else {
					s.lid = "open"
				}
			}
		}
	case wmWtsSessionChange:
		switch wp {
		case wtsSessionLock:
			s.lockedKnown, s.locked = true, true
		case wtsSessionUnlock:
			s.lockedKnown, s.locked = true, false
		}
	}
}

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type winMsg struct {
	hwnd    uintptr
	message uint32
	_       uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	x, y    int32
	_       uint32
}

// startEvents runs the message window for the life of the process. If any
// step fails the state simply stays "unknown" and the older heuristics apply.
func startEvents(s *screenState) {
	go func() {
		runtime.LockOSThread()
		className, _ := windows.UTF16PtrFromString("claude-remote-events")
		inst, _, _ := pGetModuleHandle.Call(0)
		proc := windows.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
			s.onMessage(uint32(msg), wp, lp)
			r, _, _ := pDefWindowProc.Call(hwnd, msg, wp, lp)
			return r
		})
		wc := wndClassEx{wndProc: proc, instance: inst, className: className}
		wc.size = uint32(unsafe.Sizeof(wc))
		atom, _, cerr := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
		s.note("RegisterClassEx atom=%d err=%v", atom, cerr)
		if atom == 0 {
			return
		}
		hwndMessage := ^uintptr(2) // HWND_MESSAGE (-3): a window that only receives messages
		hwnd, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, inst, 0)
		s.note("CreateWindowEx hwnd=%d", hwnd)
		if hwnd == 0 {
			return
		}
		// Windows delivers the current display and lid state right after registering.
		h1, _, e1 := pRegisterPowerSetting.Call(hwnd, uintptr(unsafe.Pointer(&guidConsoleDisplayState)), 0)
		h2, _, e2 := pRegisterPowerSetting.Call(hwnd, uintptr(unsafe.Pointer(&guidLidSwitchState)), 0)
		w, _, e3 := pWTSRegisterSessionNote.Call(hwnd, 0) // NOTIFY_FOR_THIS_SESSION
		s.note("register display=%d(%v) lid=%d(%v) wts=%d(%v)", h1, e1, h2, e2, w, e3)
		var m winMsg
		for {
			if r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
				return
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
}

// idleSeconds is the time since the last keyboard or mouse input.
func idleSeconds() int {
	type lastInput struct{ size, tick uint32 }
	li := lastInput{size: uint32(unsafe.Sizeof(lastInput{}))}
	if r, _, _ := pGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li))); r == 0 {
		return -1
	}
	now, _, _ := pGetTickCount64.Call()
	return int((uint32(now) - li.tick) / 1000) // 32-bit wrap-around is intended
}
