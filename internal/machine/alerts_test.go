package machine

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type scripted struct {
	st  Status
	err error
}

func newAlerter(s *scripted) (*Alerter, *[]Alert, *time.Time) {
	var got []Alert
	now := time.Unix(1_000_000, 0)
	a := NewAlerter(func() (Status, error) { return s.st, s.err }, func(al Alert) { got = append(got, al) })
	a.now = func() time.Time { return now }
	return a, &got, &now
}

func kinds(als []Alert) string {
	var k []string
	for _, a := range als {
		k = append(k, a.Kind)
	}
	return strings.Join(k, ",")
}

func TestLowBatteryFiresOnceAndRearms(t *testing.T) {
	s := &scripted{st: Status{Battery: &Battery{Percent: 40}}}
	a, got, now := newAlerter(s)
	a.Check()
	if len(*got) != 0 {
		t.Fatalf("fired at 40%%: %v", kinds(*got))
	}
	s.st.Battery.Percent = 19
	a.Check()
	a.Check() // still low: must stay quiet
	if kinds(*got) != "battery-low" {
		t.Fatalf("got %q", kinds(*got))
	}
	s.st.Battery.Percent = 9
	a.Check()
	if kinds(*got) != "battery-low,battery-critical" {
		t.Fatalf("critical level not reported: %q", kinds(*got))
	}
	// Plugging in clears the condition; a later drop fires again.
	s.st.Battery.OnAC = true
	a.Check()
	s.st.Battery.OnAC = false
	s.st.Battery.Percent = 18
	*now = now.Add(time.Minute)
	a.Check()
	if kinds(*got) != "battery-low,battery-critical,ac-lost,battery-low" {
		t.Fatalf("got %q", kinds(*got))
	}
}

func TestNoLowBatteryAlertWhilePluggedIn(t *testing.T) {
	s := &scripted{st: Status{Battery: &Battery{Percent: 5, OnAC: true, Charging: true}}}
	a, got, _ := newAlerter(s)
	a.Check()
	if len(*got) != 0 {
		t.Fatalf("alerted while charging: %v", kinds(*got))
	}
}

func TestLosingMainsPowerAlertsOnlyOnTheTransition(t *testing.T) {
	s := &scripted{st: Status{Battery: &Battery{Percent: 80, OnAC: false}}}
	a, got, _ := newAlerter(s)
	a.Check() // first reading, already on battery: no transition seen
	if len(*got) != 0 {
		t.Fatalf("alerted without a transition: %v", kinds(*got))
	}
	s.st.Battery.OnAC = true
	a.Check()
	s.st.Battery.OnAC = false
	a.Check()
	if kinds(*got) != "ac-lost" {
		t.Fatalf("got %q", kinds(*got))
	}
}

func TestOverheatAndThrottling(t *testing.T) {
	s := &scripted{st: Status{Thermal: &Thermal{Celsius: 60, PassiveLimit: 100}}}
	a, got, now := newAlerter(s)
	a.Check()
	if len(*got) != 0 {
		t.Fatal("alerted at 60 C")
	}
	s.st.Thermal = &Thermal{Celsius: 88, PassiveLimit: 100}
	a.Check()
	a.Check()
	if kinds(*got) != "hot" {
		t.Fatalf("got %q", kinds(*got))
	}
	// Throttling alone is enough, once the earlier alert has cooled down.
	s.st.Thermal = &Thermal{Celsius: 70, PassiveLimit: 60, Throttled: true}
	*now = now.Add(20 * time.Minute)
	a.Check()
	if kinds(*got) != "hot,hot" {
		t.Fatalf("throttling not reported: %q", kinds(*got))
	}
}

func TestLowDiskSpace(t *testing.T) {
	s := &scripted{st: Status{Disks: []Disk{{Name: "C:", FreeGB: 2.5, TotalGB: 500, Percent: 99}, {Name: "D:", FreeGB: 300, TotalGB: 500}, {Name: "E:", FreeGB: 0.4, TotalGB: 0.5, Percent: 20}}}}
	a, got, _ := newAlerter(s)
	a.Check()
	a.Check()
	if kinds(*got) != "disk:C:" || !strings.Contains((*got)[0].Body, "2.5") {
		t.Fatalf("got %v", *got)
	}
}

func TestStatusErrorIsIgnored(t *testing.T) {
	s := &scripted{err: errors.New("boom")}
	a, got, _ := newAlerter(s)
	a.Check()
	if len(*got) != 0 {
		t.Fatal("alert from a failed reading")
	}
}

func TestCachedStatusComputesOncePerTTL(t *testing.T) {
	m := &countingMachine{}
	c := NewCached(m, time.Hour)
	for i := 0; i < 5; i++ {
		c.Status()
	}
	if m.calls != 1 {
		t.Fatalf("status computed %d times", m.calls)
	}
	m.keep = true
	if st, _ := c.Status(); !st.KeepAwake {
		t.Fatal("the keep-awake switch must show immediately, not after the cache expires")
	}
}

type countingMachine struct {
	calls int
	keep  bool
}

func (m *countingMachine) Status() (Status, error)        { m.calls++; return Status{}, nil }
func (m *countingMachine) Lock() error                    { return nil }
func (m *countingMachine) Sleep(bool) error               { return nil }
func (m *countingMachine) Shutdown(bool, int, bool) error { return nil }
func (m *countingMachine) CancelShutdown() error          { return nil }
func (m *countingMachine) SetKeepAwake(on bool) error     { m.keep = on; return nil }
func (m *countingMachine) KeepAwake() bool                { return m.keep }
