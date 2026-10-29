package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const tok = "correct-token-correct-token-correct-token-0123"

func req(ip string, mod func(*http.Request)) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = ip + ":5555"
	if mod != nil {
		mod(r)
	}
	return r
}

func bearer(t string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+t) }
}

func TestCorrectAndWrongToken(t *testing.T) {
	a := New(tok)
	if res, _ := a.Authenticate(req("1.1.1.1", bearer(tok))); res != OK {
		t.Fatalf("bearer: got %v", res)
	}
	id, err := a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := a.Authenticate(req("1.1.1.1", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: id})
	})); res != OK {
		t.Fatalf("session cookie: got %v", res)
	}
	// The raw token in the cookie must NOT authenticate.
	if res, _ := a.Authenticate(req("1.1.1.1", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	})); res == OK {
		t.Fatal("raw token accepted as cookie")
	}
	if res, _ := a.Authenticate(req("1.1.1.1", bearer("wrong"))); res != Invalid {
		t.Fatalf("wrong: got %v", res)
	}
	if res, _ := a.Authenticate(req("1.1.1.1", nil)); res != NoCredentials {
		t.Fatalf("none: got %v", res)
	}
}

func TestTokenInQueryRejected(t *testing.T) {
	a := New(tok)
	r := req("1.1.1.1", func(r *http.Request) { r.URL.RawQuery = "token=" + tok })
	if got := BearerToken(r); got != "" {
		t.Fatalf("query token accepted: %q", got)
	}
	if res, _ := a.Authenticate(r); res == OK {
		t.Fatal("authenticated via query string")
	}
}

func TestRateLimitAndExponentialBlock(t *testing.T) {
	a := New(tok)
	now := time.Unix(1000, 0)
	a.Limiter.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		if res, _ := a.Authenticate(req("2.2.2.2", bearer("bad"))); res != Invalid {
			t.Fatalf("attempt %d: %v", i, res)
		}
	}
	// Even the correct token is refused while blocked.
	res, d := a.Authenticate(req("2.2.2.2", bearer(tok)))
	if res != Blocked || d <= 0 {
		t.Fatalf("expected Blocked, got %v %v", res, d)
	}
	// Other IPs are unaffected.
	if res, _ := a.Authenticate(req("3.3.3.3", bearer(tok))); res != OK {
		t.Fatalf("other IP affected: %v", res)
	}

	now = now.Add(61 * time.Second) // first block (1m) expired
	if res, _ := a.Authenticate(req("2.2.2.2", bearer("bad"))); res != Invalid {
		t.Fatalf("after block: %v", res)
	}
	for i := 0; i < 4; i++ {
		a.Authenticate(req("2.2.2.2", bearer("bad")))
	}
	_, d2 := a.Authenticate(req("2.2.2.2", bearer(tok)))
	if d2 < 119*time.Second {
		t.Fatalf("second block should be ~2m, got %v", d2)
	}
}

func TestFailuresOutsideWindowForgotten(t *testing.T) {
	a := New(tok)
	now := time.Unix(1000, 0)
	a.Limiter.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		a.Authenticate(req("4.4.4.4", bearer("bad")))
	}
	now = now.Add(6 * time.Minute)
	a.Authenticate(req("4.4.4.4", bearer("bad")))
	if _, blocked := a.Limiter.Blocked("4.4.4.4"); blocked {
		t.Fatal("old failures counted toward the limit")
	}
}

func TestXForwardedForIgnored(t *testing.T) {
	r := req("5.5.5.5", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "9.9.9.9") })
	if ClientIP(r) != "5.5.5.5" {
		t.Fatal("X-Forwarded-For was trusted")
	}
}

// A stale cookie (e.g. after `token rotate`) must not lock the owner out.
func TestStaleCookieDoesNotCountTowardLimit(t *testing.T) {
	a := New(tok)
	for i := 0; i < 20; i++ {
		res, _ := a.Authenticate(req("6.6.6.6", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: CookieName, Value: "stale"})
		}))
		if res != Invalid {
			t.Fatalf("attempt %d: %v", i, res)
		}
	}
	if res, _ := a.Authenticate(req("6.6.6.6", bearer(tok))); res != OK {
		t.Fatalf("owner locked out by stale cookie: %v", res)
	}
}

func TestRevokeSessions(t *testing.T) {
	a := New(tok)
	id1, _ := a.NewSession()
	id2, _ := a.NewSession()
	a.RevokeSession(id1)
	if a.validSession(id1) || !a.validSession(id2) {
		t.Fatal("single revoke wrong")
	}
	a.RevokeAll()
	if a.validSession(id2) {
		t.Fatal("revoke all left a session")
	}
}
