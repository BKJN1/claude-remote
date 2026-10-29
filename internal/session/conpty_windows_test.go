//go:build windows

package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConPTYEcho(t *testing.T) {
	p, err := NewStarter(Spec{Cmd: `cmd /c echo hello`})(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	out := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			sb.Write(buf[:n])
			if strings.Contains(sb.String(), "hello") {
				out <- sb.String()
				return
			}
			if err != nil {
				out <- sb.String()
				return
			}
		}
	}()
	select {
	case got := <-out:
		if !strings.Contains(got, "hello") {
			t.Fatalf("output %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for output")
	}
	code, _ := p.Wait()
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

func TestConPTYCloseKillsProcessTree(t *testing.T) {
	// cmd starts ping as a child; the unusual count makes it identifiable.
	const marker = "-n 61 127.0.0.1"
	p, err := NewStarter(Spec{Cmd: `cmd /c ping ` + marker + ` >nul`})(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	count := func() string {
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			`(Get-CimInstance Win32_Process -Filter "Name='PING.EXE'" | Where-Object { $_.CommandLine -like '*`+marker+`*' } | Measure-Object).Count`).Output()
		if err != nil {
			t.Skipf("cannot list processes: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	deadline := time.Now().Add(10 * time.Second)
	for count() != "1" {
		if time.Now().After(deadline) {
			p.Close()
			t.Fatal("child process never appeared")
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.Close()
	deadline = time.Now().Add(10 * time.Second)
	for count() != "0" {
		if time.Now().After(deadline) {
			t.Fatal("child process survived Close (orphan)")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestResolveCommandErrors(t *testing.T) {
	if _, err := resolveCommand("definitely-not-a-real-binary-xyz", nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestStartErrorKinds(t *testing.T) {
	_, err := NewStarter(Spec{Cmd: "definitely-not-a-real-binary-xyz"})(80, 24)
	var se *StartError
	if !errors.As(err, &se) || se.Kind != KindCommandNotFound {
		t.Fatalf("got %v", err)
	}
	_, err = NewStarter(Spec{Cmd: "cmd", Workdir: filepath.Join(t.TempDir(), "missing")})(80, 24)
	if !errors.As(err, &se) || se.Kind != KindWorkdirMissing {
		t.Fatalf("got %v", err)
	}
}

func TestExtraArgsAndEnvReachProcess(t *testing.T) {
	p, err := NewStarter(Spec{Cmd: `cmd /c echo %CR_TEST_VAR%`, Env: append(os.Environ(), "CR_TEST_VAR=from-env")})(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(sb.String(), "from-env") {
		n, err := p.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "from-env") {
		t.Fatalf("env not passed: %q", sb.String())
	}
}

func TestCmdShimRejectsShellMetacharacters(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "tool.cmd")
	os.WriteFile(shim, []byte("@echo off\r\n"), 0o600)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := resolveCommand("tool", []string{"--settings", `C:\Users\a&b\x.json`}); err == nil {
		t.Fatal("& in argument accepted for a .cmd shim")
	}
	if _, err := resolveCommand("tool", []string{"%PATH%"}); err == nil {
		t.Fatal("% in argument accepted for a .cmd shim")
	}
	if got, err := resolveCommand("tool", []string{"--settings", `C:\Users\a b\x.json`}); err != nil || !strings.Contains(got, "cmd.exe") {
		t.Fatalf("safe args rejected: %q %v", got, err)
	}
}
