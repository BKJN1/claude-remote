package server

import (
	"net/http"
	"time"

	"claude-remote/internal/codexhist"
	"claude-remote/internal/history"
)

// The coding tools a session can run. The client picks one by id; the program
// and its arguments are decided on the server.
const (
	toolClaude = "claude"
	toolCodex  = "codex"
)

var toolNames = map[string]string{toolClaude: "Claude Code", toolCodex: "Codex"}

// normTool returns a known tool id ("claude" when empty) or ok=false.
func normTool(t string) (string, bool) {
	if t == "" {
		return toolClaude, true
	}
	_, ok := toolNames[t]
	return t, ok
}

func (s *Server) toolAvailable(tool string) bool {
	if s.o.ToolAvailable != nil {
		return s.o.ToolAvailable(tool)
	}
	return tool == toolClaude
}

// convo is a past conversation of either tool.
type convo struct {
	ID       string
	Title    string
	Modified time.Time
	CWD      string
}

func (s *Server) convosForDir(tool, dir string) []convo {
	var out []convo
	switch tool {
	case toolCodex:
		for _, r := range codexhist.ListForDir(s.o.CodexRoot, dir) {
			out = append(out, convo{r.ID, r.Title, r.Modified, r.CWD})
		}
	default:
		items, _ := history.List(s.o.HistoryRoot, dir)
		for _, it := range items {
			out = append(out, convo{ID: it.ID, Title: it.Title, Modified: it.Modified, CWD: dir})
		}
	}
	return out
}

func (s *Server) convosAll(tool string, limit int) []convo {
	var out []convo
	switch tool {
	case toolCodex:
		for _, r := range codexhist.ListAll(s.o.CodexRoot, limit) {
			out = append(out, convo{r.ID, r.Title, r.Modified, r.CWD})
		}
	default:
		for _, r := range history.ListAll(s.o.HistoryRoot, limit) {
			out = append(out, convo{r.ID, r.Title, r.Modified, r.CWD})
		}
	}
	return out
}

func (s *Server) convoFind(tool, id string) (convo, bool) {
	switch tool {
	case toolCodex:
		if r, ok := codexhist.Find(s.o.CodexRoot, id); ok {
			return convo{r.ID, r.Title, r.Modified, r.CWD}, true
		}
	default:
		if r, ok := history.Find(s.o.HistoryRoot, id); ok {
			return convo{r.ID, r.Title, r.Modified, r.CWD}, true
		}
	}
	return convo{}, false
}

// handleTools lists the tools the UI can offer at the start of a session.
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Available bool   `json:"available"`
	}
	out := []item{}
	for _, id := range []string{toolClaude, toolCodex} {
		out = append(out, item{id, toolNames[id], s.toolAvailable(id)})
	}
	writeJSON2(w, http.StatusOK, out)
}
