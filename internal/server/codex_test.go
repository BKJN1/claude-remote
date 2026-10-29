package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-remote/internal/session"
)

const (
	codexA = "019ed1cd-5c58-71a1-bf33-fb580807e9f5"
	codexB = "01a18422-c7ea-7de0-aa54-2241643b6c1b"
)

// codexEnv builds a server where both tools are available and Codex's history
// lives in a temporary home.
func codexEnv(t *testing.T) (*env, string) {
	root := t.TempDir()
	e := newEnv(t, func(o *Options) {
		o.CodexRoot = root
		o.ToolAvailable = func(tool string) bool { return tool == "claude" || tool == "codex" }
		o.NotifySecret = "codex-secret-codex-secret"
	})
	return e, root
}

func seedCodex(t *testing.T, root, id, cwd, firstUser string) {
	t.Helper()
	dir := filepath.Join(root, "sessions", "2026", "10", "29")
	os.MkdirAll(dir, 0o700)
	content := `{"type":"session_meta","payload":{"id":"` + id + `","cwd":` + jsonString(cwd) + `}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":` + jsonString(firstUser) + `}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-29T10-00-00-"+id+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestToolsListsAvailability(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.ToolAvailable = func(tool string) bool { return tool == "claude" } })
	_, body := call(t, e, "GET", "/api/tools", nil, false, nil)
	s := string(body)
	if !strings.Contains(s, `"id":"claude"`) || !strings.Contains(s, `"id":"codex"`) ||
		!strings.Contains(s, `"name":"Claude Code"`) || !strings.Contains(s, `"name":"Codex"`) {
		t.Fatalf("%s", s)
	}
	if !strings.Contains(s, `"available":true`) || !strings.Contains(s, `"available":false`) {
		t.Fatalf("availability not reported: %s", s)
	}
}

func TestCreateCodexSession(t *testing.T) {
	e, _ := codexEnv(t)
	resp, body := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj","tool":"codex"}`), true, nil)
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(body), `"tool":"codex"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if entry := e.mgr.List()[0]; entry.Tool != "codex" || entry.Resume != "" {
		t.Fatalf("%+v", entry.Meta)
	}
	// Without a tool the default is Claude Code, exactly as before.
	call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj"}`), true, nil)
	if e.mgr.List()[1].Tool != "claude" {
		t.Fatalf("default tool %q", e.mgr.List()[1].Tool)
	}
}

func TestUnknownOrUnavailableToolRefused(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.ToolAvailable = func(tool string) bool { return tool == "claude" } })
	if resp, body := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj","tool":"codex"}`), true, nil); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "tool_unavailable") {
		t.Fatalf("unavailable: %d %s", resp.StatusCode, body)
	}
	for _, tool := range []string{"powershell", "../codex", "codex --yolo", "CODEX"} {
		resp, _ := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj","tool":"`+tool+`"}`), true, nil)
		if resp.StatusCode < 400 {
			t.Errorf("tool %q accepted", tool)
		}
	}
	if e.pty.Count() != 0 {
		t.Fatal("a process was started for a refused tool")
	}
}

func TestCodexHistoryIsPerProjectAndPerTool(t *testing.T) {
	e, root := codexEnv(t)
	seedCodex(t, root, codexA, e.workdir, "Fix the build")
	seedCodex(t, root, codexB, filepath.Join(t.TempDir(), "elsewhere"), "Unrelated")

	_, body := call(t, e, "GET", "/api/history?project=proj&tool=codex", nil, false, nil)
	if !strings.Contains(string(body), codexA) || strings.Contains(string(body), codexB) || !strings.Contains(string(body), "Fix the build") {
		t.Fatalf("%s", body)
	}
	// The same project queried for Claude does not show Codex conversations.
	_, body = call(t, e, "GET", "/api/history?project=proj", nil, false, nil)
	if strings.Contains(string(body), codexA) {
		t.Fatalf("tools mixed: %s", body)
	}
	if resp, _ := call(t, e, "GET", "/api/history?project=proj&tool=nope", nil, false, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown tool: %d", resp.StatusCode)
	}
}

func TestResumeCodexConversation(t *testing.T) {
	e, root := codexEnv(t)
	seedCodex(t, root, codexA, e.workdir, "Fix the build")
	resp, body := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj","tool":"codex","resume":"`+codexA+`"}`), true, nil)
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(body), "Fix the build") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if entry := e.mgr.List()[0]; entry.Tool != "codex" || entry.Resume != codexA {
		t.Fatalf("%+v", entry.Meta)
	}
	// An id that is not one of THIS project's Codex conversations is refused.
	if resp, _ := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj","tool":"codex","resume":"`+codexB+`"}`), true, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign conversation: %d", resp.StatusCode)
	}
	for _, bad := range []string{"--last", "--yolo", codexA + " --yolo"} {
		if resp, _ := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj","tool":"codex","resume":"`+bad+`"}`), true, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("resume %q: %d", bad, resp.StatusCode)
		}
	}
}

func TestAllCodexHistoryAndOpenFromAnyFolder(t *testing.T) {
	e, root := codexEnv(t)
	other := t.TempDir() // not a configured project
	seedCodex(t, root, codexA, other, "Old Codex task")

	_, body := call(t, e, "GET", "/api/history/all?tool=codex", nil, false, nil)
	if !strings.Contains(string(body), "Old Codex task") || strings.Contains(string(body), other) {
		t.Fatalf("%s", body)
	}
	resp, body := call(t, e, "POST", "/api/history/open", []byte(`{"id":"`+codexA+`","tool":"codex"}`), true, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	entry := e.mgr.List()[0]
	if entry.Tool != "codex" || entry.Resume != codexA || entry.Workdir != other {
		t.Fatalf("%+v", entry.Meta)
	}
}

// Claude's "open in a laptop window" check must not apply to Codex ids.
func TestCodexOpenIgnoresClaudeLiveSessions(t *testing.T) {
	e, f := laptopEnv(t) // has a live Claude session with id idInteractive
	e.srv.o.CodexRoot = t.TempDir()
	e.srv.o.ToolAvailable = func(tool string) bool { return true }
	seedCodex(t, e.srv.o.CodexRoot, idInteractive, t.TempDir(), "Codex conversation with the same id")
	resp, _ := call(t, e, "POST", "/api/history/open", []byte(`{"id":"`+idInteractive+`","tool":"codex"}`), true, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("%d", resp.StatusCode)
	}
	if len(f.killed) != 0 {
		t.Fatal("something was closed")
	}
}

func TestCodexNotifySendsPushButNeverPinsThePulse(t *testing.T) {
	e, _ := codexEnv(t)
	internal := httptest.NewServer(e.srv.InternalHandler())
	defer internal.Close()
	entry, _ := e.mgr.CreateTool("codex", "proj", e.workdir, "t", "")

	post := func(event string) {
		req, _ := http.NewRequest("POST", internal.URL+"/notify", strings.NewReader(`{"session":"`+entry.ID+`","event":"`+event+`"}`))
		req.Header.Set("Authorization", "Bearer codex-secret-codex-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	for _, ev := range []string{"working", "notification", "stop"} {
		post(ev)
	}
	if got := entry.S.Info().Activity; got != "" {
		t.Fatalf("Codex events must not set the hook-based pulse, got %q", got)
	}
	// And a Claude session still does.
	c, _ := e.mgr.Create("proj", e.workdir, "c", "")
	req, _ := http.NewRequest("POST", internal.URL+"/notify", strings.NewReader(`{"session":"`+c.ID+`","event":"working"}`))
	req.Header.Set("Authorization", "Bearer codex-secret-codex-secret")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if c.S.Info().Activity != "working" {
		t.Fatal("Claude pulse broken")
	}
}

func TestPulseOfIgnoresHooksForCodex(t *testing.T) {
	info := session.Info{State: session.Running, Activity: "idle", ActivitySince: time.Now(), LastOutput: time.Now()}
	if state, _ := pulseOf(info, "codex"); state != "working" {
		t.Fatalf("codex with fresh output: %q", state)
	}
	if state, _ := pulseOf(info, "claude"); state != "idle" {
		t.Fatalf("claude hook state: %q", state)
	}
}
