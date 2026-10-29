package server

import (
	"time"

	"claude-remote/internal/session"
)

// pulseOf summarises what a session is doing: "working", "waiting" (Claude
// needs the user), "idle" or "exited". The state comes from Claude's hooks
// when they are active. Otherwise (background sessions, other commands) it is
// guessed from recent output: anything printed in the last 3 seconds counts as
// working.
func pulseOf(info session.Info, tool string) (state string, sinceMS int64) {
	if info.State == session.Exited {
		return "exited", 0
	}
	// Codex has no per-turn hooks, so its state is only ever guessed from output.
	if tool != toolCodex && info.Activity != "" {
		return info.Activity, info.ActivitySince.UnixMilli()
	}
	if !info.LastOutput.IsZero() && time.Since(info.LastOutput) < 3*time.Second {
		return "working", info.LastOutput.UnixMilli()
	}
	return "idle", 0
}
