//go:build windows

package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

// Spec describes the process to launch. Every field comes from launch flags
// or server-side logic, never from a client.
type Spec struct {
	Cmd       string   // command line, e.g. `claude`
	ExtraArgs []string // appended to Cmd's arguments
	Workdir   string
	Env       []string // full environment; nil inherits the server's
}

// NewStarter returns a StartFunc that launches spec inside a ConPTY.
func NewStarter(spec Spec) StartFunc {
	return func(cols, rows int) (PTY, error) {
		if spec.Workdir != "" {
			if fi, err := os.Stat(spec.Workdir); err != nil || !fi.IsDir() {
				return nil, &StartError{Kind: KindWorkdirMissing, Err: fmt.Errorf("working directory %q is not available", spec.Workdir)}
			}
		}
		full, err := resolveCommand(spec.Cmd, spec.ExtraArgs)
		if err != nil {
			return nil, err
		}
		opts := []conpty.ConPtyOption{conpty.ConPtyDimensions(cols, rows), conpty.ConPtyWorkDir(spec.Workdir)}
		if spec.Env != nil {
			opts = append(opts, conpty.ConPtyEnv(spec.Env))
		}
		cp, err := conpty.Start(full, opts...)
		if err != nil {
			return nil, &StartError{Kind: KindStartFailed, Err: err}
		}
		job, err := newKillOnCloseJob()
		if err != nil {
			cp.Close()
			return nil, &StartError{Kind: KindStartFailed, Err: err}
		}
		// Fail closed: a process we cannot put in the job could leave orphans.
		if err := assignToJob(job, uint32(cp.Pid())); err != nil {
			windows.CloseHandle(job)
			cp.Close()
			return nil, &StartError{Kind: KindStartFailed, Err: fmt.Errorf("assign process to job object: %w", err)}
		}
		p := &conptyPTY{cp: cp, job: job, exited: make(chan struct{})}
		go func() {
			code, _ := cp.Wait(context.Background())
			p.code = int(code)
			close(p.exited)
		}()
		return p, nil
	}
}

type conptyPTY struct {
	cp     *conpty.ConPty
	job    windows.Handle
	exited chan struct{}
	code   int // written before exited is closed

	closeOnce sync.Once
}

func (p *conptyPTY) Read(b []byte) (int, error)  { return p.cp.Read(b) }
func (p *conptyPTY) Write(b []byte) (int, error) { return p.cp.Write(b) }
func (p *conptyPTY) Resize(cols, rows int) error { return p.cp.Resize(cols, rows) }
func (p *conptyPTY) Pid() int                    { return p.cp.Pid() }

func (p *conptyPTY) Wait() (int, error) {
	<-p.exited
	return p.code, nil
}

// Close kills the whole process tree. Closing the last job handle terminates
// every process in the job (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE), including
// grandchildren that ClosePseudoConsole alone would leave running. The same
// flag also kills the tree if this server is itself killed. Order matters:
// kill first, wait for the waiter goroutine to stop using the process handle,
// and only then let conpty close that handle.
func (p *conptyPTY) Close() error {
	p.closeOnce.Do(func() {
		windows.CloseHandle(p.job)
		select {
		case <-p.exited:
		case <-time.After(5 * time.Second):
		}
		p.cp.Close()
	})
	return nil
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// assignToJob runs just after CreateProcess. There is a tiny window in which
// the new process could spawn a child before it is in the job (the conpty
// library cannot start suspended); a shell or Claude Code does not do that
// within microseconds of starting.
func assignToJob(job windows.Handle, pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(job, h)
}

// resolveCommand turns the command and extra args into a CreateProcess
// command line. CreateProcess only finds .exe files on PATH, but Claude Code
// installed via npm is a claude.cmd shim, so resolve with PATHEXT and wrap
// batch files in cmd.exe.
func resolveCommand(cmdline string, extra []string) (string, error) {
	args, err := windows.DecomposeCommandLine(cmdline)
	if err != nil || len(args) == 0 {
		return "", &StartError{Kind: KindStartFailed, Err: fmt.Errorf("invalid command %q", cmdline)}
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return "", &StartError{Kind: KindCommandNotFound, Err: fmt.Errorf("command %q not found: %w", args[0], err)}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	args[0] = path
	args = append(args, extra...)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cmd", ".bat":
		// cmd.exe re-parses the line, so & | < > ^ % in any part (paths,
		// arguments) would be interpreted as shell syntax.
		for _, a := range args {
			if strings.ContainsAny(a, "&|<>^%") {
				return "", &StartError{Kind: KindStartFailed, Err: fmt.Errorf("argument %q contains characters cmd.exe would interpret; use a path without & | < > ^ %%", a)}
			}
		}
		return `cmd.exe /d /s /c "` + windows.ComposeCommandLine(args) + `"`, nil
	}
	return windows.ComposeCommandLine(args), nil
}
