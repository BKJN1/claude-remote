// Package machine reads the laptop's state (battery, temperature, load, disk)
// and performs power actions (lock, sleep, hibernate, shut down, restart).
package machine

import (
	"errors"
	"sync"
	"time"
)

// ErrUnsupported is returned on platforms without an implementation.
var ErrUnsupported = errors.New("not supported on this platform")

// Battery is the state of the main battery. Nil in Status on machines without one.
type Battery struct {
	Percent     int  `json:"percent"`      // 0-100, -1 if unknown
	OnAC        bool `json:"on_ac"`        // plugged in
	Charging    bool `json:"charging"`     // actively charging
	MinutesLeft int  `json:"minutes_left"` // estimated run time on battery, -1 if unknown
	Saver       bool `json:"saver"`        // battery saver is on
}

// Thermal is the hottest ACPI thermal zone Windows reports. It is a system
// zone, not necessarily the CPU die: Windows gives no CPU temperature without
// a driver.
type Thermal struct {
	Celsius      float64 `json:"celsius"`
	PassiveLimit int     `json:"passive_limit"` // 100 = not throttled
	Throttled    bool    `json:"throttled"`
}

// Memory usage in MiB.
type Memory struct {
	TotalMB int `json:"total_mb"`
	UsedMB  int `json:"used_mb"`
	Percent int `json:"percent"`
}

// Disk is one fixed drive.
type Disk struct {
	Name    string  `json:"name"` // "C:"
	TotalGB float64 `json:"total_gb"`
	FreeGB  float64 `json:"free_gb"`
	Percent int     `json:"percent"` // used, 0-100
}

// Status is a snapshot of the machine.
type Status struct {
	Battery    *Battery `json:"battery"`
	Thermal    *Thermal `json:"thermal"`
	CPUPercent int      `json:"cpu_percent"`
	Memory     Memory   `json:"memory"`
	Disks      []Disk   `json:"disks"`
	UptimeSec  int64    `json:"uptime_sec"`
	Locked     bool     `json:"locked"` // the session is locked (screenshots show the lock screen)
	// Display is "on", "dim", "off" or "unknown": a screen that is off or
	// asleep shows nothing, whether or not the session is locked.
	Display string `json:"display"`
	// Lid is "open", "closed" or "unknown".
	Lid string `json:"lid"`
	// IdleSec is the time since the last keyboard or mouse input (-1 unknown).
	IdleSec int `json:"idle_sec"`
	// SleptAt/ResumedAt: the last time the laptop went to sleep and woke up by
	// itself or by the lid. Lets the phone say "it slept while you were away".
	SleptAt   *time.Time `json:"slept_at,omitempty"`
	ResumedAt *time.Time `json:"resumed_at,omitempty"`
	KeepAwake bool       `json:"keep_awake"`
	// ShutdownAt is set while a shutdown or restart scheduled from here is pending.
	ShutdownAt *time.Time `json:"shutdown_at,omitempty"`
}

// Machine is what the server needs from the host.
type Machine interface {
	Status() (Status, error)
	Lock() error
	// Sleep suspends to RAM (hibernate=false) or to disk (hibernate=true).
	Sleep(hibernate bool) error
	// Shutdown schedules a shutdown or restart after delaySec seconds.
	Shutdown(restart bool, delaySec int, force bool) error
	// CancelShutdown aborts a pending shutdown or restart.
	CancelShutdown() error
	// SetKeepAwake stops (on=true) or allows (on=false) the system going to
	// sleep by itself while the server runs.
	SetKeepAwake(on bool) error
	KeepAwake() bool
}

// Cached wraps a Machine so Status is computed at most once per ttl,
// however many clients poll it.
type Cached struct {
	Machine
	ttl time.Duration

	mu   sync.Mutex
	at   time.Time
	last Status
	err  error
}

func NewCached(m Machine, ttl time.Duration) *Cached { return &Cached{Machine: m, ttl: ttl} }

func (c *Cached) Status() (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < c.ttl && !c.at.IsZero() {
		st := c.last
		st.KeepAwake = c.Machine.KeepAwake() // switches must show immediately
		return st, c.err
	}
	c.last, c.err = c.Machine.Status()
	c.at = time.Now()
	return c.last, c.err
}
