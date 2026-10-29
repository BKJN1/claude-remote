package server

import (
	"net/http"
	"strings"
	"time"

	"claude-remote/internal/auth"
	"claude-remote/internal/lock"
)

// lockProto is the WebSocket subprotocol that carries the unlock key.
const lockProto = "cr-unlock"

// lockCreds extracts what the PIN gate needs. need is false when no PIN is set
// or the request is authenticated by the Bearer token (scripts and tools; the
// PIN guards the browser UI, which logs in with a cookie).
func (s *Server) lockCreds(r *http.Request) (session, key string, need bool) {
	if s.o.Lock == nil || auth.BearerToken(r) != "" || !s.o.Lock.Enabled() {
		return "", "", false
	}
	if c, err := r.Cookie(auth.CookieName); err == nil {
		session = c.Value
	}
	key = r.Header.Get("X-Unlock")
	if key == "" {
		// WebSocket: "Sec-WebSocket-Protocol: cr-unlock, <key>"
		parts := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
		for i, p := range parts {
			if strings.TrimSpace(p) == lockProto && i+1 < len(parts) {
				key = strings.TrimSpace(parts[i+1])
			}
		}
	}
	return session, key, true
}

// unlocked answers 423 Locked unless the PIN gate is open for this request.
func (s *Server) unlocked(w http.ResponseWriter, r *http.Request) bool {
	sess, key, need := s.lockCreds(r)
	if !need {
		return true
	}
	// The page's own background polling sends X-Passive: it must not keep the
	// idle timer alive, only real activity (input, actions, the WebSocket) does.
	if r.Method == http.MethodGet && r.Header.Get("X-Passive") == "1" {
		if s.o.Lock.Valid(sess, key) {
			return true
		}
	} else if s.o.Lock.Check(sess, key) {
		return true
	}
	writeErr(w, http.StatusLocked, "locked")
	return false
}

// gate wraps a non-API handler (WebSocket, screenshot) with the PIN gate.
func (s *Server) gate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.unlocked(w, r) {
			h(w, r)
		}
	}
}

func (s *Server) handleLockStatus(w http.ResponseWriter, r *http.Request) {
	sess, key, need := s.lockCreds(r)
	idle := 0
	if s.o.Lock != nil {
		idle = int(s.o.Lock.Idle() / time.Second)
	}
	writeJSON2(w, http.StatusOK, map[string]any{
		"enabled":      need,
		"unlocked":     !need || s.o.Lock.Valid(sess, key),
		"idle_seconds": idle,
	})
}

func (s *Server) handleLockUnlock(w http.ResponseWriter, r *http.Request) {
	sess, _, need := s.lockCreds(r)
	if !need {
		writeErr(w, http.StatusBadRequest, "lock_disabled")
		return
	}
	var req struct {
		PIN string `json:"pin"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	key, ok := s.verifyPIN(w, r, sess, req.PIN)
	if !ok {
		return
	}
	s.o.Log.Info("unlocked", "ip", auth.ClientIP(r))
	writeJSON2(w, http.StatusOK, map[string]any{"key": key, "idle_seconds": int(s.o.Lock.Idle() / time.Second)})
}

// verifyPIN checks a PIN with every limit applied. On failure it has already
// written the response. It is used by the unlock screen and by the power
// actions, so both share one lockout.
func (s *Server) verifyPIN(w http.ResponseWriter, r *http.Request, sess, pin string) (key string, ok bool) {
	// One PIN attempt at a time: the limiter check and its update are atomic,
	// and at most one memory-hungry hash runs.
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	ip := auth.ClientIP(r)
	// A PIN is short, so guesses are limited per IP (exponential lockout, like
	// the token) and per login session (see lock.MaxFailures).
	limiterKey := "pin:" + ip
	if d, blocked := s.o.Auth.Limiter.Blocked(limiterKey); blocked {
		s.o.Log.Warn("pin blocked", "ip", ip, "retry_after", d.Round(time.Second))
		w.Header().Set("Retry-After", itoa(int(d.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts")
		return "", false
	}
	key, tooMany, err := s.o.Lock.Unlock(sess, pin)
	switch {
	case err == lock.ErrBadPIN:
		s.o.Auth.Limiter.Fail(limiterKey)
		s.o.Log.Warn("pin failed", "ip", ip)
		if tooMany {
			// End this login: the token must be entered again.
			s.o.Auth.RevokeSession(sess)
			s.o.Lock.Forget(sess)
			s.o.Log.Warn("pin failure limit reached, login ended", "ip", ip)
			writeErr(w, http.StatusUnauthorized, "session_ended")
			return "", false
		}
		writeErr(w, http.StatusForbidden, "bad_pin")
		return "", false
	case err != nil:
		s.o.Log.Error("unlock failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return "", false
	}
	s.o.Auth.Limiter.Success(limiterKey)
	return key, true
}

// handleLockNow locks this login session immediately.
func (s *Server) handleLockNow(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil && s.o.Lock != nil {
		s.o.Lock.Lock(c.Value)
	}
	writeJSON2(w, http.StatusOK, map[string]string{"status": "locked"})
}
