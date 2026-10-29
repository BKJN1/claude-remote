//go:build windows

package agents

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// parents maps pid -> parent pid from a process snapshot.
func parents() map[uint32]uint32 {
	m := map[uint32]uint32{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return m
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		m[e.ProcessID] = e.ParentProcessID
	}
	return m
}

// isOurs reports whether pid belongs to this server's own family: the server
// itself, its descendants (the sessions it runs), or its ancestors (for
// example the Claude session this server was started from). None of them may
// be offered or closed from the phone.
func isOurs(pid int) bool {
	self := uint32(os.Getpid())
	m := parents()
	// descendants: walk up from the candidate
	cur := uint32(pid)
	for i := 0; i < 64 && cur != 0; i++ {
		if cur == self {
			return true
		}
		cur = m[cur]
	}
	// ancestors: walk up from the server
	cur = m[self]
	for i := 0; i < 64 && cur != 0; i++ {
		if cur == uint32(pid) {
			return true
		}
		cur = m[cur]
	}
	return false
}

// imageOK reports whether an executable file name is a Claude Code session.
// Claude Code renames its running binary when it self-updates, so a live
// session can be "claude.exe.old.<number>".
var imageRe = regexp.MustCompile(`^(claude(\.exe)?(\.old\.[0-9]+)?|node\.exe|bun\.exe)$`)

func imageOK(name string) bool { return imageRe.MatchString(strings.ToLower(name)) }

// startTolerance is how far the process creation time may differ from the
// session's reported start (observed: up to ~16 s, as the session registers
// itself shortly after the process starts).
const startTolerance = 60 * time.Second

// verifyAndKill terminates pid only after checking that it still is the
// session we were told about. PIDs are reused, so the image name and the
// process start time must both match what `claude agents` reported; otherwise
// an unrelated process could be killed.
func verifyAndKill(a Agent) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(a.PID))
	if err != nil {
		return fmt.Errorf("open process %d: %w", a.PID, err)
	}
	defer windows.CloseHandle(h)

	var buf [windows.MAX_PATH * 2]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return fmt.Errorf("query image of %d: %w", a.PID, err)
	}
	image := strings.ToLower(filepath.Base(windows.UTF16ToString(buf[:size])))
	if !imageOK(image) {
		return fmt.Errorf("process %d is %q, not a Claude Code session", a.PID, image)
	}

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return fmt.Errorf("process times of %d: %w", a.PID, err)
	}
	started := time.Unix(0, creation.Nanoseconds())
	reported := time.UnixMilli(a.StartedAt)
	if d := started.Sub(reported); d < -startTolerance || d > startTolerance {
		return fmt.Errorf("process %d started at %s but the session reports %s: refusing (PID reuse?)", a.PID, started.Format(time.RFC3339), reported.Format(time.RFC3339))
	}

	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminate %d: %w", a.PID, err)
	}
	ev, err := windows.WaitForSingleObject(h, 5000)
	if err != nil || ev != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("process %d did not exit", a.PID)
	}
	return nil
}
