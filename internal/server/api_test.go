package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"claude-remote/internal/history"
	"claude-remote/internal/push"
	"claude-remote/internal/screen"
)

// call performs an authenticated request; csrf adds the X-CR header.
func call(t *testing.T, e *env, method, path string, body []byte, csrf bool, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	if csrf {
		req.Header.Set("X-CR", "1")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestAPIRequiresAuth(t *testing.T) {
	e := newEnv(t, nil)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/projects"}, {"GET", "/api/sessions"}, {"POST", "/api/sessions"},
		{"DELETE", "/api/sessions/abc"}, {"POST", "/api/upload?session=x"}, {"GET", "/api/displays"},
		{"GET", "/api/history?project=proj"}, {"GET", "/api/push/key"}, {"POST", "/api/push/subscribe"},
	} {
		req, _ := http.NewRequest(c.method, e.ts.URL+c.path, nil)
		req.Header.Set("X-CR", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without auth: %d", c.method, c.path, resp.StatusCode)
		}
	}
}

func TestProjectsDoNotExposePaths(t *testing.T) {
	e := newEnv(t, nil)
	_, body := call(t, e, "GET", "/api/projects", nil, false, nil)
	if !strings.Contains(string(body), `"proj"`) || strings.Contains(string(body), e.workdir) {
		t.Fatalf("body %s", body)
	}
}

func TestStateChangingAPINeedsCSRFHeader(t *testing.T) {
	e := newEnv(t, nil)
	resp, _ := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj"}`), false, nil)
	if resp.StatusCode != http.StatusForbidden || e.pty.Count() != 0 {
		t.Fatalf("status %d, started %d", resp.StatusCode, e.pty.Count())
	}
	// A foreign Origin is refused even with the header.
	resp, _ = call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj"}`), true, map[string]string{"Origin": "http://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", resp.StatusCode)
	}
	// Origin "null" (no-referrer) with the header is fine.
	resp, _ = call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj"}`), true, map[string]string{"Origin": "null"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("null origin: %d", resp.StatusCode)
	}
}

func TestCreateListDeleteSessions(t *testing.T) {
	e := newEnv(t, nil)
	resp, body := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj"}`), true, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var v sessionView
	json.Unmarshal(body, &v)
	if v.ID == "" || v.Project != "proj" || v.State != "running" || e.pty.Count() != 1 {
		t.Fatalf("view %+v count %d", v, e.pty.Count())
	}
	_, body = call(t, e, "GET", "/api/sessions", nil, false, nil)
	if !strings.Contains(string(body), v.ID) {
		t.Fatalf("list %s", body)
	}
	if resp, _ := call(t, e, "DELETE", "/api/sessions/"+v.ID, nil, true, nil); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if !e.pty.Last().IsClosed() {
		t.Fatal("process not killed on delete")
	}
	if resp, _ := call(t, e, "DELETE", "/api/sessions/"+v.ID, nil, true, nil); resp.StatusCode != 404 {
		t.Fatalf("second delete: %d", resp.StatusCode)
	}
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t, nil)
	for name, body := range map[string]string{
		"unknown project": `{"project":"../../etc"}`,
		"path as project": `{"project":"` + strings.ReplaceAll(e.workdir, `\`, `\\`) + `"}`,
		"bad json":        `{`,
	} {
		resp, _ := call(t, e, "POST", "/api/sessions", []byte(body), true, nil)
		if resp.StatusCode < 400 || e.pty.Count() != 0 {
			t.Errorf("%s: status %d, started %d", name, resp.StatusCode, e.pty.Count())
		}
	}
	// The resume id goes onto a command line: only plain UUIDs pass.
	for _, bad := range []string{"--dangerously-skip-permissions", "abc", "11111111-1111-4111-8111-111111111111 --x"} {
		resp, _ := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj","resume":"`+bad+`"}`), true, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("resume %q: %d", bad, resp.StatusCode)
		}
	}
	// A well-formed id that is not one of this project's conversations is refused.
	const id = "11111111-1111-4111-8111-111111111111"
	resp, _ := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj","resume":"`+id+`"}`), true, nil)
	if resp.StatusCode != http.StatusBadRequest || e.pty.Count() != 0 {
		t.Fatalf("unknown conversation accepted: %d", resp.StatusCode)
	}
	// Once it exists in the project's history, resume works and uses its title.
	dir := filepath.Join(e.srv.o.HistoryRoot, history.EncodeDir(e.workdir))
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(`{"type":"user","message":{"role":"user","content":"Fix the bug"}}`+"\n"), 0o600)
	resp, body := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj","resume":"`+id+`"}`), true, nil)
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(body), "Fix the bug") {
		t.Fatalf("valid resume: %d %s", resp.StatusCode, body)
	}
}

func TestSessionLimit(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 5; i++ {
		if resp, _ := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj"}`), true, nil); resp.StatusCode != 201 {
			t.Fatalf("create %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := call(t, e, "POST", "/api/sessions", []byte(`{"project":"proj"}`), true, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("over limit: %d", resp.StatusCode)
	}
}

func TestWSUnknownSessionRejected(t *testing.T) {
	e := newEnv(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+"/ws?session=nope", &websocket.DialOptions{HTTPHeader: e.goodHeaders()})
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("err %v resp %v", err, resp)
	}
}

func TestSessionsAreIsolatedOverWebSocket(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.mgr.Create("proj", e.workdir, "a", "")
	b, _ := e.mgr.Create("proj", e.workdir, "b", "")
	dial := func(id string) *websocket.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http")+"/ws?session="+id, &websocket.DialOptions{HTTPHeader: e.goodHeaders()})
		if err != nil {
			t.Fatal(err)
		}
		readText(t, c)
		readBinary(t, c)
		return c
	}
	ca, cb := dial(a.ID), dial(b.ID)
	defer ca.CloseNow()
	defer cb.CloseNow()
	ctx := context.Background()
	ca.Write(ctx, websocket.MessageBinary, []byte("to-a"))
	cb.Write(ctx, websocket.MessageBinary, []byte("to-b"))
	time.Sleep(200 * time.Millisecond)
	// fakepty.Factory keeps start order: first PTY belongs to a, second to b.
	for i, want := range []string{"to-a", "to-b"} {
		if got := string(e.pty.Nth(i).Written()); got != want {
			t.Fatalf("session %d received %q, want %q", i, got, want)
		}
	}
}

func TestUpload(t *testing.T) {
	e := newEnv(t, nil)
	id := e.sid(t)
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	resp, body := call(t, e, "POST", "/api/upload?session="+id, png, true, map[string]string{"Content-Type": "image/png"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	var out struct{ Path string }
	json.Unmarshal(body, &out)
	if !strings.HasPrefix(out.Path, UploadDir+"/") || !strings.HasSuffix(out.Path, ".png") {
		t.Fatalf("path %q", out.Path)
	}
	saved, err := os.ReadFile(filepath.Join(e.workdir, filepath.FromSlash(out.Path)))
	if err != nil || !bytes.Equal(saved, png) {
		t.Fatalf("file not stored in the project: %v", err)
	}
	if gi, _ := os.ReadFile(filepath.Join(e.workdir, UploadDir, ".gitignore")); string(gi) != "*\n" {
		t.Fatalf(".gitignore %q", gi)
	}
	// The client's claimed type and any name are ignored: content decides.
	resp, _ = call(t, e, "POST", "/api/upload?session="+id+"&name=../../evil.png", []byte("MZ\x90\x00 pretend exe"), true, map[string]string{"Content-Type": "image/png"})
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("disguised file: %d", resp.StatusCode)
	}
	resp, _ = call(t, e, "POST", "/api/upload?session="+id, []byte("<html><script>alert(1)</script></html>"), true, nil)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("html accepted: %d", resp.StatusCode)
	}
	// Nothing escaped the uploads folder.
	entries, _ := os.ReadDir(e.workdir)
	for _, en := range entries {
		if en.Name() != UploadDir {
			t.Errorf("stray entry in project: %s", en.Name())
		}
	}
}

func TestUploadLimitsAndAuth(t *testing.T) {
	e := newEnv(t, nil)
	id := e.sid(t)
	big := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, maxUpload+10)...)
	if resp, _ := call(t, e, "POST", "/api/upload?session="+id, big, true, nil); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d", resp.StatusCode)
	}
	if resp, _ := call(t, e, "POST", "/api/upload?session="+id, []byte("hello text"), false, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no CSRF header: %d", resp.StatusCode)
	}
	if resp, _ := call(t, e, "POST", "/api/upload?session=missing", []byte("hello"), true, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: %d", resp.StatusCode)
	}
	if resp, _ := call(t, e, "POST", "/api/upload?session="+id, nil, true, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty body: %d", resp.StatusCode)
	}
}

func TestDisplays(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Displays = func() []screen.Display {
			return []screen.Display{{Index: 0, Width: 1920, Height: 1080}, {Index: 1, Width: 1280, Height: 720}}
		}
	})
	_, body := call(t, e, "GET", "/api/displays", nil, false, nil)
	if !strings.Contains(string(body), `"width":1920`) || !strings.Contains(string(body), `"index":1`) {
		t.Fatalf("body %s", body)
	}
}

func TestPushDisabledWithoutService(t *testing.T) {
	e := newEnv(t, nil)
	if resp, _ := call(t, e, "GET", "/api/push/key", nil, false, nil); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestPushSubscribeRejectsInternalEndpoints(t *testing.T) {
	svc, err := push.Load(t.TempDir(), e2eLog())
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(o *Options) { o.Push = svc })
	bad := `{"endpoint":"https://127.0.0.1:9/x","keys":{"auth":"dGVzdA","p256dh":"dGVzdA"}}`
	if resp, _ := call(t, e, "POST", "/api/push/subscribe", []byte(bad), true, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("internal endpoint accepted: %d", resp.StatusCode)
	}
	good := `{"endpoint":"https://fcm.googleapis.com/fcm/send/x","keys":{"auth":"dGVzdA","p256dh":"dGVzdA"}}`
	if resp, _ := call(t, e, "POST", "/api/push/subscribe", []byte(good), true, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("good endpoint: %d", resp.StatusCode)
	}
	if svc.Count() != 1 {
		t.Fatalf("count %d", svc.Count())
	}
	_, key := call(t, e, "GET", "/api/push/key", nil, false, nil)
	if !strings.Contains(string(key), `"key":"`) {
		t.Fatalf("key %s", key)
	}
}

func TestInternalNotifyNeedsSecretAndThrottles(t *testing.T) {
	svc, _ := push.Load(t.TempDir(), e2eLog())
	e := newEnv(t, func(o *Options) { o.Push = svc; o.NotifySecret = "s3cret-s3cret-s3cret" })
	entry, _ := e.mgr.Create("proj", e.workdir, "task title", "")
	internal := httptest.NewServer(e.srv.InternalHandler())
	defer internal.Close()

	post := func(secret, body string) int {
		req, _ := http.NewRequest("POST", internal.URL+"/notify", strings.NewReader(body))
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	body := `{"session":"` + entry.ID + `","event":"notification","message":"needs permission"}`
	if c := post("", body); c != 401 {
		t.Fatalf("no secret: %d", c)
	}
	if c := post("wrong", body); c != 401 {
		t.Fatalf("wrong secret: %d", c)
	}
	if c := post("s3cret-s3cret-s3cret", body); c != 204 {
		t.Fatalf("good secret: %d", c)
	}
	if c := post("s3cret-s3cret-s3cret", `{"session":"unknown","event":"stop"}`); c != 204 {
		t.Fatalf("unknown session: %d", c)
	}
	if !e.srv.notifyThrottled(entry.ID + ":attention") {
		t.Fatal("repeat within 3s was not throttled")
	}
	// The public listener must not serve /notify.
	resp, _ := call(t, e, "POST", "/notify", []byte(body), true, map[string]string{"Authorization": "Bearer s3cret-s3cret-s3cret"})
	if resp.StatusCode == 204 {
		t.Fatal("/notify reachable on the public listener")
	}
}

func e2eLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestLogoutRevokesSession(t *testing.T) {
	e := newEnv(t, nil)
	id, _ := e.srv.o.Auth.NewSession()
	id2, _ := e.srv.o.Auth.NewSession()
	get := func(cookie string) int {
		req, _ := http.NewRequest("GET", e.ts.URL+"/api/projects", nil)
		req.AddCookie(&http.Cookie{Name: "cr_session", Value: cookie})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if get(id) != 200 || get(id2) != 200 {
		t.Fatal("sessions should be valid")
	}
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/logout", nil)
	req.Header.Set("X-CR", "1")
	req.AddCookie(&http.Cookie{Name: "cr_session", Value: id})
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 || get(id) != 401 || get(id2) != 200 {
		t.Fatalf("single logout: %d, after: %d %d", resp.StatusCode, get(id), get(id2))
	}
	req, _ = http.NewRequest("POST", e.ts.URL+"/api/logout?all=1", nil)
	req.Header.Set("X-CR", "1")
	req.AddCookie(&http.Cookie{Name: "cr_session", Value: id2})
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if get(id2) != 401 {
		t.Fatal("logout everywhere did not revoke")
	}
}

func TestUploadFileCountCapped(t *testing.T) {
	e := newEnv(t, nil)
	id := e.sid(t)
	dir := filepath.Join(e.workdir, UploadDir)
	os.MkdirAll(dir, 0o700)
	for i := 0; i < maxUploadFiles; i++ {
		os.WriteFile(filepath.Join(dir, strings.Repeat("a", i+1)+".txt"), []byte("x"), 0o600)
	}
	resp, _ := call(t, e, "POST", "/api/upload?session="+id, []byte("hello text"), true, nil)
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("cap not enforced: %d", resp.StatusCode)
	}
}
