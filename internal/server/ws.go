package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"claude-remote/internal/auth"
	"claude-remote/internal/session"
)

// Protocol: binary frames carry raw terminal bytes in both directions. Text
// frames carry JSON control messages.
//
//	client -> server: {"type":"resize","cols":N,"rows":N}, {"type":"restart"}
//	server -> client: {"type":"status","state":"running|exited","code":N},
//	                  {"type":"reset"}, {"type":"exit","code":N}, {"type":"error"}
type ctrlMsg struct {
	Type  string `json:"type"`
	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
	State string `json:"state,omitempty"`
	Code  int    `json:"code"`
}

const writeTimeout = 10 * time.Second

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ip := auth.ClientIP(r)
	// Cross-site WebSocket hijacking guard. Cookies are SameSite=Strict, but
	// the Origin check is the primary defence and must not be skipped.
	if !originOK(r) {
		s.o.Log.Warn("ws rejected", "ip", ip, "reason", "bad_origin")
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.o.Log.Warn("ws accept failed", "ip", ip, "err", err)
		return
	}
	c.SetReadLimit(s.o.MaxFrame) // oversized frame => library closes with 1009
	s.wg.Add(1)
	defer s.wg.Done()
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() { // server shutdown
		select {
		case <-s.ctx.Done():
			c.Close(websocket.StatusGoingAway, "server shutting down")
			cancel()
		case <-ctx.Done():
		}
	}()

	sess := s.o.Session
	if err := sess.Ensure(); err != nil {
		// Details go to the log only; the client gets a generic message.
		s.o.Log.Error("session start failed", "err", err)
		writeJSON(ctx, c, ctrlMsg{Type: "error"})
	}
	sub, snap, err := sess.Subscribe()
	if err != nil {
		c.Close(websocket.StatusGoingAway, "session closed")
		return
	}
	defer sub.Close()
	s.o.Log.Info("ws connected", "ip", ip)
	defer s.o.Log.Info("ws disconnected", "ip", ip)

	state := "running"
	if snap.State == session.Exited {
		state = "exited"
	}
	if writeJSON(ctx, c, ctrlMsg{Type: "status", State: state, Code: snap.ExitCode}) != nil ||
		writeBinary(ctx, c, snap.Replay) != nil {
		return
	}

	go s.pump(ctx, cancel, c, sub)
	go s.ping(ctx, cancel, c)

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if sess.Write(data) != nil {
				return
			}
		case websocket.MessageText:
			var m ctrlMsg
			if json.Unmarshal(data, &m) != nil {
				c.Close(websocket.StatusPolicyViolation, "bad control message")
				return
			}
			switch m.Type {
			case "resize":
				if err := sess.Resize(m.Cols, m.Rows); err != nil {
					s.o.Log.Debug("resize rejected", "err", err)
				}
			case "restart":
				s.o.Log.Info("session restart requested", "ip", ip)
				if err := sess.Restart(); err != nil {
					s.o.Log.Error("restart failed", "err", err)
				}
			default:
				c.Close(websocket.StatusPolicyViolation, "unknown control message")
				return
			}
		}
	}
}

// pump forwards session events to the client until either side goes away.
func (s *Server) pump(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, sub *session.Subscriber) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			// Dropped for being too slow (or the session closed).
			c.Close(websocket.StatusPolicyViolation, "too slow or session closed")
			return
		case ev := <-sub.C:
			var err error
			switch {
			case ev.Reset:
				err = writeJSON(ctx, c, ctrlMsg{Type: "reset"})
			case ev.Exited:
				err = writeJSON(ctx, c, ctrlMsg{Type: "exit", Code: ev.ExitCode})
			default:
				err = writeBinary(ctx, c, ev.Data)
			}
			if err != nil {
				return
			}
		}
	}
}

// ping detects dead peers: a missing pong within the timeout ends the handler.
func (s *Server) ping(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn) {
	t := time.NewTicker(s.o.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, pcancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

func writeBinary(ctx context.Context, c *websocket.Conn, b []byte) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.Write(ctx, websocket.MessageBinary, b)
}

func writeJSON(ctx context.Context, c *websocket.Conn, m ctrlMsg) error {
	b, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, b)
}
