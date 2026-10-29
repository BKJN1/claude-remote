package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	id1 = "11111111-1111-4111-8111-111111111111"
	id2 = "22222222-2222-4222-8222-222222222222"
	id3 = "33333333-3333-4333-8333-333333333333"
)

func write(t *testing.T, dir, id, content string, mod time.Time) {
	t.Helper()
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeDir(t *testing.T) {
	got := EncodeDir(`C:\Users\RBk\Desktop\BekeJan\IT_PROJECTS\claude-remote`)
	want := "C--Users-RBk-Desktop-BekeJan-IT-PROJECTS-claude-remote"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestValidID(t *testing.T) {
	if !ValidID(id1) {
		t.Fatal("valid id rejected")
	}
	for _, bad := range []string{"", "x", id1 + " --dangerously-skip-permissions", "../" + id1, id1 + "\n"} {
		if ValidID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestListNewestFirstWithTitles(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(t.TempDir(), "my proj")
	os.MkdirAll(work, 0o700)
	abs, _ := filepath.Abs(work)
	dir := filepath.Join(root, EncodeDir(abs))
	os.MkdirAll(dir, 0o700)
	now := time.Now()

	write(t, dir, id1, `{"type":"mode"}
{"type":"user","message":{"role":"user","content":"Fix the login   bug\nplease"}}
`, now.Add(-2*time.Hour))
	write(t, dir, id2, `{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Second task"}]}}
`, now.Add(-1*time.Hour))
	// Never started: no user message => omitted. Slash-command echo is skipped.
	write(t, dir, id3, `{"type":"mode"}
{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}
`, now)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)

	items, err := List(root, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != id2 || items[1].ID != id1 {
		t.Fatalf("items %+v", items)
	}
	if items[0].Title != "Second task" || items[1].Title != "Fix the login bug please" {
		t.Fatalf("titles %q %q", items[0].Title, items[1].Title)
	}
}

func TestListMissingProjectIsEmpty(t *testing.T) {
	items, err := List(t.TempDir(), t.TempDir())
	if err != nil || len(items) != 0 {
		t.Fatalf("%v %v", items, err)
	}
	items, err = List(filepath.Join(t.TempDir(), "nope"), t.TempDir())
	if err != nil || len(items) != 0 {
		t.Fatalf("%v %v", items, err)
	}
}

func TestLongTitleTruncatedAndGarbageSkipped(t *testing.T) {
	root := t.TempDir()
	work := t.TempDir()
	abs, _ := filepath.Abs(work)
	dir := filepath.Join(root, strings.ToUpper(EncodeDir(abs))) // case differs
	os.MkdirAll(dir, 0o700)
	write(t, dir, id1, "not json\n{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\""+strings.Repeat("я", 300)+"\"}}\n", time.Now())
	items, _ := List(root, work)
	if len(items) != 1 {
		t.Fatalf("items %+v", items)
	}
	if n := len([]rune(items[0].Title)); n != titleRunes+1 {
		t.Fatalf("title runes %d", n)
	}
}

func TestListAllAcrossFoldersAndFind(t *testing.T) {
	root := t.TempDir()
	mk := func(dir, id, content string, mod time.Time) {
		d := filepath.Join(root, dir)
		os.MkdirAll(d, 0o700)
		write(t, d, id, content, mod)
	}
	now := time.Now()
	mk("C--proj-a", id1, `{"type":"user","cwd":"C:\\proj\\a","message":{"role":"user","content":"Task in A"}}`+"\n", now.Add(-3*time.Hour))
	mk("C--proj-b", id2, `{"type":"mode"}`+"\n"+`{"type":"user","cwd":"C:\\proj\\b","message":{"role":"user","content":"Task in B"}}`+"\n", now.Add(-1*time.Hour))
	// No user message => skipped; no cwd => skipped (cannot be resumed from here).
	mk("C--proj-c", id3, `{"type":"mode"}`+"\n", now)
	mk("C--proj-d", "44444444-4444-4444-8444-444444444444", `{"type":"user","message":{"role":"user","content":"no cwd"}}`+"\n", now)
	os.WriteFile(filepath.Join(root, "stray.jsonl"), []byte("x"), 0o600)

	got := ListAll(root, 10)
	if len(got) != 2 || got[0].ID != id2 || got[1].ID != id1 {
		t.Fatalf("%+v", got)
	}
	if got[0].CWD != `C:\proj\b` || got[0].Title != "Task in B" {
		t.Fatalf("%+v", got[0])
	}
	if limited := ListAll(root, 1); len(limited) != 1 || limited[0].ID != id2 {
		t.Fatalf("limit not applied: %+v", limited)
	}

	r, ok := Find(root, id1)
	if !ok || r.CWD != `C:\proj\a` || r.Title != "Task in A" {
		t.Fatalf("%+v %v", r, ok)
	}
	for _, bad := range []string{"", "../" + id1, id1 + "/../x", "not-an-id", id3} {
		if _, ok := Find(root, bad); ok {
			t.Errorf("Find(%q) succeeded", bad)
		}
	}
}

func TestClaudeGeneratedTitleIsPreferred(t *testing.T) {
	root := t.TempDir()
	work := t.TempDir()
	abs, _ := filepath.Abs(work)
	dir := filepath.Join(root, EncodeDir(abs))
	os.MkdirAll(dir, 0o700)
	now := time.Now()
	// ai-title written after the first prompt: it wins over the prompt text.
	write(t, dir, id1, `{"type":"user","cwd":"C:\\x","message":{"role":"user","content":"привет, давай сделаем всякое разное в проекте"}}
{"type":"assistant","message":{"role":"assistant","content":"ok"}}
{"type":"ai-title","aiTitle":"Удалённое управление Claude Code","sessionId":"s"}
`, now)
	// No ai-title (older conversations): falls back to the first prompt.
	write(t, dir, id2, `{"type":"user","cwd":"C:\\x","message":{"role":"user","content":"Plain prompt"}}
`, now.Add(-time.Hour))

	items, err := List(root, work)
	if err != nil || len(items) != 2 {
		t.Fatalf("%v %+v", err, items)
	}
	if items[0].Title != "Удалённое управление Claude Code" || items[1].Title != "Plain prompt" {
		t.Fatalf("titles %q / %q", items[0].Title, items[1].Title)
	}
	if r, ok := Find(root, id1); !ok || r.Title != "Удалённое управление Claude Code" || r.CWD != `C:\x` {
		t.Fatalf("Find: %+v %v", r, ok)
	}
	// An empty or whitespace ai-title is ignored.
	write(t, dir, id3, `{"type":"ai-title","aiTitle":"   "}
{"type":"user","cwd":"C:\\x","message":{"role":"user","content":"Fallback prompt"}}
`, now.Add(-2*time.Hour))
	if r, ok := Find(root, id3); !ok || r.Title != "Fallback prompt" {
		t.Fatalf("blank ai-title: %+v %v", r, ok)
	}
}
