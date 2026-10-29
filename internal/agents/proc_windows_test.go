//go:build windows

package agents

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// An unrelated process that happens to have the reported PID must never be killed.
func TestVerifyAndKillRefusesNonClaudeProcess(t *testing.T) {
	cmd := exec.Command("ping", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer cmd.Process.Kill()
	err := verifyAndKill(Agent{PID: cmd.Process.Pid, StartedAt: time.Now().UnixMilli()})
	if err == nil || !strings.Contains(err.Error(), "not a Claude Code session") {
		t.Fatalf("got %v", err)
	}
	// Still running: Wait would return immediately if it had been terminated.
	done := make(chan struct{})
	go func() { cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("unrelated process was killed")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestIsOursDetectsDescendants(t *testing.T) {
	cmd := exec.Command("ping", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer cmd.Process.Kill()
	if !isOurs(cmd.Process.Pid) {
		t.Fatal("child of this process not recognised as ours")
	}
	if isOurs(1) { // System Idle / init is never our descendant
		t.Fatal("pid 1 reported as ours")
	}
}

func TestImageNames(t *testing.T) {
	for name, want := range map[string]bool{
		"claude.exe":                   true,
		"Claude.EXE":                   true,
		"claude.exe.old.1793171133127": true, // renamed by a self-update while running
		"claude":                       true,
		"node.exe":                     true,
		"bun.exe":                      true,
		"claude.exe.bak":               false,
		"notclaude.exe":                false,
		"powershell.exe":               false,
		"cmd.exe":                      false,
		"claude.exe.old.":              false,
		"explorer.exe":                 false,
	} {
		if got := imageOK(name); got != want {
			t.Errorf("imageOK(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsOursCoversAncestors(t *testing.T) {
	if !isOurs(os.Getppid()) {
		t.Fatal("the server's parent process must never be offered or closed")
	}
	if !isOurs(os.Getpid()) {
		t.Fatal("the server itself must be excluded")
	}
}
