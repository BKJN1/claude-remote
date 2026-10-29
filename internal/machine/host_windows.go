//go:build windows

package machine

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	user32   = windows.NewLazySystemDLL("user32.dll")
	powrprof = windows.NewLazySystemDLL("powrprof.dll")
	pdh      = windows.NewLazySystemDLL("pdh.dll")

	pGetSystemPowerStatus    = kernel32.NewProc("GetSystemPowerStatus")
	pGetSystemTimes          = kernel32.NewProc("GetSystemTimes")
	pGlobalMemoryStatusEx    = kernel32.NewProc("GlobalMemoryStatusEx")
	pGetTickCount64          = kernel32.NewProc("GetTickCount64")
	pSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
	pLockWorkStation         = user32.NewProc("LockWorkStation")
	pOpenInputDesktop        = user32.NewProc("OpenInputDesktop")
	pCloseDesktop            = user32.NewProc("CloseDesktop")
	pSetSuspendState         = powrprof.NewProc("SetSuspendState")

	pPdhOpenQuery              = pdh.NewProc("PdhOpenQueryW")
	pPdhAddEnglishCounter      = pdh.NewProc("PdhAddEnglishCounterW")
	pPdhCollectQueryData       = pdh.NewProc("PdhCollectQueryData")
	pPdhGetFormattedCounterArr = pdh.NewProc("PdhGetFormattedCounterArrayW")
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001

	pdhFmtDouble = 0x00000200
	pdhMoreData  = 0x800007D2
)

// Host is the real Windows machine.
type Host struct {
	cpuPct atomic.Int32

	keepOn   atomic.Bool
	keepMu   sync.Mutex // keeps keepOn and the thread's execution state in step
	keepOnce sync.Once
	keepCh   chan bool

	mu         sync.Mutex
	shutdownAt *time.Time
	thermal    *thermalQuery
	screen     *screenState
}

// New starts the background CPU sampler and opens the thermal counters.
func New() *Host {
	h := &Host{keepCh: make(chan bool, 1), thermal: newThermalQuery(), screen: newScreenState()}
	go h.sampleCPU()
	startEvents(h.screen)
	return h
}

// ---- CPU, memory, disks, uptime ------------------------------------------

type fileTime struct{ low, high uint32 }

func (f fileTime) u64() uint64 { return uint64(f.high)<<32 | uint64(f.low) }

func systemTimes() (idle, kernel, user uint64, ok bool) {
	var i, k, u fileTime
	r, _, _ := pGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	return i.u64(), k.u64(), u.u64(), r != 0
}

// sampleCPU keeps a running CPU percentage (the kernel time includes idle).
func (h *Host) sampleCPU() {
	pi, pk, pu, ok := systemTimes()
	for range time.Tick(2 * time.Second) {
		i, k, u, ok2 := systemTimes()
		if ok && ok2 {
			total := (k - pk) + (u - pu)
			if total > 0 {
				busy := float64(total-(i-pi)) / float64(total) * 100
				h.cpuPct.Store(int32(math.Round(math.Max(0, math.Min(100, busy)))))
			}
		}
		pi, pk, pu, ok = i, k, u, ok2
	}
}

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys, availPhys uint64
	totalPage, availPage uint64
	totalVirt, availVirt uint64
	availExtVirt         uint64
}

func memory() Memory {
	ms := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := pGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return Memory{}
	}
	const mb = 1 << 20
	return Memory{
		TotalMB: int(ms.totalPhys / mb),
		UsedMB:  int((ms.totalPhys - ms.availPhys) / mb),
		Percent: int(ms.memoryLoad),
	}
}

func disks() []Disk {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []Disk
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := windows.UTF16PtrFromString(root)
		if windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		var avail, total, free uint64
		if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil || total == 0 {
			continue
		}
		const gb = 1 << 30
		used := total - free
		out = append(out, Disk{
			Name:    root[:2],
			TotalGB: round1(float64(total) / gb),
			FreeGB:  round1(float64(free) / gb),
			Percent: int(math.Round(float64(used) / float64(total) * 100)),
		})
	}
	return out
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func uptime() int64 {
	r, _, _ := pGetTickCount64.Call()
	return int64(r / 1000)
}

// ---- battery -------------------------------------------------------------------

type systemPowerStatus struct {
	acLine         byte
	batteryFlag    byte
	batteryPercent byte
	systemFlag     byte
	lifeTime       uint32
	fullLifeTime   uint32
}

func battery() *Battery {
	var s systemPowerStatus
	if r, _, _ := pGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&s))); r == 0 {
		return nil
	}
	if s.batteryFlag&128 != 0 || s.batteryFlag == 255 && s.batteryPercent == 255 {
		return nil // no battery (or unknown): a desktop
	}
	b := &Battery{
		Percent:     -1,
		OnAC:        s.acLine == 1,
		Charging:    s.batteryFlag&8 != 0,
		MinutesLeft: -1,
		Saver:       s.systemFlag&1 != 0,
	}
	if s.batteryPercent <= 100 {
		b.Percent = int(s.batteryPercent)
	}
	if s.lifeTime != 0xFFFFFFFF {
		b.MinutesLeft = int(s.lifeTime / 60)
	}
	return b
}

// ---- temperature (PDH "Thermal Zone Information") ---------------------------------------

type thermalQuery struct {
	query, temp, limit uintptr
}

func newThermalQuery() *thermalQuery {
	var q uintptr
	if r, _, _ := pPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		return nil
	}
	add := func(path string) uintptr {
		p, _ := windows.UTF16PtrFromString(path)
		var c uintptr
		if r, _, _ := pPdhAddEnglishCounter.Call(q, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&c))); r != 0 {
			return 0
		}
		return c
	}
	t := &thermalQuery{query: q, temp: add(`\Thermal Zone Information(*)\High Precision Temperature`), limit: add(`\Thermal Zone Information(*)\% Passive Limit`)}
	if t.temp == 0 {
		return nil // this machine exposes no ACPI thermal zone
	}
	return t
}

// counterValues returns every instance's value of a wildcard counter.
func counterValues(counter uintptr) []float64 {
	if counter == 0 {
		return nil
	}
	var size, count uint32
	r, _, _ := pPdhGetFormattedCounterArr.Call(counter, pdhFmtDouble, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if uint32(r) != pdhMoreData || size == 0 {
		return nil
	}
	buf := make([]uint64, (size+7)/8) // 8-byte aligned
	r, _, _ = pPdhGetFormattedCounterArr.Call(counter, pdhFmtDouble, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 || count == 0 || uint64(count)*24 > uint64(size) {
		return nil // never trust a count larger than the buffer
	}
	// PDH_FMT_COUNTERVALUE_ITEM_W: name pointer, then {status, padding, double}.
	type item struct {
		name   uintptr
		status uint32
		_      uint32
		value  float64
	}
	items := unsafe.Slice((*item)(unsafe.Pointer(&buf[0])), int(count))
	var out []float64
	for _, it := range items {
		if it.status == 0 || it.status == 1 { // valid data / new data
			out = append(out, it.value)
		}
	}
	return out
}

func (t *thermalQuery) read() *Thermal {
	if t == nil {
		return nil
	}
	if r, _, _ := pPdhCollectQueryData.Call(t.query); r != 0 {
		return nil
	}
	temps := counterValues(t.temp)
	if len(temps) == 0 {
		return nil
	}
	hottest := temps[0]
	for _, v := range temps {
		if v > hottest {
			hottest = v
		}
	}
	celsius := hottest/10 - 273.15 // tenths of a kelvin
	if celsius < -50 || celsius > 200 {
		return nil // nonsense reading
	}
	limit := 100
	for _, v := range counterValues(t.limit) {
		if int(v) < limit {
			limit = int(v)
		}
	}
	return &Thermal{Celsius: round1(celsius), PassiveLimit: limit, Throttled: limit < 100}
}

// ---- screen lock ----------------------------------------------------------------------------

// locked reports whether the input desktop is unreachable, which is the case
// on the lock screen.
func locked() bool {
	h, _, _ := pOpenInputDesktop.Call(0, 0, 0x0100) // DESKTOP_SWITCHDESKTOP
	if h == 0 {
		return true
	}
	pCloseDesktop.Call(h)
	return false
}

// ---- Machine ----------------------------------------------------------------------------------

func (h *Host) Status() (Status, error) {
	h.mu.Lock()
	var sd *time.Time
	if h.shutdownAt != nil && time.Now().Before(*h.shutdownAt) {
		t := *h.shutdownAt
		sd = &t
	}
	h.mu.Unlock()
	scr := h.screen.snapshot()
	// Lock events are authoritative once seen; before the first one, fall back
	// to asking whether the input desktop is reachable.
	isLocked := scr.Locked
	if !scr.LockedKnown {
		isLocked = locked()
	}
	var sleptAt, resumedAt *time.Time
	if !scr.SuspendedAt.IsZero() {
		t := scr.SuspendedAt
		sleptAt = &t
	}
	if !scr.ResumedAt.IsZero() {
		t := scr.ResumedAt
		resumedAt = &t
	}
	return Status{
		Display:    scr.Display,
		Lid:        scr.Lid,
		IdleSec:    idleSeconds(),
		SleptAt:    sleptAt,
		ResumedAt:  resumedAt,
		Battery:    battery(),
		Thermal:    h.thermal.read(),
		CPUPercent: int(h.cpuPct.Load()),
		Memory:     memory(),
		Disks:      disks(),
		UptimeSec:  uptime(),
		Locked:     isLocked,
		KeepAwake:  h.keepOn.Load(),
		ShutdownAt: sd,
	}, nil
}

func (h *Host) Lock() error {
	if r, _, err := pLockWorkStation.Call(); r == 0 {
		return fmt.Errorf("LockWorkStation: %w", err)
	}
	return nil
}

func (h *Host) Sleep(hibernate bool) error {
	var hib uintptr
	if hibernate {
		hib = 1
	}
	if r, _, err := pSetSuspendState.Call(hib, 0, 0); r == 0 {
		return fmt.Errorf("SetSuspendState: %w", err)
	}
	return nil
}

func shutdownExe() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "shutdown.exe")
}

// Shutdown schedules a shutdown or restart. Every argument is fixed or a
// validated number: nothing from a client is ever placed on the command line.
func (h *Host) Shutdown(restart bool, delaySec int, force bool) error {
	if delaySec < 10 || delaySec > 600 {
		return errors.New("delay must be 10-600 seconds")
	}
	arg := "/s"
	if restart {
		arg = "/r"
	}
	args := []string{arg, "/t", strconv.Itoa(delaySec), "/c", "claude-remote: requested from the phone"}
	if force {
		args = append(args, "/f")
	}
	cmd := exec.Command(shutdownExe(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("shutdown.exe: %w: %s", err, trim(out))
	}
	at := time.Now().Add(time.Duration(delaySec) * time.Second)
	h.mu.Lock()
	h.shutdownAt = &at
	h.mu.Unlock()
	return nil
}

func (h *Host) CancelShutdown() error {
	cmd := exec.Command(shutdownExe(), "/a")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	h.mu.Lock()
	h.shutdownAt = nil
	h.mu.Unlock()
	if err != nil {
		return fmt.Errorf("shutdown.exe /a: %w: %s", err, trim(out))
	}
	return nil
}

func trim(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}

// SetKeepAwake asks Windows not to sleep by itself. The execution state belongs
// to the thread that sets it, so one goroutine is pinned to its OS thread for
// the life of the process.
func (h *Host) SetKeepAwake(on bool) error {
	h.keepOnce.Do(func() {
		go func() {
			runtime.LockOSThread()
			for on := range h.keepCh {
				state := uintptr(esContinuous)
				if on {
					state |= esSystemRequired
				}
				pSetThreadExecutionState.Call(state)
			}
		}()
	})
	h.keepMu.Lock()
	defer h.keepMu.Unlock()
	h.keepOn.Store(on)
	h.keepCh <- on
	return nil
}

func (h *Host) KeepAwake() bool { return h.keepOn.Load() }
