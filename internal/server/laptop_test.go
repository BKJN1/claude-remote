package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"claude-remote/internal/agents"
)

const (
	idInteractive = "c30f70ea-4877-4aa0-8ea7-a19c07c7d6e7"
	idBackground  = "56a6722b-4cd9-40ad-bf98-52fb000ce657"
	idUnsafe      = "393e2175-a7cf-4ee7-9bfe-6c6fb0aae155"
)

type fakeAgents struct {
	mu      sync.Mutex
	list    []agents.Agent
	listErr error
	killErr error
	preErr  error
	killed  []agents.Agent
}

func (f *fakeAgents) Preflight(a agents.Agent) error { return f.preErr }

func (f *fakeAgents) List(ctx context.Context) ([]agents.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agents.Agent(nil), f.list...), f.listErr
}

func (f *fakeAgents) Kill(a agents.Agent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.killErr != nil {
		return f.killErr
	}
	f.killed = append(f.killed, a)
	return nil
}

func laptopEnv(t *testing.T) (*env, *fakeAgents) {
	f := &fakeAgents{list: []agents.Agent{
		{PID: 4242, CWD: `C:\work\other-folder`, Kind: "interactive", StartedAt: 1793171045708, SessionID: idInteractive, Name: "versola-d4", Status: "idle"},
		{PID: 4343, CWD: `C:\work\bg`, Kind: "background", StartedAt: 1793171122407, SessionID: idBackground, Name: "nightly", Status: "busy"},
		{PID: 4444, CWD: `C:\work\evil`, Kind: "background", StartedAt: 1, SessionID: idUnsafe, Name: "--dangerously-skip-permissions", Status: "idle"},
	}}
	e := newEnv(t, func(o *Options) { o.Agents = f })
	return e, f
}

func TestLaptopListHidesPathsAndPIDs(t *testing.T) {
	e, _ := laptopEnv(t)
	resp, body := call(t, e, "GET", "/api/laptop", nil, false, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	s := string(body)
	if !strings.Contains(s, "versola-d4") || !strings.Contains(s, `"folder":"other-folder"`) {
		t.Fatalf("body %s", s)
	}
	for _, leak := range []string{`C:\\work`, "4242", `"pid"`, "cwd"} {
		if strings.Contains(s, leak) {
			t.Errorf("response leaks %q: %s", leak, s)
		}
	}
}

func TestLaptopRequiresAuth(t *testing.T) {
	e, _ := laptopEnv(t)
	for _, c := range []struct{ m, p string }{{"GET", "/api/laptop"}, {"POST", "/api/laptop/take"}} {
		req, _ := http.NewRequest(c.m, e.ts.URL+c.p, nil)
		req.Header.Set("X-CR", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", c.m, c.p, resp.StatusCode)
		}
	}
}

func TestTakeInteractiveNeedsConfirmationAndNothingHappensWithout(t *testing.T) {
	e, f := laptopEnv(t)
	resp, body := call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idInteractive+`"}`), true, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "confirm_required") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(f.killed) != 0 || e.pty.Count() != 0 {
		t.Fatal("something was closed or started without confirmation")
	}
}

func TestTakeBusyNeedsSecondConfirmation(t *testing.T) {
	e, f := laptopEnv(t)
	f.list[0].Status = "busy"
	resp, body := call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idInteractive+`","confirm":true}`), true, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "confirm_busy_required") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(f.killed) != 0 {
		t.Fatal("a busy session was closed without the extra confirmation")
	}
	resp, _ = call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idInteractive+`","confirm":true,"confirm_busy":true}`), true, nil)
	if resp.StatusCode != http.StatusCreated || len(f.killed) != 1 {
		t.Fatalf("%d killed=%d", resp.StatusCode, len(f.killed))
	}
}

func TestTakeInteractiveClosesThenResumesHere(t *testing.T) {
	e, f := laptopEnv(t)
	resp, body := call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idInteractive+`","confirm":true}`), true, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(f.killed) != 1 || f.killed[0].PID != 4242 {
		t.Fatalf("killed %+v", f.killed)
	}
	entry := e.mgr.List()[0]
	if entry.Resume != idInteractive || entry.Workdir != `C:\work\other-folder` || entry.Project != "other-folder" || entry.Title != "versola-d4" {
		t.Fatalf("session %+v", entry.Meta)
	}
}

func TestTakeDoesNotResumeIfCloseFails(t *testing.T) {
	e, f := laptopEnv(t)
	f.killErr = errors.New("access denied")
	resp, _ := call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idInteractive+`","confirm":true}`), true, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if e.pty.Count() != 0 {
		t.Fatal("resumed while the original is still running (would fork the conversation)")
	}
}

func TestTakeBackgroundAttachesWithoutClosing(t *testing.T) {
	e, f := laptopEnv(t)
	resp, body := call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idBackground+`"}`), true, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(f.killed) != 0 {
		t.Fatal("a background session was killed")
	}
	if entry := e.mgr.List()[0]; entry.Attach != "nightly" || entry.Resume != "" {
		t.Fatalf("%+v", entry.Meta)
	}
}

func TestTakeRefusesUnsafeNamesAndUnknownIDs(t *testing.T) {
	e, _ := laptopEnv(t)
	resp, _ := call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idUnsafe+`"}`), true, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || e.pty.Count() != 0 {
		t.Fatalf("option-like name accepted: %d", resp.StatusCode)
	}
	for _, body := range []string{
		`{"id":"11111111-1111-4111-8111-111111111111"}`, // valid but not running
		`{"id":"../../etc"}`,
		`{"id":"4242"}`, // a PID is not accepted
		`{"pid":4242,"confirm":true}`,
		`{`,
	} {
		resp, _ := call(t, e, "POST", "/api/laptop/take", []byte(body), true, nil)
		if resp.StatusCode < 400 {
			t.Errorf("%s accepted: %d", body, resp.StatusCode)
		}
	}
	if e.pty.Count() != 0 {
		t.Fatal("a session was started by an invalid request")
	}
}

func TestTakeRequiresCSRFHeader(t *testing.T) {
	e, f := laptopEnv(t)
	resp, _ := call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idInteractive+`","confirm":true}`), false, nil)
	if resp.StatusCode != http.StatusForbidden || len(f.killed) != 0 {
		t.Fatalf("%d killed=%d", resp.StatusCode, len(f.killed))
	}
}

func TestLaptopDisabledWithoutProvider(t *testing.T) {
	e := newEnv(t, nil)
	resp, body := call(t, e, "GET", "/api/laptop", nil, false, nil)
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, _ = call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idInteractive+`","confirm":true}`), true, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("%d", resp.StatusCode)
	}
}

// Closing the laptop window and then failing to continue loses the window for
// nothing, so everything that can fail is checked first.
func TestNothingIsClosedIfItCannotContinueHere(t *testing.T) {
	e, f := laptopEnv(t)
	f.preErr = errors.New("working directory is not available")
	resp, body := call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idInteractive+`","confirm":true}`), true, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "cannot_continue_here") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(f.killed) != 0 || e.pty.Count() != 0 {
		t.Fatal("the window was closed although continuing was impossible")
	}
}

func TestNothingIsClosedWhenThereIsNoRoom(t *testing.T) {
	e, f := laptopEnv(t)
	for i := 0; i < 5; i++ { // the test manager allows 5 sessions
		if _, err := e.mgr.Create("proj", e.workdir, "x", ""); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idInteractive+`","confirm":true}`), true, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "too_many_sessions") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(f.killed) != 0 {
		t.Fatal("the window was closed although the phone had no room")
	}
}

// A double tap or two devices must not fork the conversation.
func TestRepeatedTakeReusesTheExistingSession(t *testing.T) {
	e, f := laptopEnv(t)
	body := []byte(`{"id":"` + idInteractive + `","confirm":true}`)
	r1, b1 := call(t, e, "POST", "/api/laptop/take", body, true, nil)
	r2, b2 := call(t, e, "POST", "/api/laptop/take", body, true, nil)
	if r1.StatusCode != http.StatusCreated || r2.StatusCode != http.StatusOK {
		t.Fatalf("%d %d", r1.StatusCode, r2.StatusCode)
	}
	if len(f.killed) != 1 || len(e.mgr.List()) != 1 || e.pty.Count() != 1 {
		t.Fatalf("killed=%d sessions=%d processes=%d", len(f.killed), len(e.mgr.List()), e.pty.Count())
	}
	var v1, v2 sessionView
	json.Unmarshal(b1, &v1)
	json.Unmarshal(b2, &v2)
	if v1.ID == "" || v1.ID != v2.ID {
		t.Fatalf("second take opened a different session: %q vs %q", v1.ID, v2.ID)
	}
	// Same for a background session: no second `claude attach`.
	bg := []byte(`{"id":"` + idBackground + `"}`)
	call(t, e, "POST", "/api/laptop/take", bg, true, nil)
	call(t, e, "POST", "/api/laptop/take", bg, true, nil)
	if len(e.mgr.List()) != 2 {
		t.Fatalf("background attached twice: %d sessions", len(e.mgr.List()))
	}
}

func TestParallelTakesCloseOnlyOnce(t *testing.T) {
	e, f := laptopEnv(t)
	body := []byte(`{"id":"` + idInteractive + `","confirm":true}`)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); call(t, e, "POST", "/api/laptop/take", body, true, nil) }()
	}
	wg.Wait()
	f.mu.Lock()
	killed := len(f.killed)
	f.mu.Unlock()
	if killed != 1 || len(e.mgr.List()) != 1 {
		t.Fatalf("killed=%d sessions=%d", killed, len(e.mgr.List()))
	}
}

// People recognise a conversation by its title, not by Claude's short handle.
func TestLaptopListShowsConversationTitles(t *testing.T) {
	e, _ := laptopEnv(t)
	dir := filepath.Join(e.srv.o.HistoryRoot, "any-folder")
	os.MkdirAll(dir, 0o700)
	content := `{"type":"user","cwd":"C:\\work\\other-folder","message":{"role":"user","content":"first prompt"}}` + "\n" +
		`{"type":"ai-title","aiTitle":"Release notes research"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, idInteractive+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, body := call(t, e, "GET", "/api/laptop", nil, false, nil)
	s := string(body)
	if !strings.Contains(s, `"title":"Release notes research"`) {
		t.Fatalf("title missing: %s", s)
	}
	if !strings.Contains(s, `"name":"versola-d4"`) {
		t.Fatalf("the short handle should still be available: %s", s)
	}
	// A session whose file cannot be found falls back to the handle.
	if !strings.Contains(s, `"title":"nightly"`) {
		t.Fatalf("fallback to the handle: %s", s)
	}
	// The session created when taking it carries the real title.
	resp, _ := call(t, e, "POST", "/api/laptop/take", []byte(`{"id":"`+idInteractive+`","confirm":true}`), true, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("take: %d", resp.StatusCode)
	}
	if got := e.mgr.List()[0].Title; got != "Release notes research" {
		t.Fatalf("session title %q", got)
	}
}
