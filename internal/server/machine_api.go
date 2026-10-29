package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"claude-remote/internal/auth"
	"claude-remote/internal/machine"
)

// Power actions need two steps: prepare returns a one-time code, execute
// presents it (plus the PIN when one is set). A single stray tap, a repeated
// request or a replayed call therefore cannot put the laptop to sleep or turn
// it off. Package variables so tests can shorten them.
var (
	powerCodeTTL = 30 * time.Second
	// powerActDelay is how long after the HTTP reply a sleep or hibernate
	// really starts, so the phone receives the confirmation first.
	powerActDelay = 1500 * time.Millisecond
)

// shutdownDelaySec is fixed: long enough to cancel from the phone.
const shutdownDelaySec = 30

// maxPendingCodes bounds the codes waiting to be used (they also expire).
const maxPendingCodes = 64

type powerAction struct {
	hibernate    bool
	restart      bool
	shutdown     bool
	irreversible bool // cannot be cancelled and cannot be woken remotely
}

var powerActions = map[string]powerAction{
	"sleep":     {irreversible: true},
	"hibernate": {hibernate: true, irreversible: true},
	"shutdown":  {shutdown: true},
	"restart":   {shutdown: true, restart: true},
}

type pendingPower struct {
	action  string
	caller  [32]byte
	expires time.Time
}

type powerState struct {
	mu    sync.Mutex
	codes map[string]pendingPower
}

// callerID binds a confirmation code to whoever asked for it: the login
// session cookie, or "bearer" for token clients.
func callerID(r *http.Request) [32]byte {
	if c, err := r.Cookie(auth.CookieName); err == nil && auth.BearerToken(r) == "" {
		return sha256.Sum256([]byte("cookie:" + c.Value))
	}
	return sha256.Sum256([]byte("bearer"))
}

func (s *Server) machineOr501(w http.ResponseWriter) bool {
	if s.o.Machine == nil {
		writeErr(w, http.StatusNotImplemented, "machine_unavailable")
		return false
	}
	return true
}

// handleMachine returns the laptop's state: battery, temperature, load, disks.
func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request) {
	if !s.machineOr501(w) {
		return
	}
	st, err := s.o.Machine.Status()
	if err != nil {
		s.o.Log.Warn("machine status failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "machine_status_failed")
		return
	}
	writeJSON2(w, http.StatusOK, struct {
		machine.Status
		PowerEnabled bool `json:"power_enabled"` // sleep/hibernate/shutdown/restart are offered
	}{st, !s.o.NoPower})
}

// handleMachineLock locks the screen. Reversible and not destructive, so it is
// one step; note that a locked screen shows the lock screen in screenshots.
func (s *Server) handleMachineLock(w http.ResponseWriter, r *http.Request) {
	if !s.machineOr501(w) {
		return
	}
	if err := s.o.Machine.Lock(); err != nil {
		s.o.Log.Error("lock screen failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "power_failed")
		return
	}
	s.o.Log.Warn("power action", "ip", auth.ClientIP(r), "action", "lock")
	writeJSON2(w, http.StatusOK, map[string]string{"status": "locked"})
}

func (s *Server) handleKeepAwake(w http.ResponseWriter, r *http.Request) {
	if !s.machineOr501(w) {
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.o.Machine.SetKeepAwake(req.On); err != nil {
		writeErr(w, http.StatusInternalServerError, "power_failed")
		return
	}
	s.o.Log.Info("keep awake", "ip", auth.ClientIP(r), "on", req.On)
	writeJSON2(w, http.StatusOK, map[string]bool{"keep_awake": req.On})
}

// handlePowerPrepare is step one of a destructive power action.
func (s *Server) handlePowerPrepare(w http.ResponseWriter, r *http.Request) {
	if !s.machineOr501(w) {
		return
	}
	if s.o.NoPower {
		writeErr(w, http.StatusForbidden, "power_disabled")
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	act, ok := powerActions[req.Action]
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_action")
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	code := hex.EncodeToString(b)
	now := time.Now()
	s.power.mu.Lock()
	if s.power.codes == nil {
		s.power.codes = map[string]pendingPower{}
	}
	for k, p := range s.power.codes { // drop expired codes
		if now.After(p.expires) {
			delete(s.power.codes, k)
		}
	}
	if len(s.power.codes) >= maxPendingCodes {
		s.power.mu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts")
		return
	}
	s.power.codes[code] = pendingPower{action: req.Action, caller: callerID(r), expires: now.Add(powerCodeTTL)}
	s.power.mu.Unlock()

	needPIN := s.pinRequired()
	writeJSON2(w, http.StatusOK, map[string]any{
		"code":         code,
		"expires_in":   int(powerCodeTTL / time.Second),
		"needs_pin":    needPIN,
		"irreversible": act.irreversible,
		"delay_sec":    shutdownDelaySec,
	})
}

// handlePowerExecute is step two: it needs the code from step one, issued to
// this same caller for this same action, and the PIN when one is set.
func (s *Server) handlePowerExecute(w http.ResponseWriter, r *http.Request) {
	if !s.machineOr501(w) {
		return
	}
	if s.o.NoPower {
		writeErr(w, http.StatusForbidden, "power_disabled")
		return
	}
	var req struct {
		Action string `json:"action"`
		Code   string `json:"code"`
		PIN    string `json:"pin"`
		Force  bool   `json:"force"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	act, ok := powerActions[req.Action]
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_action")
		return
	}
	caller := callerID(r)
	s.power.mu.Lock()
	p, found := s.power.codes[req.Code]
	valid := found && p.action == req.Action && p.caller == caller && time.Now().Before(p.expires)
	s.power.mu.Unlock()
	if !valid {
		writeErr(w, http.StatusBadRequest, "bad_code")
		return
	}
	// The PIN is asked again for these actions: the page being unlocked a
	// minute ago is not enough to switch the machine off. It applies to token
	// (Bearer) callers too, so a stolen token alone cannot turn the laptop off
	// when a PIN is set.
	if s.pinRequired() {
		if _, ok := s.verifyPIN(w, r, powerPINSession(r), req.PIN); !ok {
			return // the code stays valid for another try until it expires
		}
	}
	s.power.mu.Lock()
	cur, still := s.power.codes[req.Code]
	if still && cur == p {
		delete(s.power.codes, req.Code) // single use
	}
	s.power.mu.Unlock()
	if !still || cur != p {
		writeErr(w, http.StatusBadRequest, "bad_code") // used by a parallel request
		return
	}

	ip := auth.ClientIP(r)
	s.o.Log.Warn("power action", "ip", ip, "action", req.Action, "force", req.Force)
	if act.shutdown {
		if err := s.o.Machine.Shutdown(act.restart, shutdownDelaySec, req.Force); err != nil {
			s.o.Log.Error("shutdown failed", "err", err)
			writeErr(w, http.StatusInternalServerError, "power_failed")
			return
		}
		writeJSON2(w, http.StatusOK, map[string]any{"status": "scheduled", "delay_sec": shutdownDelaySec, "cancellable": true})
		return
	}
	// Sleep and hibernate start after the reply has been sent.
	hib := act.hibernate
	time.AfterFunc(powerActDelay, func() {
		if err := s.o.Machine.Sleep(hib); err != nil {
			s.o.Log.Error("sleep failed", "err", err)
		}
	})
	writeJSON2(w, http.StatusOK, map[string]any{"status": "scheduled", "in_ms": int(powerActDelay / time.Millisecond), "cancellable": false})
}

// handlePowerCancel aborts a pending shutdown or restart.
func (s *Server) handlePowerCancel(w http.ResponseWriter, r *http.Request) {
	if !s.machineOr501(w) {
		return
	}
	if err := s.o.Machine.CancelShutdown(); err != nil {
		s.o.Log.Warn("cancel shutdown failed", "err", err)
		writeErr(w, http.StatusConflict, "nothing_to_cancel")
		return
	}
	s.o.Log.Warn("power action", "ip", auth.ClientIP(r), "action", "cancel")
	writeJSON2(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// pinRequired reports whether destructive power actions must be confirmed with
// the PIN: whenever a PIN is set, whoever the caller is.
func (s *Server) pinRequired() bool { return s.o.Lock != nil && s.o.Lock.Enabled() }

// powerPINSession identifies the caller for the PIN failure counters: the login
// session for the browser, one shared identity for token clients.
func powerPINSession(r *http.Request) string {
	if c, err := r.Cookie(auth.CookieName); err == nil && auth.BearerToken(r) == "" {
		return c.Value
	}
	return "bearer"
}
