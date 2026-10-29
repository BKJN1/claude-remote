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
//	server -> client: {"type":"status","state":"running|exited","code":N,"err":KIND},
//	                  {"type":"reset"}, {"type":"exit","code":N,"err":KIND},
//	                  {"type":"pulse","state":"working|waiting|idle|exited","since":MS}
//
// The session is chosen with /ws?session=ID. err is one of the session.Kind*
// values when the process could not be started.
type ctrlMsg struct {
	Type  string `json:"type"`
	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
	State string `json:"state,omitempty"`
	Code  int    `json:"code"`
	Err   string `json:"err,omitempty"`
	Pulse string `json:"pulse,omitempty"` // in "status": the session's current pulse
	Since int64  `json:"since,omitempty"` // pulse start, Unix ms
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
	entry, ok := s.o.Sessions.Get(r.URL.Query().Get("session"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	// The PIN unlock key arrives as a WebSocket subprotocol ("cr-unlock, <key>")
	// because browsers cannot set headers on a WebSocket and a URL parameter
	// would end up in logs and history.
	lockSess, lockKey, _ := s.lockCreds(r)
	bearer := auth.BearerToken(r) != ""
	// Evaluated on every check, not once: a PIN set while this connection is
	// open must start applying to it too.
	lockApplies := func() bool { return s.o.Lock != nil && !bearer && s.o.Lock.Enabled() }
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{lockProto}})
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

	sess := entry.S
	if err := sess.Ensure(); err != nil {
		// Details go to the log only; the client gets a category (see Snapshot.ErrKind).
		s.o.Log.Error("session start failed", "session", entry.ID, "err", err)
	}
	sub, snap, err := sess.Subscribe()
	if err != nil {
		c.Close(websocket.StatusGoingAway, "session closed")
		return
	}
	defer sub.Close()
	s.o.Log.Info("ws connected", "ip", ip, "session", entry.ID)
	defer s.o.Log.Info("ws disconnected", "ip", ip, "session", entry.ID)

	state := "running"
	if snap.State == session.Exited {
		state = "exited"
	}
	// The first message carries the current pulse too, so no extra frame is needed.
	tool := entry.Tool
	if tool == "" {
		tool = toolClaude
	}
	pulse, since := pulseOf(sess.Info(), tool)
	if writeJSON(ctx, c, ctrlMsg{Type: "status", State: state, Code: snap.ExitCode, Err: snap.ErrKind, Pulse: pulse, Since: since}) != nil ||
		writeBinary(ctx, c, snap.Replay) != nil {
		return
	}

	// While the PIN lock is on, the connection ends when the unlock key
	// expires (checked on every ping; the key's idle timer is fed by input).
	alive := func() bool { return !lockApplies() || s.o.Lock.Valid(lockSess, lockKey) }
	go s.pump(ctx, cancel, c, sub)
	go s.ping(ctx, cancel, c)
	go s.watchLock(ctx, cancel, c, alive)

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if lockApplies() && !s.o.Lock.Check(lockSess, lockKey) { // input is activity
			c.Close(websocket.StatusPolicyViolation, "locked")
			return
		}
		switch typ {
		case websocket.MessageBinary:
			// Answering a prompt means Claude is working again.
			if sess.Info().Activity == "waiting" {
				sess.SetActivity("working")
			}
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
				s.o.Log.Info("session restart requested", "ip", ip, "session", entry.ID)
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
			case ev.Pulse != "":
				err = writeJSON(ctx, c, ctrlMsg{Type: "pulse", State: ev.Pulse, Since: ev.PulseSince.UnixMilli()})
			case ev.Exited:
				err = writeJSON(ctx, c, ctrlMsg{Type: "exit", Code: ev.ExitCode, Err: ev.ErrKind})
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

// watchLock ends the connection soon after the PIN lock closes (idle expiry,
// "lock now", or a PIN set while connected), so output stops streaming within
// seconds, not after the next 30-second ping.
func (s *Server) watchLock(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, alive func() bool) {
	every := 5 * time.Second
	if s.o.PingInterval < every {
		every = s.o.PingInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !alive() {
				c.Close(websocket.StatusPolicyViolation, "locked")
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
