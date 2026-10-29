// Package auth implements token authentication and brute-force protection.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CookieName is the cookie holding a random session id (never the token:
// cookies are not port-isolated, so another local web service on the same
// host would otherwise receive the long-lived secret).
const CookieName = "cr_session"

// SessionTTL is how long a login stays valid.
const SessionTTL = 14 * 24 * time.Hour

// Result of an authentication attempt.
type Result int

const (
	// NoCredentials: nothing presented; not counted as a failed attempt.
	NoCredentials Result = iota
	OK
	Invalid
	Blocked
)

type Auth struct {
	tokenHash [32]byte
	Limiter   *Limiter

	// guessMu makes "is the IP blocked?" + "record the failure" atomic, so a
	// parallel burst cannot all pass the check before the first failure counts.
	guessMu  sync.Mutex
	mu       sync.Mutex
	sessions map[[32]byte]time.Time // sha256(session id) -> expiry; memory only
}

// New creates an Auth. Sessions live in memory, so restarting the server (as
// required after `token rotate`) logs everyone out.
func New(token string) *Auth {
	return &Auth{
		tokenHash: sha256.Sum256([]byte(token)),
		Limiter:   NewLimiter(),
		sessions:  make(map[[32]byte]time.Time),
	}
}

// NewSession issues a random session id after a successful login.
func (a *Auth) NewSession() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	a.sessions[sha256.Sum256([]byte(id))] = now.Add(SessionTTL)
	return id, nil
}

// RevokeSession ends one login (logout on this device).
func (a *Auth) RevokeSession(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, sha256.Sum256([]byte(id)))
}

// RevokeAll ends every login (logout everywhere).
func (a *Auth) RevokeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = make(map[[32]byte]time.Time)
}

func (a *Auth) validSession(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[sha256.Sum256([]byte(id))]
	return ok && time.Now().Before(exp)
}

// CheckToken compares in constant time. Both sides are hashed first so the
// comparison does not leak the token length either.
func (a *Auth) CheckToken(got string) bool {
	h := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(h[:], a.tokenHash[:]) == 1
}

// ClientIP uses only the TCP peer address. X-Forwarded-For is never trusted:
// there is no proxy in front, so any such header is attacker-controlled.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// BearerToken extracts the token from the Authorization header. The query
// string is deliberately ignored: URLs end up in browser history and logs.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return h[len(p):]
	}
	return ""
}

// Authenticate checks the request and updates the rate limiter. A blocked IP
// is refused even with a correct token, so lockout cannot be probed.
// retryAfter is set only for Blocked.
//
// Only token guesses (Bearer, login form) count toward the limit. An unknown
// session cookie is rejected without counting: ids are 256-bit random so they
// cannot be guessed, and a stale cookie (e.g. after `token rotate`) must not
// lock the owner out.
func (a *Auth) Authenticate(r *http.Request) (res Result, retryAfter time.Duration) {
	ip := ClientIP(r)
	if d, blocked := a.Limiter.Blocked(ip); blocked {
		return Blocked, d
	}
	if tok := BearerToken(r); tok != "" {
		return a.guess(ip, tok)
	}
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		if a.validSession(c.Value) {
			return OK, 0
		}
		return Invalid, 0
	}
	return NoCredentials, 0
}

// Login validates a token submitted via the login form.
func (a *Auth) Login(r *http.Request, token string) (res Result, retryAfter time.Duration) {
	return a.guess(ClientIP(r), token)
}

func (a *Auth) guess(ip, tok string) (Result, time.Duration) {
	a.guessMu.Lock()
	defer a.guessMu.Unlock()
	if d, blocked := a.Limiter.Blocked(ip); blocked {
		return Blocked, d
	}
	return a.checkAndRecord(ip, tok), 0
}

func (a *Auth) checkAndRecord(ip, tok string) Result {
	if a.CheckToken(tok) {
		a.Limiter.Success(ip)
		return OK
	}
	a.Limiter.Fail(ip)
	return Invalid
}
