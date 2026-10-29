// Package agents finds the Claude Code sessions running on this laptop (via
// `claude agents --json`) and can close one so it can be continued from the
// phone.
package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Agent is one live Claude Code session.
type Agent struct {
	PID       int    `json:"pid"`
	CWD       string `json:"cwd"`
	Kind      string `json:"kind"`      // "interactive" (a terminal window) or "background"
	StartedAt int64  `json:"startedAt"` // Unix milliseconds
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"` // "busy" or "idle"
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidID reports whether s is a plain session UUID.
func ValidID(s string) bool { return uuidRe.MatchString(s) }

// validCWD accepts only absolute local paths: a drive path (C:\x) or a POSIX
// path. UNC paths (\\host\share, \\?\) are refused: using one as a working
// directory would make Windows authenticate to a remote host.
var cwdRe = regexp.MustCompile(`^([A-Za-z]:[\\/]|/)`)

func validCWD(p string) bool {
	return cwdRe.MatchString(p) && !strings.HasPrefix(p, `\\`) && !strings.HasPrefix(p, "//")
}

// ValidCWD reports whether p is an absolute local path (no UNC paths).
func ValidCWD(p string) bool { return validCWD(p) }

// SafeName reports whether a session name may be passed on a command line:
// it must not look like an option or contain shell syntax.
var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func SafeName(s string) bool { return safeName.MatchString(s) }

// Parse decodes the output of `claude agents --json`, dropping entries that
// are malformed (the format is Claude's own and may change).
func Parse(data []byte) ([]Agent, error) {
	var all []Agent
	if err := json.Unmarshal(bytes.TrimSpace(data), &all); err != nil {
		return nil, fmt.Errorf("unexpected output from `claude agents --json`: %w", err)
	}
	out := all[:0]
	for _, a := range all {
		if a.PID <= 0 || !ValidID(a.SessionID) || !validCWD(a.CWD) {
			continue
		}
		if a.Kind != "background" {
			a.Kind = "interactive"
		}
		out = append(out, a)
	}
	return out, nil
}

// Provider lists and closes laptop sessions.
type Provider struct {
	program string
	run     func(ctx context.Context, args ...string) ([]byte, error)
}

// New builds a provider that runs the program from the launch command line
// (e.g. `claude`).
func New(program string) *Provider {
	p := &Provider{program: program}
	p.run = func(ctx context.Context, args ...string) ([]byte, error) {
		path, err := exec.LookPath(p.program)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Env = os.Environ()
		hideWindow(cmd)
		return cmd.Output()
	}
	return p
}

// List returns the live sessions, minus the ones this server runs itself
// (they are already in its own list).
func (p *Provider) List(ctx context.Context) ([]Agent, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	data, err := p.run(ctx, "agents", "--json")
	if err != nil {
		return nil, fmt.Errorf("claude agents: %w", err)
	}
	all, err := Parse(data)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, a := range all {
		if !isOurs(a.PID) {
			out = append(out, a)
		}
	}
	return out, nil
}

// Preflight checks, BEFORE anything is closed, that a session can be
// continued here: its folder is still there and the command is installed.
// Closing the laptop window and then failing to start would lose the window
// for nothing.
func (p *Provider) Preflight(a Agent) error {
	if !validCWD(a.CWD) {
		return fmt.Errorf("unsafe working directory")
	}
	if fi, err := os.Stat(a.CWD); err != nil || !fi.IsDir() {
		return fmt.Errorf("working directory %q is not available", a.CWD)
	}
	if _, err := exec.LookPath(p.program); err != nil {
		return fmt.Errorf("command %q not found: %w", p.program, err)
	}
	return nil
}

// Kill ends the process of a session obtained from a fresh List and waits
// until it is gone. The process is verified first (see verifyAndKill).
func (p *Provider) Kill(a Agent) error {
	if a.PID == os.Getpid() || isOurs(a.PID) {
		return fmt.Errorf("refusing to close a session owned by this server")
	}
	return verifyAndKill(a)
}

// Label turns a folder path into a short project label for the UI.
func Label(cwd string) string {
	cwd = strings.TrimRight(cwd, `\/`)
	if i := strings.LastIndexAny(cwd, `\/`); i >= 0 {
		cwd = cwd[i+1:]
	}
	var b strings.Builder
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == ' ', r > 127:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		return "проект"
	}
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40])
	}
	return s
}
