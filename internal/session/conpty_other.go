//go:build !windows

package session

import "errors"

// NewStarter exists so the code builds (go vet in CI) on non-Windows systems;
// the real implementation needs ConPTY.
func NewStarter(cmdline, workdir string) StartFunc {
	return func(cols, rows int) (PTY, error) {
		return nil, errors.New("ConPTY is only available on Windows")
	}
}
