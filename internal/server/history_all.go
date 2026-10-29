package server

import (
	"context"
	"net/http"
	"os"
	"time"

	"claude-remote/internal/agents"
	"claude-remote/internal/auth"
	"claude-remote/internal/history"
)

type recentView struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Folder   string    `json:"folder"` // last path element only
	Modified time.Time `json:"modified"`
}

// handleHistoryAll lists past conversations from EVERY folder on the laptop
// (not only the open ones), newest first.
func (s *Server) handleHistoryAll(w http.ResponseWriter, r *http.Request) {
	tool, ok := normTool(r.URL.Query().Get("tool"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	items := s.convosAll(tool, 80)
	out := make([]recentView, 0, len(items))
	for _, it := range items {
		out = append(out, recentView{ID: it.ID, Title: it.Title, Folder: agents.Label(it.CWD), Modified: it.Modified})
	}
	writeJSON2(w, http.StatusOK, out)
}

// handleHistoryOpen resumes a closed conversation in the folder Claude
// recorded for it. The client sends only the conversation id; the folder is
// read from Claude's own file on the server, never from the client.
//
// A conversation that is currently open in a laptop window is refused: it must
// be taken through the "laptop sessions" flow, which closes the window first,
// otherwise two processes would write to the same conversation.
func (s *Server) handleHistoryOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Tool string `json:"tool"`
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
	if !history.ValidID(req.ID) {
		writeErr(w, http.StatusBadRequest, "bad_resume_id")
		return
	}
	s.takeMu.Lock()
	defer s.takeMu.Unlock()

	rec, ok := s.convoFind(tool, req.ID)
	if !ok {
		writeErr(w, http.StatusNotFound, "no_such_conversation")
		return
	}
	if !agents.ValidCWD(rec.CWD) {
		writeErr(w, http.StatusUnprocessableEntity, "cannot_continue_here")
		return
	}
	if fi, err := os.Stat(rec.CWD); err != nil || !fi.IsDir() {
		writeErr(w, http.StatusUnprocessableEntity, "cannot_continue_here")
		return
	}
	// `claude agents` knows which Claude conversations are open in a window;
	// Codex offers no such list, so for Codex the UI warns instead.
	if s.o.Agents != nil && tool == toolClaude {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if live, err := s.o.Agents.List(ctx); err != nil {
			s.o.Log.Warn("history open: could not check live sessions", "err", err)
		} else {
			for _, a := range live {
				if a.SessionID == rec.ID {
					writeErr(w, http.StatusConflict, "open_on_laptop")
					return
				}
			}
		}
		if err := s.o.Agents.Preflight(agents.Agent{CWD: rec.CWD}); err != nil {
			s.o.Log.Warn("history open: cannot continue here", "claude_session", rec.ID, "err", err)
			writeErr(w, http.StatusUnprocessableEntity, "cannot_continue_here")
			return
		}
	}
	if e, ok := s.o.Sessions.FindLaptop(rec.ID, ""); ok {
		writeJSON2(w, http.StatusOK, view(e)) // already open here
		return
	}
	if !s.o.Sessions.HasRoom() {
		writeErr(w, http.StatusConflict, "too_many_sessions")
		return
	}
	e, err := s.o.Sessions.CreateTool(tool, agents.Label(rec.CWD), rec.CWD, rec.Title, rec.ID)
	if err != nil {
		s.createFailed(w, err)
		return
	}
	s.o.Log.Info("past conversation opened", "ip", auth.ClientIP(r), "session", e.ID, "claude_session", rec.ID)
	writeJSON2(w, http.StatusCreated, view(e))
}
