package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const idClosed = "11111111-1111-4111-8111-111111111111"

// seedConversation writes a past conversation under a folder that differs from
// every configured project.
func seedConversation(t *testing.T, e *env, id, cwd string) {
	t.Helper()
	dir := filepath.Join(e.srv.o.HistoryRoot, "some-other-folder")
	os.MkdirAll(dir, 0o700)
	content := `{"type":"user","cwd":` + jsonString(cwd) + `,"message":{"role":"user","content":"Old task"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func TestClosedConversationsFromAnyFolderAreListed(t *testing.T) {
	e, _ := laptopEnv(t)
	other := t.TempDir() // not one of the configured projects
	seedConversation(t, e, idClosed, other)
	resp, body := call(t, e, "GET", "/api/history/all", nil, false, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Old task") || !strings.Contains(string(body), idClosed) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), other) {
		t.Fatalf("full folder path leaked: %s", body)
	}
}

func TestOpenClosedConversationResumesInItsOwnFolder(t *testing.T) {
	e, f := laptopEnv(t)
	other := t.TempDir()
	seedConversation(t, e, idClosed, other)
	resp, body := call(t, e, "POST", "/api/history/open", []byte(`{"id":"`+idClosed+`"}`), true, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	entry := e.mgr.List()[0]
	if entry.Resume != idClosed || entry.Workdir != other || entry.Title != "Old task" {
		t.Fatalf("%+v", entry.Meta)
	}
	if len(f.killed) != 0 {
		t.Fatal("opening a CLOSED conversation must not close anything")
	}
	// Opening it again reuses the same session.
	resp, _ = call(t, e, "POST", "/api/history/open", []byte(`{"id":"`+idClosed+`"}`), true, nil)
	if resp.StatusCode != http.StatusOK || len(e.mgr.List()) != 1 {
		t.Fatalf("duplicate session: %d, %d sessions", resp.StatusCode, len(e.mgr.List()))
	}
}

// A conversation that is open in a laptop window must go through the take flow.
func TestOpenRefusedWhileOpenInALaptopWindow(t *testing.T) {
	e, f := laptopEnv(t)
	seedConversation(t, e, idInteractive, t.TempDir())
	resp, body := call(t, e, "POST", "/api/history/open", []byte(`{"id":"`+idInteractive+`"}`), true, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "open_on_laptop") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if e.pty.Count() != 0 || len(f.killed) != 0 {
		t.Fatal("a live conversation was forked or closed")
	}
}

func TestOpenValidation(t *testing.T) {
	e, f := laptopEnv(t)
	for name, body := range map[string]string{
		"unknown id":     `{"id":"22222222-2222-4222-8222-222222222222"}`,
		"not a uuid":     `{"id":"../../x"}`,
		"option-like id": `{"id":"--dangerously-skip-permissions"}`,
		"empty":          `{}`,
		"bad json":       `{`,
	} {
		resp, _ := call(t, e, "POST", "/api/history/open", []byte(body), true, nil)
		if resp.StatusCode < 400 {
			t.Errorf("%s accepted: %d", name, resp.StatusCode)
		}
	}
	// A recorded folder that is a UNC path, relative, or no longer exists is refused.
	for name, cwd := range map[string]string{
		"unc":      `\\evil-host\share`,
		"relative": `relative\dir`,
		"missing":  filepath.Join(t.TempDir(), "gone"),
	} {
		seedConversation(t, e, idClosed, cwd)
		resp, body := call(t, e, "POST", "/api/history/open", []byte(`{"id":"`+idClosed+`"}`), true, nil)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "cannot_continue_here") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if e.pty.Count() != 0 || len(f.killed) != 0 {
		t.Fatal("something started or was closed by invalid requests")
	}
}

func TestOpenNeedsCSRFHeaderAndRoom(t *testing.T) {
	e, _ := laptopEnv(t)
	seedConversation(t, e, idClosed, t.TempDir())
	if resp, _ := call(t, e, "POST", "/api/history/open", []byte(`{"id":"`+idClosed+`"}`), false, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no CSRF header: %d", resp.StatusCode)
	}
	for i := 0; i < 5; i++ {
		e.mgr.Create("proj", e.workdir, "x", "")
	}
	if resp, _ := call(t, e, "POST", "/api/history/open", []byte(`{"id":"`+idClosed+`"}`), true, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("no room: %d", resp.StatusCode)
	}
}
