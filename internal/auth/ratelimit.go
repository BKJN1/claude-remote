package auth

import (
	"sync"
	"time"
)

// Limiter blocks an IP after too many failed attempts in a window. Each
// consecutive lockout doubles the block time. State lives in memory only.
type Limiter struct {
	mu        sync.Mutex
	now       func() time.Time
	max       int
	window    time.Duration
	baseBlock time.Duration
	maxBlock  time.Duration
	m         map[string]*entry
}

type entry struct {
	fails        []time.Time
	blockedUntil time.Time
	strikes      int // number of lockouts so far; drives exponential growth
	lastSeen     time.Time
}

// NewLimiter: 5 failures per 5 minutes, then 1m, 2m, 4m ... up to 1h.
func NewLimiter() *Limiter {
	return &Limiter{
		now:       time.Now,
		max:       5,
		window:    5 * time.Minute,
		baseBlock: time.Minute,
		maxBlock:  time.Hour,
		m:         make(map[string]*entry),
	}
}

// Blocked reports whether ip is currently locked out and for how long.
func (l *Limiter) Blocked(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[ip]
	if e == nil {
		return 0, false
	}
	if d := e.blockedUntil.Sub(l.now()); d > 0 {
		return d, true
	}
	return 0, false
}

// Fail records a failed attempt.
func (l *Limiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.gc(now)
	e := l.m[ip]
	if e == nil {
		e = &entry{}
		l.m[ip] = e
	}
	e.lastSeen = now
	if now.Before(e.blockedUntil) {
		return
	}
	cutoff := now.Add(-l.window)
	kept := e.fails[:0]
	for _, t := range e.fails {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	e.fails = append(kept, now)
	if len(e.fails) >= l.max {
		block := l.baseBlock << min(e.strikes, 20)
		if block > l.maxBlock || block <= 0 {
			block = l.maxBlock
		}
		e.strikes++
		e.blockedUntil = now.Add(block)
		e.fails = nil
	}
}

// Success clears recent failures (but not the lockout history while blocked).
func (l *Limiter) Success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[ip]; e != nil {
		e.fails = nil
		if !l.now().Before(e.blockedUntil) {
			delete(l.m, ip)
		}
	}
}

// gc drops entries idle for over a day so the map cannot grow without bound.
func (l *Limiter) gc(now time.Time) {
	if len(l.m) < 256 {
		return
	}
	for ip, e := range l.m {
		if now.Sub(e.lastSeen) > 24*time.Hour && now.After(e.blockedUntil) {
			delete(l.m, ip)
		}
	}
}
