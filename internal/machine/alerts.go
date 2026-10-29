package machine

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Alert is a notification about the laptop's state.
type Alert struct {
	Kind  string // stable id, also used as the push "tag"
	Title string
	Body  string
}

// Thresholds. They are deliberately fixed: the point is a heads-up, not a
// monitoring system.
const (
	lowBatteryPct      = 20
	criticalBatteryPct = 10
	rearmBatteryPct    = 25
	hotCelsius         = 85.0
	coolCelsius        = 75.0
	lowDiskGB          = 5.0
	minDiskGB          = 20.0 // smaller volumes are ignored
)

// Alerter turns status changes into edge-triggered alerts. Each kind fires
// once, then stays quiet until the condition clears (or its cooldown passes),
// so a laptop sitting at 18% does not buzz every minute.
type Alerter struct {
	status func() (Status, error)
	notify func(Alert)
	now    func() time.Time

	mu       sync.Mutex
	active   map[string]time.Time // kind -> when it last fired
	prevOnAC *bool
}

func NewAlerter(status func() (Status, error), notify func(Alert)) *Alerter {
	return &Alerter{status: status, notify: notify, now: time.Now, active: map[string]time.Time{}}
}

// Run checks every interval until ctx ends.
func (a *Alerter) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		a.Check()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// fire sends an alert unless the same kind fired within cooldown. It returns
// without locking: callers hold a.mu.
func (a *Alerter) fire(al Alert, cooldown time.Duration) {
	if last, ok := a.active[al.Kind]; ok && a.now().Sub(last) < cooldown {
		return
	}
	a.active[al.Kind] = a.now()
	a.notify(al)
}

func (a *Alerter) clear(kinds ...string) {
	for _, k := range kinds {
		delete(a.active, k)
	}
}

// Check evaluates the current status once.
func (a *Alerter) Check() {
	st, err := a.status()
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if b := st.Battery; b != nil {
		onAC := b.OnAC
		if a.prevOnAC != nil && *a.prevOnAC && !onAC {
			a.fire(Alert{Kind: "ac-lost", Title: "Ноутбук перешёл на батарею",
				Body: fmt.Sprintf("Питание от сети пропало, заряд %d%%", b.Percent)}, 10*time.Minute)
		}
		a.prevOnAC = &onAC

		switch {
		case onAC || b.Percent < 0 || b.Percent > rearmBatteryPct:
			a.clear("battery-low", "battery-critical")
		case b.Percent <= criticalBatteryPct:
			a.fire(Alert{Kind: "battery-critical", Title: fmt.Sprintf("Ноутбук: заряд %d%%", b.Percent),
				Body: "Критически низкий заряд: подключите питание или отправьте ноутбук в сон"}, time.Hour)
		case b.Percent <= lowBatteryPct:
			a.fire(Alert{Kind: "battery-low", Title: fmt.Sprintf("Ноутбук: заряд %d%%", b.Percent),
				Body: "Низкий заряд, ноутбук не подключён к питанию"}, time.Hour)
		}
	}

	if t := st.Thermal; t != nil {
		switch {
		case t.Celsius >= hotCelsius || t.Throttled:
			a.fire(Alert{Kind: "hot", Title: fmt.Sprintf("Ноутбук горячий: %.0f °C", t.Celsius),
				Body: "Температура высокая или процессор снижает частоту"}, 15*time.Minute)
		case t.Celsius < coolCelsius:
			a.clear("hot")
		}
	}

	for _, d := range st.Disks {
		kind := "disk:" + d.Name
		if d.TotalGB < minDiskGB {
			continue // service partitions (EFI, recovery) are always nearly full
		}
		if d.FreeGB < lowDiskGB {
			a.fire(Alert{Kind: kind, Title: fmt.Sprintf("Мало места на диске %s", d.Name),
				Body: fmt.Sprintf("Свободно %.1f ГБ", d.FreeGB)}, 6*time.Hour)
		} else {
			a.clear(kind)
		}
	}
}
