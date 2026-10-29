package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"claude-remote/internal/lock"
)

const testPIN = "493021"

// lockEnv builds a server with a PIN set and returns a login-session cookie.
func lockEnv(t *testing.T, idle time.Duration, mod func(*Options)) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	if err := lock.Set(dir, testPIN); err != nil {
		t.Fatal(err)
	}
	lk := lock.New(dir, idle)
	e := newEnv(t, func(o *Options) {
		o.Lock = lk
		if mod != nil {
			mod(o)
		}
	})
	cookie, err := e.srv.o.Auth.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	return e, cookie
}

// asBrowser sends a cookie-authenticated request, like the page does.
func asBrowser(t *testing.T, e *env, cookie, method, path string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, bytes.NewReader(body))
	req.Header.Set("X-CR", "1")
	req.AddCookie(&http.Cookie{Name: "cr_session", Value: cookie})
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func unlock(t *testing.T, e *env, cookie, pin string) (int, string) {
	t.Helper()
	resp, body := asBrowser(t, e, cookie, "POST", "/api/lock/unlock", []byte(`{"pin":"`+pin+`"}`), nil)
	var out struct{ Key string }
	json.Unmarshal(body, &out)
	return resp.StatusCode, out.Key
}

func TestLockedUntilPINEntered(t *testing.T) {
	e, cookie := lockEnv(t, time.Minute, nil)
	for _, path := range []string{"/api/projects", "/api/sessions", "/api/laptop", "/api/displays", "/screenshot"} {
		if resp, _ := asBrowser(t, e, cookie, "GET", path, nil, nil); resp.StatusCode != http.StatusLocked {
			t.Errorf("%s while locked: %d", path, resp.StatusCode)
		}
	}
	if resp, _ := asBrowser(t, e, cookie, "POST", "/api/sessions", []byte(`{"project":"proj"}`), nil); resp.StatusCode != http.StatusLocked || e.pty.Count() != 0 {
		t.Fatalf("session created while locked: %d", resp.StatusCode)
	}
	code, key := unlock(t, e, cookie, testPIN)
	if code != 200 || key == "" {
		t.Fatalf("unlock: %d", code)
	}
	if resp, _ := asBrowser(t, e, cookie, "GET", "/api/projects", nil, map[string]string{"X-Unlock": key}); resp.StatusCode != 200 {
		t.Fatalf("with key: %d", resp.StatusCode)
	}
	if resp, _ := asBrowser(t, e, cookie, "GET", "/api/projects", nil, map[string]string{"X-Unlock": key + "x"}); resp.StatusCode != http.StatusLocked {
		t.Fatalf("forged key: %d", resp.StatusCode)
	}
}

func TestStaticPageAndLockEndpointsStayReachableWhileLocked(t *testing.T) {
	e, cookie := lockEnv(t, time.Minute, nil)
	if resp, _ := asBrowser(t, e, cookie, "GET", "/", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	resp, body := asBrowser(t, e, cookie, "GET", "/api/lock/status", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"enabled":true`) || !strings.Contains(string(body), `"unlocked":false`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestKeyBelongsToItsLoginSession(t *testing.T) {
	e, cookieA := lockEnv(t, time.Minute, nil)
	cookieB, _ := e.srv.o.Auth.NewSession()
	_, key := unlock(t, e, cookieA, testPIN)
	if resp, _ := asBrowser(t, e, cookieB, "GET", "/api/projects", nil, map[string]string{"X-Unlock": key}); resp.StatusCode != http.StatusLocked {
		t.Fatalf("a key worked for another login session: %d", resp.StatusCode)
	}
}

func TestBearerClientsAreNotSubjectToThePIN(t *testing.T) {
	e, _ := lockEnv(t, time.Minute, nil)
	resp, _ := call(t, e, "GET", "/api/projects", nil, false, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("bearer blocked by the PIN: %d", resp.StatusCode)
	}
}

func TestNoPINMeansNoGate(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Lock = lock.New(t.TempDir(), time.Minute) })
	cookie, _ := e.srv.o.Auth.NewSession()
	if resp, _ := asBrowser(t, e, cookie, "GET", "/api/projects", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("gated without a PIN: %d", resp.StatusCode)
	}
	if code, _ := unlock(t, e, cookie, testPIN); code != http.StatusBadRequest {
		t.Fatalf("unlock without a PIN set: %d", code)
	}
}

func TestLockNowRelocks(t *testing.T) {
	e, cookie := lockEnv(t, time.Minute, nil)
	_, key := unlock(t, e, cookie, testPIN)
	hdr := map[string]string{"X-Unlock": key}
	if resp, _ := asBrowser(t, e, cookie, "GET", "/api/projects", nil, hdr); resp.StatusCode != 200 {
		t.Fatal("not unlocked")
	}
	asBrowser(t, e, cookie, "POST", "/api/lock/lock", nil, nil)
	if resp, _ := asBrowser(t, e, cookie, "GET", "/api/projects", nil, hdr); resp.StatusCode != http.StatusLocked {
		t.Fatalf("still unlocked after lock-now: %d", resp.StatusCode)
	}
}

func TestWrongPINIsRateLimited(t *testing.T) {
	e, cookie := lockEnv(t, time.Minute, nil)
	for i := 0; i < 5; i++ {
		if code, _ := unlock(t, e, cookie, "000001"); code != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	// The sixth attempt is refused even with the right PIN.
	if code, _ := unlock(t, e, cookie, testPIN); code != http.StatusTooManyRequests {
		t.Fatalf("lockout not enforced: %d", code)
	}
}

func TestTooManyWrongPINsEndTheLogin(t *testing.T) {
	e, cookie := lockEnv(t, time.Minute, nil)
	// Bypass the per-IP limiter so the per-session limit is what trips.
	var last int
	for i := 0; i < lock.MaxFailures; i++ {
		e.srv.o.Auth.Limiter.Success("pin:127.0.0.1")
		last, _ = unlock(t, e, cookie, "000001")
	}
	if last != http.StatusUnauthorized {
		t.Fatalf("login not ended after %d failures: %d", lock.MaxFailures, last)
	}
	if resp, _ := asBrowser(t, e, cookie, "GET", "/api/lock/status", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie still valid: %d", resp.StatusCode)
	}
}

func dialWithKey(t *testing.T, e *env, cookie, key string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	h := http.Header{}
	h.Set("Origin", "http://"+e.host())
	h.Set("Cookie", "cr_session="+cookie)
	opts := &websocket.DialOptions{HTTPHeader: h}
	if key != "" {
		opts.Subprotocols = []string{"cr-unlock", key}
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+"/ws?session="+e.sid(t), opts)
}

func TestWebSocketNeedsTheUnlockKey(t *testing.T) {
	e, cookie := lockEnv(t, time.Minute, nil)
	if _, resp, err := dialWithKey(t, e, cookie, ""); err == nil || resp == nil || resp.StatusCode != http.StatusLocked {
		t.Fatalf("websocket opened while locked: err=%v resp=%v", err, resp)
	}
	_, key := unlock(t, e, cookie, testPIN)
	c, _, err := dialWithKey(t, e, cookie, key)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readText(t, c) // status
	readBinary(t, c)
	if c.Subprotocol() != "cr-unlock" {
		t.Fatalf("subprotocol %q", c.Subprotocol())
	}
	// The key must not be echoed or logged.
	if strings.Contains(e.logs.String(), key) {
		t.Fatal("unlock key found in the log")
	}
}

func TestWebSocketClosesWhenTheKeyExpires(t *testing.T) {
	e, cookie := lockEnv(t, 400*time.Millisecond, func(o *Options) { o.PingInterval = 100 * time.Millisecond })
	_, key := unlock(t, e, cookie, testPIN)
	c, _, err := dialWithKey(t, e, cookie, key)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var cerr error
	for cerr == nil {
		_, _, cerr = c.Read(ctx)
	}
	if websocket.CloseStatus(cerr) != websocket.StatusPolicyViolation {
		t.Fatalf("connection was not closed by the lock: %v", cerr)
	}
}

func TestPINAndKeyNeverReachTheLog(t *testing.T) {
	e, cookie := lockEnv(t, time.Minute, nil)
	unlock(t, e, cookie, "135790") // wrong
	_, key := unlock(t, e, cookie, testPIN)
	asBrowser(t, e, cookie, "GET", "/api/projects", nil, map[string]string{"X-Unlock": key})
	logs := e.logs.String()
	for _, secret := range []string{testPIN, "135790", key} {
		if strings.Contains(logs, secret) {
			t.Errorf("log leaks %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, "pin failed") || !strings.Contains(logs, "unlocked") {
		t.Errorf("expected PIN events in the log:\n%s", logs)
	}
}

func TestSecurityHeadersIncludeMicrophonePolicyAndBlobImages(t *testing.T) {
	e := newEnv(t, nil)
	resp, err := http.Get(e.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Permissions-Policy"), "microphone=(self)") {
		t.Fatalf("Permissions-Policy %q", resp.Header.Get("Permissions-Policy"))
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "img-src 'self' data: blob:") {
		t.Fatalf("CSP %q", resp.Header.Get("Content-Security-Policy"))
	}
}

// ---- pulse -------------------------------------------------------------

func pulseEnv(t *testing.T) (*env, *httptest.Server) {
	e := newEnv(t, func(o *Options) { o.NotifySecret = "pulse-secret-pulse-secret" })
	internal := httptest.NewServer(e.srv.InternalHandler())
	t.Cleanup(internal.Close)
	return e, internal
}

func hook(t *testing.T, internal *httptest.Server, session, event, message string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"session": session, "event": event, "message": message})
	req, _ := http.NewRequest("POST", internal.URL+"/notify", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer pulse-secret-pulse-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func pulseOfSession(t *testing.T, e *env, id string) string {
	_, body := call(t, e, "GET", "/api/sessions", nil, false, nil)
	var list []sessionView
	json.Unmarshal(body, &list)
	for _, v := range list {
		if v.ID == id {
			return v.Pulse
		}
	}
	t.Fatalf("session %s not listed", id)
	return ""
}

func TestPulseFollowsClaudeHooks(t *testing.T) {
	e, internal := pulseEnv(t)
	id := e.sid(t)
	if p := pulseOfSession(t, e, id); p != "idle" {
		t.Fatalf("fresh session: %q", p)
	}
	hook(t, internal, id, "working", "")
	if p := pulseOfSession(t, e, id); p != "working" {
		t.Fatalf("after UserPromptSubmit: %q", p)
	}
	hook(t, internal, id, "notification", "Claude needs your permission")
	if p := pulseOfSession(t, e, id); p != "waiting" {
		t.Fatalf("after Notification: %q", p)
	}
	hook(t, internal, id, "stop", "")
	if p := pulseOfSession(t, e, id); p != "idle" {
		t.Fatalf("after Stop: %q", p)
	}
	e.pty.Last().Exit(0)
	deadline := time.Now().Add(2 * time.Second)
	for pulseOfSession(t, e, id) != "exited" {
		if time.Now().After(deadline) {
			t.Fatal("exited session not reported")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPulseWithoutHooksFollowsOutput(t *testing.T) {
	e, _ := pulseEnv(t)
	id := e.sid(t)
	e.pty.Last().Emit([]byte("busy printing"))
	deadline := time.Now().Add(2 * time.Second)
	for pulseOfSession(t, e, id) != "working" {
		if time.Now().After(deadline) {
			t.Fatal("recent output not reported as working")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPulseIsPushedOverTheWebSocket(t *testing.T) {
	e, internal := pulseEnv(t)
	id := e.sid(t)
	c, _, err := e.dial(t, e.goodHeaders())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if got := readText(t, c); !strings.Contains(got, `"pulse":"idle"`) {
		t.Fatalf("status lacks the pulse: %s", got)
	}
	readBinary(t, c)
	hook(t, internal, id, "notification", "needs approval")
	if got := readText(t, c); !strings.Contains(got, `"type":"pulse"`) || !strings.Contains(got, `"state":"waiting"`) {
		t.Fatalf("got %s", got)
	}
	// Answering the prompt means Claude is working again.
	c.Write(context.Background(), websocket.MessageBinary, []byte("y\r"))
	if got := readText(t, c); !strings.Contains(got, `"state":"working"`) {
		t.Fatalf("got %s", got)
	}
}

// The page polls in the background; that must not keep the PIN unlocked forever.
func TestPassivePollingDoesNotExtendTheIdleTimer(t *testing.T) {
	e, cookie := lockEnv(t, 300*time.Millisecond, nil)
	_, key := unlock(t, e, cookie, testPIN)
	passive := map[string]string{"X-Unlock": key, "X-Passive": "1"}
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		asBrowser(t, e, cookie, "GET", "/api/sessions", nil, passive)
		time.Sleep(50 * time.Millisecond)
	}
	if resp, _ := asBrowser(t, e, cookie, "GET", "/api/projects", nil, map[string]string{"X-Unlock": key}); resp.StatusCode != http.StatusLocked {
		t.Fatalf("background polling kept the session unlocked: %d", resp.StatusCode)
	}
}

func TestRealActivityExtendsTheIdleTimer(t *testing.T) {
	e, cookie := lockEnv(t, 400*time.Millisecond, nil)
	_, key := unlock(t, e, cookie, testPIN)
	hdr := map[string]string{"X-Unlock": key}
	for i := 0; i < 5; i++ {
		time.Sleep(150 * time.Millisecond)
		if resp, _ := asBrowser(t, e, cookie, "GET", "/api/projects", nil, hdr); resp.StatusCode != 200 {
			t.Fatalf("expired during activity at step %d", i)
		}
	}
}

// A PIN set while a connection is already open must start applying to it.
func TestPINSetWhileConnectedClosesTheWebSocket(t *testing.T) {
	dir := t.TempDir()
	lk := lock.New(dir, time.Minute)
	e := newEnv(t, func(o *Options) { o.Lock = lk; o.PingInterval = 100 * time.Millisecond })
	cookie, _ := e.srv.o.Auth.NewSession()
	c, _, err := dialWithKey(t, e, cookie, "")
	if err != nil {
		t.Fatalf("no PIN yet, should connect: %v", err)
	}
	defer c.CloseNow()
	readText(t, c)
	readBinary(t, c)
	if err := lock.Set(dir, testPIN); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var cerr error
	for cerr == nil {
		_, _, cerr = c.Read(ctx)
	}
	if websocket.CloseStatus(cerr) != websocket.StatusPolicyViolation {
		t.Fatalf("connection stayed open after a PIN was set: %v", cerr)
	}
}
