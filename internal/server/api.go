package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"claude-remote/internal/auth"
	"claude-remote/internal/history"
	"claude-remote/internal/push"
	"claude-remote/internal/session"
)

// api wraps a JSON API handler: authentication first, then, for requests that
// change state, a CSRF guard.
//
// The guard is the custom X-CR header. A cross-site page cannot add a custom
// header without a CORS preflight, and this server never answers preflights,
// so only our own page can send it. A non-matching Origin is rejected too; a
// missing or "null" Origin is tolerated because Referrer-Policy: no-referrer
// makes some browsers send "null" even for same-origin requests.
func (s *Server) api(h http.HandlerFunc) http.HandlerFunc { return s.apiGuard(true, h) }

// apiOpen is api without the PIN gate (the lock endpoints themselves).
func (s *Server) apiOpen(h http.HandlerFunc) http.HandlerFunc { return s.apiGuard(false, h) }

func (s *Server) apiGuard(gated bool, h http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-CR") != "1" {
				writeErr(w, http.StatusForbidden, "csrf")
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "null" && !originOK(r) {
				writeErr(w, http.StatusForbidden, "bad_origin")
				return
			}
		}
		if gated && !s.unlocked(w, r) {
			return
		}
		h(w, r)
	})
}

func writeJSON2(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code string) {
	writeJSON2(w, status, map[string]string{"error": code})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return false
	}
	return true
}

func (s *Server) project(name string) (Project, bool) {
	for _, p := range s.o.Projects {
		if p.Name == name {
			return p, true
		}
	}
	return Project{}, false
}

// Paths are never sent to clients: only names.
// handleLogout ends this device's login, or every login with ?all=1. The
// cookie cannot be scoped to a port, so being able to cut access off matters.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") == "1" {
		s.o.Auth.RevokeAll()
	} else if c, err := r.Cookie(auth.CookieName); err == nil {
		s.o.Auth.RevokeSession(c.Value)
		if s.o.Lock != nil {
			s.o.Lock.Forget(c.Value)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.o.Secure, SameSite: http.SameSiteStrictMode})
	s.o.Log.Info("logout", "ip", auth.ClientIP(r), "all", r.URL.Query().Get("all") == "1")
	writeJSON2(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name string `json:"name"`
	}
	out := make([]item, 0, len(s.o.Projects))
	for _, p := range s.o.Projects {
		out = append(out, item{p.Name})
	}
	writeJSON2(w, http.StatusOK, out)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	tool, ok := normTool(r.URL.Query().Get("tool"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	p, ok := s.project(r.URL.Query().Get("project"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no_such_project")
		return
	}
	items := []history.Item{}
	for _, c := range s.convosForDir(tool, p.Path) {
		items = append(items, history.Item{ID: c.ID, Title: c.Title, Modified: c.Modified})
	}
	writeJSON2(w, http.StatusOK, items)
}

type sessionView struct {
	ID       string    `json:"id"`
	Project  string    `json:"project"`
	Title    string    `json:"title"`
	Resume   string    `json:"resume,omitempty"`
	Tool     string    `json:"tool"`
	State    string    `json:"state"`
	Pulse    string    `json:"pulse"`       // working, waiting, idle or exited
	PulseAt  int64     `json:"pulse_since"` // Unix ms when the pulse started (0 if unknown)
	ExitCode int       `json:"exit_code"`
	Err      string    `json:"err,omitempty"`
	Created  time.Time `json:"created"`
}

func view(e *session.Entry) sessionView {
	info := e.S.Info()
	state := "running"
	if info.State == session.Exited {
		state = "exited"
	}
	tool := e.Tool
	if tool == "" {
		tool = toolClaude
	}
	pulse, pulseAt := pulseOf(info, tool)
	return sessionView{
		Tool: tool, Pulse: pulse, PulseAt: pulseAt,
		ID: e.ID, Project: e.Project, Title: e.Title, Resume: e.Resume,
		State: state, ExitCode: info.ExitCode, Err: info.ErrKind, Created: e.Created,
	}
}

func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	list := s.o.Sessions.List()
	out := make([]sessionView, 0, len(list))
	for _, e := range list {
		out = append(out, view(e))
	}
	writeJSON2(w, http.StatusOK, out)
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project string `json:"project"`
		Resume  string `json:"resume"`
		Tool    string `json:"tool"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	tool, known := normTool(req.Tool)
	if !known {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !s.toolAvailable(tool) {
		writeErr(w, http.StatusUnprocessableEntity, "tool_unavailable")
		return
	}
	p, ok := s.project(req.Project)
	if !ok {
		writeErr(w, http.StatusNotFound, "no_such_project")
		return
	}
	title := "Новая сессия"
	if req.Resume != "" {
		// The id ends up on the command line, so it must be a plain UUID.
		if !history.ValidID(req.Resume) {
			writeErr(w, http.StatusBadRequest, "bad_resume_id")
			return
		}
		// The id must be one of THIS project's conversations: folder names
		// are lossy ("a b" and "a-b" share one), so do not trust the id alone.
		title = ""
		for _, c := range s.convosForDir(tool, p.Path) {
			if c.ID == req.Resume {
				title = c.Title
			}
		}
		if title == "" {
			writeErr(w, http.StatusBadRequest, "bad_resume_id")
			return
		}
	}
	e, err := s.o.Sessions.CreateTool(tool, p.Name, p.Path, title, req.Resume)
	if err == session.ErrTooManySessions {
		writeErr(w, http.StatusConflict, "too_many_sessions")
		return
	}
	if err != nil {
		s.o.Log.Error("create session failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.o.Log.Info("session created", "ip", auth.ClientIP(r), "session", e.ID, "project", p.Name, "resume", req.Resume != "")
	writeJSON2(w, http.StatusCreated, view(e))
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.o.Sessions.Close(id) {
		writeErr(w, http.StatusNotFound, "no_such_session")
		return
	}
	s.o.Log.Info("session closed", "ip", auth.ClientIP(r), "session", id)
	writeJSON2(w, http.StatusOK, map[string]string{"status": "closed"})
}

func (s *Server) handleDisplays(w http.ResponseWriter, r *http.Request) {
	if s.o.Displays == nil {
		writeJSON2(w, http.StatusOK, []struct{}{})
		return
	}
	d := s.o.Displays()
	if d == nil {
		writeJSON2(w, http.StatusOK, []struct{}{})
		return
	}
	writeJSON2(w, http.StatusOK, d)
}

func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	if s.o.Push == nil {
		writeErr(w, http.StatusNotImplemented, "push_disabled")
		return
	}
	writeJSON2(w, http.StatusOK, map[string]string{"key": s.o.Push.PublicKey()})
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.o.Push == nil {
		writeErr(w, http.StatusNotImplemented, "push_disabled")
		return
	}
	var sub struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			Auth   string `json:"auth"`
			P256dh string `json:"p256dh"`
		} `json:"keys"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	ps := push.Subscription{Endpoint: sub.Endpoint}
	ps.Keys.Auth, ps.Keys.P256dh = sub.Keys.Auth, sub.Keys.P256dh
	if err := s.o.Push.Subscribe(ps); err != nil {
		s.o.Log.Warn("push subscribe rejected", "err", err)
		writeErr(w, http.StatusBadRequest, "bad_subscription")
		return
	}
	s.o.Log.Info("push subscribed", "ip", auth.ClientIP(r))
	writeJSON2(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.o.Push == nil {
		writeErr(w, http.StatusNotImplemented, "push_disabled")
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	s.o.Push.Unsubscribe(strings.TrimSpace(req.Endpoint))
	writeJSON2(w, http.StatusOK, map[string]string{"status": "ok"})
}
