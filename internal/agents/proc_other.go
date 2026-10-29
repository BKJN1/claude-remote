//go:build !windows

package agents

import (
	"errors"
	"os/exec"
)

func hideWindow(cmd *exec.Cmd) {}

// isOurs: off Windows nothing is excluded (this program only runs terminals on Windows).
func isOurs(pid int) bool { return false }

func verifyAndKill(a Agent) error {
	return errors.New("closing laptop sessions is only supported on Windows")
}
