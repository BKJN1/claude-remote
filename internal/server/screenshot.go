package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"claude-remote/internal/auth"
)

// A var so tests can shorten it.
var screenshotTimeout = 10 * time.Second

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	ip := auth.ClientIP(r)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.o.Screenshot == nil {
		http.Error(w, "screenshots not available", http.StatusNotImplemented)
		return
	}
	display, scale, ok := parseShotParams(r)
	if !ok {
		http.Error(w, "bad parameters", http.StatusBadRequest)
		return
	}
	// One capture at a time: if a capture hangs, later requests fail fast
	// instead of piling up goroutines.
	select {
	case s.shotSem <- struct{}{}:
	default:
		s.o.Log.Warn("screenshot busy", "ip", ip)
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	type result struct {
		png []byte
		err error
	}
	ch := make(chan result, 1)
	ctx, cancel := context.WithTimeout(r.Context(), screenshotTimeout)
	defer cancel()
	go func() {
		defer func() { <-s.shotSem }() // released only when the capture really returns
		png, err := s.o.Screenshot(ctx, display, scale)
		ch <- result{png, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			s.o.Log.Error("screenshot failed", "ip", ip, "err", res.err)
			http.Error(w, "capture failed", http.StatusInternalServerError)
			return
		}
		s.o.Log.Info("screenshot", "ip", ip, "display", display, "bytes", len(res.png))
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(res.png)
	case <-ctx.Done():
		s.o.Log.Error("screenshot timed out", "ip", ip)
		http.Error(w, "capture timed out", http.StatusGatewayTimeout)
	}
}

// parseShotParams: display is a 0-based index; scale is (0,1], 0 = default.
func parseShotParams(r *http.Request) (display int, scale float64, ok bool) {
	q := r.URL.Query()
	if v := q.Get("display"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 16 {
			return 0, 0, false
		}
		display = n
	}
	if v := q.Get("scale"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f > 0 && f <= 1) {
			return 0, 0, false
		}
		scale = f
	}
	return display, scale, true
}
