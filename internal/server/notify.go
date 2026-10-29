package server

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"claude-remote/internal/push"
)

// InternalHandler serves the single endpoint the local hook helper calls
// (`claude-remote notify`, started by Claude Code's Notification and Stop
// hooks). It is bound to loopback on its own listener, never to the Tailscale
// address, and still demands the per-run secret.
func (s *Server) InternalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /notify", s.handleNotify)
	return mux
}

// ServeInternal runs the internal listener until Shutdown.
func (s *Server) ServeInternal(ln net.Listener) error {
	s.internal = &http.Server{
		Handler:           s.InternalHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
	}
	err := s.internal.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.o.NotifySecret == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.o.NotifySecret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Session string `json:"session"`
		Event   string `json:"event"`
		Message string `json:"message"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	entry, ok := s.o.Sessions.Get(req.Session)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	toolName := "claude"
	if entry.Tool == toolCodex {
		toolName = "codex"
	}
	// The pulse follows Claude's own hooks, whether or not push is enabled.
	// Codex only reports "turn finished": that sends a push but must not pin
	// the pulse (it would stay "idle" while Codex works again).
	if entry.Tool != toolCodex {
		switch req.Event {
		case "working":
			entry.S.SetActivity("working")
			w.WriteHeader(http.StatusNoContent)
			return
		case "stop":
			entry.S.SetActivity("idle")
		default:
			entry.S.SetActivity("waiting")
		}
	} else if req.Event != "stop" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.o.Push == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	n := push.Notification{Title: toolName + " · " + entry.Project, Tag: entry.ID + ":" + req.Event, Session: entry.ID}
	switch req.Event {
	case "stop":
		n.Body = toolDisplay(entry.Tool) + " закончил: " + entry.Title
	default:
		req.Event = "attention"
		n.Tag = entry.ID + ":attention"
		n.Body = shorten(req.Message, 140)
		if n.Body == "" {
			n.Body = "Claude ждёт вашего ответа"
		}
	}
	if s.notifyThrottled(n.Tag) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.o.Log.Info("notify", "session", entry.ID, "event", req.Event)
	s.o.Push.Notify(n)
	w.WriteHeader(http.StatusNoContent)
}

// notifyThrottled drops a repeat of the same notification within 3 seconds.
func (s *Server) notifyThrottled(tag string) bool {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	now := time.Now()
	if t, ok := s.notifyLast[tag]; ok && now.Sub(t) < 3*time.Second {
		return true
	}
	s.notifyLast[tag] = now
	if len(s.notifyLast) > 256 {
		for k, t := range s.notifyLast {
			if now.Sub(t) > time.Minute {
				delete(s.notifyLast, k)
			}
		}
	}
	return false
}

func shorten(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

func toolDisplay(tool string) string {
	if tool == toolCodex {
		return "Codex"
	}
	return "Claude"
}
