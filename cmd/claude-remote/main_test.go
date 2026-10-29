package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsClaude(t *testing.T) {
	for cmd, want := range map[string]bool{
		"claude":                             true,
		"claude.cmd":                         true,
		`C:\Users\x\npm\claude.exe`:          true,
		`"C:\Program Files\x\claude.exe" -x`: true,
		"powershell":                         false,
		"cmd.exe /c claude":                  false,
		"claude-remote":                      false,
		"":                                   false,
	} {
		if got := isClaude(cmd); got != want {
			t.Errorf("isClaude(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestResolveProjects(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	o := &options{projects: multiFlag{"alpha=" + a, "beta=" + b}}
	got, err := o.resolveProjects()
	if err != nil || len(got) != 2 || got[0].Name != "alpha" || got[1].Path != b {
		t.Fatalf("%v %v", got, err)
	}

	// Name defaults to the folder name, and --workdir adds a project.
	o = &options{workdir: a}
	got, err = o.resolveProjects()
	if err != nil || len(got) != 1 || got[0].Name != filepath.Base(a) {
		t.Fatalf("%v %v", got, err)
	}

	bad := map[string]*options{
		"missing folder":  {projects: multiFlag{"x=" + filepath.Join(a, "nope")}},
		"duplicate name":  {projects: multiFlag{"same=" + a, "SAME=" + b}},
		"path-like name":  {projects: multiFlag{`..\evil=` + a}},
		"overlong name":   {projects: multiFlag{strings.Repeat("a", 41) + "=" + a}},
		"file not folder": {projects: multiFlag{"f=" + writeFile(t, a)}},
	}
	for name, opt := range bad {
		if _, err := opt.resolveProjects(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func writeFile(t *testing.T, dir string) string {
	p := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWriteHooks(t *testing.T) {
	dir := t.TempDir()
	path, err := writeHooks(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"UserPromptSubmit", "Notification", "Stop"} {
		h := cfg.Hooks[ev]
		if len(h) != 1 || len(h[0].Hooks) != 1 || h[0].Hooks[0].Type != "command" ||
			!strings.HasSuffix(h[0].Hooks[0].Command, `" notify`) || strings.Contains(h[0].Hooks[0].Command, `\`) {
			t.Errorf("%s hook malformed: %+v", ev, h)
		}
	}
}

func TestFriendlyErrors(t *testing.T) {
	if got := friendly(errString("no Tailscale interface with an address")); !strings.Contains(got, "Tailscale") || !strings.Contains(got, "tailscale ip") {
		t.Errorf("no hint: %q", got)
	}
	if got := friendly(errString("listen tcp: bind: Only one usage of each socket address")); !strings.Contains(got, "порт уже занят") {
		t.Errorf("no hint: %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestChildEnvReplacesExistingHookVariables(t *testing.T) {
	t.Setenv(envNotifySecret, "stale-secret")
	t.Setenv(envSession, "stale-session")
	env := childEnv("http://127.0.0.1:1/notify", "new-secret", "abc")
	count := map[string]int{}
	for _, kv := range env {
		for _, k := range []string{envNotifyURL, envNotifySecret, envSession} {
			if strings.HasPrefix(strings.ToUpper(kv), k+"=") {
				count[k]++
			}
		}
	}
	for _, k := range []string{envNotifyURL, envNotifySecret, envSession} {
		if count[k] != 1 {
			t.Errorf("%s appears %d times", k, count[k])
		}
	}
	for _, kv := range env {
		if strings.Contains(kv, "stale-") {
			t.Errorf("stale value kept: %s", kv)
		}
	}
}
