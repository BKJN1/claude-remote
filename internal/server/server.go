// Package server wires HTTP routes, authentication and the terminal WebSocket.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"claude-remote/internal/auth"
	"claude-remote/internal/session"
)

// ScreenshotFunc captures a display as PNG. scale<=0 means "pick a default".
type ScreenshotFunc func(ctx context.Context, display int, scale float64) ([]byte, error)

type Options struct {
	Auth    *auth.Auth
	Session *session.Session
	Web     fs.FS // embedded static files (root contains index.html)
	Log     *slog.Logger
	// Screenshot may be nil, in which case /screenshot returns 501.
	Screenshot ScreenshotFunc
	// AllowedHosts are extra Host names (e.g. the MagicDNS name) accepted in
	// addition to IP literals; this blocks DNS-rebinding attacks.
	AllowedHosts []string
	// Secure marks cookies Secure; set when serving TLS.
	Secure       bool
	MaxFrame     int64         // max incoming WebSocket frame, default 256 KiB
	PingInterval time.Duration // default 30s
}

type Server struct {
	o       Options
	allowed map[string]bool
	shotSem chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // active WebSocket handlers
	http   *http.Server
}

func New(o Options) *Server {
	if o.MaxFrame == 0 {
		o.MaxFrame = 256 << 10
	}
	if o.PingInterval == 0 {
		o.PingInterval = 30 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &Server{o: o, allowed: map[string]bool{}, shotSem: make(chan struct{}, 1)}
	for _, h := range o.AllowedHosts {
		s.allowed[strings.ToLower(h)] = true
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

// publicAssets are served without authentication so the login page can render
// and the browser can fetch the manifest (which is requested without cookies).
// None of them is sensitive.
var publicAssets = map[string]bool{
	"/style.css":            true,
	"/manifest.webmanifest": true,
	"/icon-192.png":         true,
	"/icon-512.png":         true,
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/ws", s.requireAuth(s.handleWS))
	mux.HandleFunc("/screenshot", s.requireAuth(s.handleScreenshot))
	static := http.FileServerFS(s.o.Web)
	mux.Handle("/", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		static.ServeHTTP(w, r)
	}))
	return s.secure(mux)
}

// Serve blocks until the listener fails or Shutdown is called.
func (s *Server) Serve(ln net.Listener, certFile, keyFile string) error {
	s.http = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	var err error
	if certFile != "" {
		err = s.http.ServeTLS(ln, certFile, keyFile)
	} else {
		err = s.http.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown closes WebSockets first (hijacked connections are invisible to
// http.Server.Shutdown), then stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

// secure applies the Host allow-list and the security headers to everything.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "bad host", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		// No inline scripts. Inline *styles* are allowed because xterm.js
		// injects style rules at runtime. connect-src names the host
		// explicitly since some browsers do not let 'self' cover ws:.
		h.Set("Content-Security-Policy", "default-src 'self'; "+
			"connect-src 'self' ws://"+r.Host+" wss://"+r.Host+"; "+
			"style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
			"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// hostAllowed accepts IP-literal Hosts and explicitly allowed names. A
// DNS-rebinding page would have to use a domain name, so it is refused.
func (s *Server) hostAllowed(hostport string) bool {
	host := hostport
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		// The port ends up in the CSP header, so it must be a plain number.
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return false
		}
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Zone() == "" // a zone would flow into the CSP header
	}
	return s.allowed[host]
}

// originOK requires an Origin header whose host equals the request Host.
// A missing Origin is rejected (browsers always send it on WebSocket
// handshakes and cross-origin or POST requests).
func originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return false
	}
	i := strings.Index(o, "://")
	if i < 0 {
		return false
	}
	return strings.EqualFold(o[i+3:], r.Host)
}

// formOriginOK is originOK for same-site form posts. With
// "Referrer-Policy: no-referrer" browsers send "Origin: null" on form
// submissions, so also accept a missing/null Origin when the browser itself
// says the request is same-origin (Sec-Fetch-Site cannot be set by a page).
// WebSocket handshakes keep the strict originOK: browsers always send a real
// Origin there.
func formOriginOK(r *http.Request) bool {
	if originOK(r) {
		return true
	}
	o := r.Header.Get("Origin")
	return (o == "" || o == "null") && r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// requireAuth gates a handler on a valid token (cookie or Bearer header).
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if publicAssets[r.URL.Path] {
			next(w, r)
			return
		}
		res, retry := s.o.Auth.Authenticate(r)
		switch res {
		case auth.OK:
			next(w, r)
		case auth.Blocked:
			s.o.Log.Warn("auth blocked", "ip", auth.ClientIP(r), "path", r.URL.Path, "retry_after", retry.Round(time.Second))
			w.Header().Set("Retry-After", itoa(int(retry.Seconds())+1))
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
		default:
			s.o.Log.Warn("auth denied", "ip", auth.ClientIP(r), "path", r.URL.Path, "result", resultName(res))
			if r.URL.Path == "/" && r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}
}

func resultName(r auth.Result) string {
	switch r {
	case auth.NoCredentials:
		return "no_credentials"
	case auth.Invalid:
		return "invalid_token"
	}
	return "unknown"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
