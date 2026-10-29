//go:build !windows

package session

import "errors"

// Spec describes the process to launch (see conpty_windows.go).
type Spec struct {
	Cmd       string
	ExtraArgs []string
	Workdir   string
	Env       []string
}

// NewStarter exists so the code builds (go vet in CI) on non-Windows systems;
// the real implementation needs ConPTY.
func NewStarter(spec Spec) StartFunc {
	return func(cols, rows int) (PTY, error) {
		return nil, errors.New("ConPTY is only available on Windows")
	}
}
