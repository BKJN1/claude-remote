//go:build windows

package machine

import (
	"strings"
	"testing"
	"time"
)

// These tests only READ state. None of them may call Sleep, Lock, Shutdown or
// CancelShutdown: they would act on the machine running the tests.

func TestHostStatusIsPlausible(t *testing.T) {
	h := New()
	time.Sleep(2500 * time.Millisecond) // let the CPU sampler take a reading
	st, err := h.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Memory.TotalMB <= 0 || st.Memory.UsedMB <= 0 || st.Memory.UsedMB > st.Memory.TotalMB || st.Memory.Percent < 0 || st.Memory.Percent > 100 {
		t.Fatalf("memory %+v", st.Memory)
	}
	if st.CPUPercent < 0 || st.CPUPercent > 100 {
		t.Fatalf("cpu %d", st.CPUPercent)
	}
	if st.UptimeSec <= 0 {
		t.Fatalf("uptime %d", st.UptimeSec)
	}
	if len(st.Disks) == 0 {
		t.Fatal("no fixed disks found")
	}
	for _, d := range st.Disks {
		if !strings.HasSuffix(d.Name, ":") || d.TotalGB <= 0 || d.FreeGB < 0 || d.FreeGB > d.TotalGB || d.Percent < 0 || d.Percent > 100 {
			t.Fatalf("disk %+v", d)
		}
	}
	if b := st.Battery; b != nil {
		if b.Percent < -1 || b.Percent > 100 {
			t.Fatalf("battery %+v", b)
		}
	}
	if th := st.Thermal; th != nil {
		if th.Celsius < 0 || th.Celsius > 130 || th.PassiveLimit < 0 || th.PassiveLimit > 100 {
			t.Fatalf("thermal %+v", th)
		}
	}
	t.Logf("battery=%+v thermal=%+v cpu=%d%% mem=%+v disks=%+v locked=%v", st.Battery, st.Thermal, st.CPUPercent, st.Memory, st.Disks, st.Locked)
}

func TestKeepAwakeToggles(t *testing.T) {
	h := New()
	if h.KeepAwake() {
		t.Fatal("on by default")
	}
	if err := h.SetKeepAwake(true); err != nil || !h.KeepAwake() {
		t.Fatal("not switched on")
	}
	if st, _ := h.Status(); !st.KeepAwake {
		t.Fatal("status does not show it")
	}
	// Always restore the machine's normal sleep behaviour.
	if err := h.SetKeepAwake(false); err != nil || h.KeepAwake() {
		t.Fatal("not switched off")
	}
}

func TestShutdownRejectsUnreasonableDelays(t *testing.T) {
	h := New()
	// These must fail BEFORE anything is executed.
	for _, d := range []int{-1, 0, 5, 601, 100000} {
		if err := h.Shutdown(false, d, false); err == nil {
			t.Errorf("delay %d accepted", d)
		}
	}
}
