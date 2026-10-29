package server

import (
	"context"
	"net/http"
	"time"

	"claude-remote/internal/agents"
	"claude-remote/internal/auth"
	"claude-remote/internal/history"
)

// Agents is what the server needs from the laptop-session provider.
type Agents interface {
	List(ctx context.Context) ([]agents.Agent, error)
	// Preflight verifies the session can be continued here BEFORE it is closed.
	Preflight(a agents.Agent) error
	Kill(a agents.Agent) error
}

type laptopView struct {
	ID      string `json:"id"`     // Claude session id
	Title   string `json:"title"`  // the conversation's own title (what `claude --resume` shows)
	Name    string `json:"name"`   // Claude's short handle for the session, e.g. "versola-28"
	Folder  string `json:"folder"` // last path element only; full paths stay on the server
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Started int64  `json:"started"`
}

// handleLaptopList shows the Claude sessions open anywhere on the laptop.
func (s *Server) handleLaptopList(w http.ResponseWriter, r *http.Request) {
	if s.o.Agents == nil {
		writeJSON2(w, http.StatusOK, []struct{}{})
		return
	}
	s.laptopMu.Lock()
	list, err := s.laptopCached, error(nil)
	if time.Since(s.laptopAt) > 3*time.Second {
		list, err = s.o.Agents.List(r.Context())
		if err == nil {
			s.laptopCached, s.laptopAt = list, time.Now()
		}
	}
	s.laptopMu.Unlock()
	if err != nil {
		s.o.Log.Warn("laptop sessions list failed", "err", err)
		writeErr(w, http.StatusBadGateway, "agents_unavailable")
		return
	}
	out := make([]laptopView, 0, len(list))
	for _, a := range list {
		out = append(out, laptopView{ID: a.SessionID, Title: s.laptopTitle(a), Name: a.Name, Folder: agents.Label(a.CWD), Kind: a.Kind, Status: a.Status, Started: a.StartedAt})
	}
	writeJSON2(w, http.StatusOK, out)
}

// handleLaptopTake opens a laptop session here. The client sends only the
// Claude session id; the folder, process and name are looked up again on the
// server, so a client can never name a path or a PID.
//
//   - background session: attach to it (claude attach), nothing is closed;
//   - interactive session (a terminal window): the window's process is closed
//     and the conversation is resumed here, so two copies never write to the
//     same conversation. This needs confirm=true, and confirm_busy=true when
//     the session is working right now.
func (s *Server) handleLaptopTake(w http.ResponseWriter, r *http.Request) {
	if s.o.Agents == nil {
		writeErr(w, http.StatusNotImplemented, "agents_disabled")
		return
	}
	var req struct {
		ID          string `json:"id"`
		Confirm     bool   `json:"confirm"`
		ConfirmBusy bool   `json:"confirm_busy"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !agents.ValidID(req.ID) {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	// One take at a time: two parallel takes (a double tap, two devices) could
	// both close the window and both resume, forking the conversation.
	s.takeMu.Lock()
	defer s.takeMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	list, err := s.o.Agents.List(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "agents_unavailable")
		return
	}
	var a *agents.Agent
	for i := range list {
		if list[i].SessionID == req.ID {
			a = &list[i]
		}
	}
	if a == nil {
		writeErr(w, http.StatusNotFound, "no_such_laptop_session")
		return
	}
	ip := auth.ClientIP(r)
	label := agents.Label(a.CWD)
	title := s.laptopTitle(*a)
	if title == "" {
		title = "Сессия с ноутбука"
	}

	if a.Kind == "background" {
		if !agents.SafeName(a.Name) {
			writeErr(w, http.StatusUnprocessableEntity, "unsafe_session_name")
			return
		}
		if e, ok := s.o.Sessions.FindLaptop("", a.Name); ok {
			writeJSON2(w, http.StatusOK, view(e)) // already open here
			return
		}
		e, err := s.o.Sessions.CreateAttach(label, a.CWD, title, a.Name)
		if err != nil {
			s.createFailed(w, err)
			return
		}
		s.o.Log.Info("laptop session attached", "ip", ip, "session", e.ID, "claude_session", a.SessionID)
		writeJSON2(w, http.StatusCreated, view(e))
		return
	}

	if e, ok := s.o.Sessions.FindLaptop(a.SessionID, ""); ok {
		writeJSON2(w, http.StatusOK, view(e)) // already continued here
		return
	}
	if !req.Confirm {
		writeErr(w, http.StatusConflict, "confirm_required")
		return
	}
	if a.Status == "busy" && !req.ConfirmBusy {
		writeErr(w, http.StatusConflict, "confirm_busy_required")
		return
	}
	// Everything that could stop us from continuing is checked BEFORE the
	// window is closed: closing it and then failing would lose it for nothing.
	if !s.o.Sessions.HasRoom() {
		writeErr(w, http.StatusConflict, "too_many_sessions")
		return
	}
	if err := s.o.Agents.Preflight(*a); err != nil {
		s.o.Log.Warn("laptop session cannot be continued here", "claude_session", a.SessionID, "err", err)
		writeErr(w, http.StatusUnprocessableEntity, "cannot_continue_here")
		return
	}
	if err := s.o.Agents.Kill(*a); err != nil {
		s.o.Log.Error("closing laptop session failed", "ip", ip, "pid", a.PID, "claude_session", a.SessionID, "err", err)
		writeErr(w, http.StatusInternalServerError, "close_failed")
		return
	}
	s.o.Log.Info("laptop session closed", "ip", ip, "pid", a.PID, "claude_session", a.SessionID, "was", a.Status)
	e, err := s.o.Sessions.Create(label, a.CWD, title, a.SessionID)
	if err != nil {
		// The window is already closed; say so and where the conversation is.
		s.o.Log.Error("resume after close failed", "claude_session", a.SessionID, "err", err)
		writeErr(w, http.StatusInternalServerError, "closed_not_resumed")
		return
	}
	s.o.Log.Info("laptop session resumed here", "ip", ip, "session", e.ID, "claude_session", a.SessionID)
	writeJSON2(w, http.StatusCreated, view(e))
}

func (s *Server) createFailed(w http.ResponseWriter, err error) {
	s.o.Log.Error("create session failed", "err", err)
	writeErr(w, http.StatusConflict, "too_many_sessions")
}

// laptopTitle returns the conversation's own title when its file can be found
// (that is what the user recognises), else Claude's short handle.
func (s *Server) laptopTitle(a agents.Agent) string {
	if r, ok := history.Find(s.o.HistoryRoot, a.SessionID); ok && r.Title != "" {
		return r.Title
	}
	return a.Name
}
