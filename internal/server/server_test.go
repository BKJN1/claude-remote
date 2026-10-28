package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"claude-remote/internal/auth"
	"claude-remote/internal/session"
	"claude-remote/internal/session/fakepty"
)

const testToken = "test-token-test-token-test-token-test-token-xyz"

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type env struct {
	ts   *httptest.Server
	srv  *Server
	pty  *fakepty.Factory
	logs *lockedBuf
}

func (e *env) host() string { return strings.TrimPrefix(e.ts.URL, "http://") }

func newEnv(t *testing.T, mod func(*Options)) *env {
	t.Helper()
	f := &fakepty.Factory{}
	sess := session.New(f.StartFunc(), 1<<16, 80, 24)
	logs := &lockedBuf{}
	o := Options{
		Auth:    auth.New(testToken),
		Session: sess,
		Web: fstest.MapFS{
			"index.html": {Data: []byte("<html>app</html>")},
			"login.html": {Data: []byte("<html><!--ERROR--></html>")},
			"style.css":  {Data: []byte("body{}")},
			"app.js":     {Data: []byte("//")},
		},
		Log:          slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		MaxFrame:     1024,
		PingInterval: time.Hour,
		Screenshot: func(ctx context.Context, display int, scale float64) ([]byte, error) {
			return []byte("\x89PNG-fake"), nil
		},
	}
	if mod != nil {
		mod(&o)
	}
	srv := New(o)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		srv.cancel()
		sess.Close()
	})
	return &env{ts: ts, srv: srv, pty: f, logs: logs}
}

func (e *env) dial(t *testing.T, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: hdr})
}

func (e *env) goodHeaders() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testToken)
	h.Set("Origin", "http://"+e.host())
	return h
}

func TestWSWithoutAuthRejected(t *testing.T) {
	e := newEnv(t, nil)
	h := http.Header{}
	h.Set("Origin", "http://"+e.host())
	_, resp, err := e.dial(t, h)
	if err == nil {
		t.Fatal("unauthenticated websocket accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response %v", resp)
	}
	if e.pty.Count() != 0 {
		t.Fatal("unauthenticated client started the process")
	}
}

func TestWSForeignOriginRejected(t *testing.T) {
	e := newEnv(t, nil)
	for _, origin := range []string{"http://evil.example", "http://" + e.host() + ".evil.example", "null", ""} {
		h := e.goodHeaders()
		if origin == "" {
			h.Del("Origin")
		} else {
			h.Set("Origin", origin)
		}
		_, resp, err := e.dial(t, h)
		if err == nil {
			t.Fatalf("origin %q accepted", origin)
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: response %v", origin, resp)
		}
	}
}

func readText(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("expected text frame, got binary %q", data)
	}
	return string(data)
}

func readBinary(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("expected binary frame, got text %q", data)
	}
	return string(data)
}

func TestWSRoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	c, _, err := e.dial(t, e.goodHeaders())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if got := readText(t, c); !strings.Contains(got, `"status"`) || !strings.Contains(got, "running") {
		t.Fatalf("status %q", got)
	}
	readBinary(t, c) // replay

	ctx := context.Background()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}
	e.pty.Last().Emit([]byte("output"))
	if got := readBinary(t, c); got != "output" {
		t.Fatalf("got %q", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		cols, rows := e.pty.Last().Size()
		if string(e.pty.Last().Written()) == "ls\r" && cols == 120 && rows == 40 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("input/resize did not arrive: %q %dx%d", e.pty.Last().Written(), cols, rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWSRestartCommand(t *testing.T) {
	e := newEnv(t, nil)
	c, _, err := e.dial(t, e.goodHeaders())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readText(t, c)
	readBinary(t, c)
	if err := c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"restart"}`)); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, c); !strings.Contains(got, "reset") {
		t.Fatalf("got %q", got)
	}
	if e.pty.Count() != 2 {
		t.Fatalf("process count %d", e.pty.Count())
	}
}

func TestWSOversizedFrameClosesConnection(t *testing.T) {
	e := newEnv(t, nil) // MaxFrame = 1024
	c, _, err := e.dial(t, e.goodHeaders())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readText(t, c)
	readBinary(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.Write(ctx, websocket.MessageBinary, bytes.Repeat([]byte("A"), 4096))
	_, _, err = c.Read(ctx)
	if err == nil {
		t.Fatal("connection stayed open after oversized frame")
	}
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("close status %v (%v)", websocket.CloseStatus(err), err)
	}
	if len(e.pty.Last().Written()) != 0 {
		t.Fatal("oversized frame was forwarded to the terminal")
	}
}

func TestWSBadControlMessageClosed(t *testing.T) {
	e := newEnv(t, nil)
	c, _, _ := e.dial(t, e.goodHeaders())
	defer c.CloseNow()
	readText(t, c)
	readBinary(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.Write(ctx, websocket.MessageText, []byte(`{"type":"exec","cmd":"calc"}`))
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("err %v", err)
	}
}

func TestScreenshotRequiresAuth(t *testing.T) {
	e := newEnv(t, nil)
	resp, err := http.Get(e.ts.URL + "/screenshot")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// A token in the query string must not authenticate.
	resp, _ = http.Get(e.ts.URL + "/screenshot?token=" + testToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query token: status %d", resp.StatusCode)
	}
}

func authedGet(t *testing.T, e *env, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestScreenshotOK(t *testing.T) {
	e := newEnv(t, nil)
	resp := authedGet(t, e, "/screenshot?display=0&scale=0.5")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.HasPrefix(body, []byte("\x89PNG")) {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("headers %v", resp.Header)
	}
	if r := authedGet(t, e, "/screenshot?scale=5"); r.StatusCode != 400 {
		t.Fatalf("bad scale accepted: %d", r.StatusCode)
	}
}

func TestScreenshotHangDoesNotHangServer(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	e := newEnv(t, func(o *Options) {
		o.Screenshot = func(ctx context.Context, d int, s float64) ([]byte, error) {
			<-block // ignores ctx on purpose: a wedged GDI call
			return nil, errors.New("never")
		}
	})
	old := screenshotTimeoutForTest(200 * time.Millisecond)
	defer old()
	start := time.Now()
	resp := authedGet(t, e, "/screenshot")
	if resp.StatusCode != http.StatusGatewayTimeout || time.Since(start) > 3*time.Second {
		t.Fatalf("status %d after %v", resp.StatusCode, time.Since(start))
	}
	// Second request fails fast (busy) and the server still serves others.
	if r := authedGet(t, e, "/screenshot"); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected busy, got %d", r.StatusCode)
	}
	if r := authedGet(t, e, "/"); r.StatusCode != 200 {
		t.Fatalf("server wedged: %d", r.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, nil)
	for _, path := range []string{"/", "/login", "/screenshot", "/nope"} {
		resp, err := http.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		h := resp.Header
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "script-src") && strings.Contains(csp, "unsafe") ||
			!strings.Contains(csp, "ws://"+e.host()) {
			t.Errorf("%s: CSP %q", path, csp)
		}
		for k, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"Referrer-Policy":        "no-referrer",
			"X-Frame-Options":        "DENY",
		} {
			if h.Get(k) != want {
				t.Errorf("%s: %s = %q", path, k, h.Get(k))
			}
		}
	}
}

func TestHostAllowlistBlocksDNSRebinding(t *testing.T) {
	e := newEnv(t, nil)
	req, _ := http.NewRequest("GET", e.ts.URL+"/", nil)
	req.Host = "evil.example:8443"
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if e.srv.hostAllowed("[fe80::1%25eth0]:8443") || e.srv.hostAllowed("fe80::1%eth0") {
		t.Fatal("IPv6 zone accepted in Host")
	}
	if e.srv.hostAllowed("127.0.0.1:1;x") || e.srv.hostAllowed("127.0.0.1:99999") {
		t.Fatal("malformed port accepted in Host")
	}
	e2 := newEnv(t, func(o *Options) { o.AllowedHosts = []string{"laptop.tail1234.ts.net"} })
	req, _ = http.NewRequest("GET", e2.ts.URL+"/", nil)
	req.Host = "laptop.tail1234.ts.net:8443"
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("allowed host rejected: %d", resp.StatusCode)
	}
}

func TestLoginFlow(t *testing.T) {
	e := newEnv(t, nil)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(token, origin string) *http.Response {
		req, _ := http.NewRequest("POST", e.ts.URL+"/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := post(testToken, ""); r.StatusCode != 403 {
		t.Fatalf("missing Origin: %d", r.StatusCode)
	}
	// Browsers send "Origin: null" for same-origin form posts under
	// no-referrer; accepted only with Sec-Fetch-Site: same-origin.
	nullPost := func(site string) int {
		req, _ := http.NewRequest("POST", e.ts.URL+"/login", strings.NewReader(url.Values{"token": {testToken}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "null")
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := nullPost("cross-site"); c != 403 {
		t.Fatalf("null Origin + cross-site: %d", c)
	}
	if c := nullPost(""); c != 403 {
		t.Fatalf("null Origin without Sec-Fetch-Site: %d", c)
	}
	if c := nullPost("same-origin"); c != 303 {
		t.Fatalf("null Origin + same-origin: %d", c)
	}
	if r := post(testToken, "http://evil.example"); r.StatusCode != 403 {
		t.Fatalf("foreign Origin: %d", r.StatusCode)
	}
	if r := post("wrong", "http://"+e.host()); r.StatusCode != 401 || len(r.Cookies()) != 0 {
		t.Fatalf("wrong token: %d cookies=%v", r.StatusCode, r.Cookies())
	}
	r := post(testToken, "http://"+e.host())
	if r.StatusCode != 303 {
		t.Fatalf("good login: %d", r.StatusCode)
	}
	cs := r.Cookies()
	if len(cs) != 1 || !cs[0].HttpOnly || cs[0].SameSite != http.SameSiteStrictMode || cs[0].Name != auth.CookieName {
		t.Fatalf("cookie %+v", cs)
	}
	// Cookie alone authenticates.
	req, _ := http.NewRequest("GET", e.ts.URL+"/", nil)
	req.AddCookie(cs[0])
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("cookie auth: %d", resp.StatusCode)
	}
	// Unauthenticated / redirects to the login page.
	resp, _ = client.Get(e.ts.URL + "/")
	resp.Body.Close()
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauth /: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Static assets other than the public ones need auth.
	resp, _ = http.Get(e.ts.URL + "/app.js")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("app.js without auth: %d", resp.StatusCode)
	}
}

func TestLoginRateLimited(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", e.ts.URL+"/login", strings.NewReader("token=bad"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://"+e.host())
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
	}
	resp := authedGet(t, e, "/") // correct token, but the IP is locked out
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// The log must never contain the token, wrong tokens, cookies or terminal data.
func TestLogsDoNotLeakSecrets(t *testing.T) {
	e := newEnv(t, nil)
	const wrong = "WRONG-TOKEN-SHOULD-NOT-APPEAR"

	req, _ := http.NewRequest("POST", e.ts.URL+"/login", strings.NewReader(url.Values{"token": {wrong}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+e.host())
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	resp, _ = http.Get(e.ts.URL + "/screenshot?token=" + wrong)
	resp.Body.Close()
	req, _ = http.NewRequest("GET", e.ts.URL+"/screenshot", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	req, _ = http.NewRequest("POST", e.ts.URL+"/login", strings.NewReader(url.Values{"token": {testToken}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+e.host())
	resp, _ = (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	resp.Body.Close()
	authedGet(t, e, "/screenshot")

	c, _, err := e.dial(t, e.goodHeaders())
	if err != nil {
		t.Fatal(err)
	}
	readText(t, c)
	readBinary(t, c)
	c.Write(context.Background(), websocket.MessageBinary, []byte("TYPED-SECRET-INPUT"))
	e.pty.Last().Emit([]byte("TERMINAL-SECRET-OUTPUT"))
	readBinary(t, c)
	c.Close(websocket.StatusNormalClosure, "")
	time.Sleep(100 * time.Millisecond)

	logs := e.logs.String()
	if logs == "" {
		t.Fatal("expected some log output")
	}
	for _, secret := range []string{testToken, wrong, "TYPED-SECRET-INPUT", "TERMINAL-SECRET-OUTPUT", auth.CookieName + "="} {
		if strings.Contains(logs, secret) {
			t.Errorf("log leaks %q:\n%s", secret, logs)
		}
	}
	for _, want := range []string{"login failed", "login ok", "screenshot", "ws connected"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q", want)
		}
	}
}

// screenshotTimeoutForTest overrides the capture timeout; call the result to restore.
func screenshotTimeoutForTest(d time.Duration) func() {
	old := screenshotTimeout
	screenshotTimeout = d
	return func() { screenshotTimeout = old }
}
