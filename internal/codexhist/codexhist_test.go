package codexhist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	id1 = "019ed1cd-5c58-71a1-bf33-fb580807e9f5"
	id2 = "019dc3f8-9a8f-7291-b39a-183615d32b1a"
	id3 = "01a18422-c7ea-7de0-aa54-2241643b6c1b"
	id4 = "019bfa86-1823-7730-85df-0ed98f32ca17"
)

func meta(cwd string) string {
	return `{"type":"session_meta","payload":{"id":"x","cwd":` + quote(cwd) + `,"base_instructions":"` + strings.Repeat("long ", 50) + `"}}` + "\n"
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

func user(text string) string {
	return `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":` + quote(text) + `}]}}` + "\n"
}

func writeRollout(t *testing.T, root, day, id, content string, zst bool, mod time.Time) {
	t.Helper()
	dir := filepath.Join(root, "sessions", "2026", "10", day)
	os.MkdirAll(dir, 0o700)
	name := "rollout-2026-10-" + day + "T10-00-00-" + id + ".jsonl"
	data := []byte(content)
	if zst {
		name += ".zst"
		enc, _ := zstd.NewWriter(nil)
		data = enc.EncodeAll(data, nil)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mod, mod)
}

func fixture(t *testing.T) string {
	root := t.TempDir()
	now := time.Now()
	// plain, with a name in the index
	writeRollout(t, root, "01", id1, meta(`C:\work\alpha`)+user("<environment_context>x</environment_context>")+user("Real prompt in alpha"), false, now.Add(-1*time.Hour))
	// compressed (older conversations are stored this way), verbatim Windows prefix
	writeRollout(t, root, "02", id2, meta(`\\?\C:\work\beta`)+user("Beta task"), true, now.Add(-2*time.Hour))
	// same folder as alpha, differently cased
	writeRollout(t, root, "03", id3, meta(`c:\WORK\Alpha`)+user("Second alpha task"), false, now.Add(-30*time.Minute))
	// no session_meta => skipped; empty file => skipped
	writeRollout(t, root, "04", id4, user("orphan"), false, now)
	os.WriteFile(filepath.Join(root, "sessions", "2026", "10", "04", "rollout-2026-10-04T10-00-00-019bfa86-1823-7730-85df-0ed98f32ca18.jsonl"), nil, 0o600)
	os.WriteFile(filepath.Join(root, "session_index.jsonl"), []byte(
		`{"id":"`+id1+`","thread_name":"Named alpha thread","updated_at":"2026-10-01T00:00:00Z"}`+"\n"+
			`not json`+"\n"), 0o600)
	return root
}

func TestListAllReadsPlainAndCompressedAndSkipsBroken(t *testing.T) {
	root := fixture(t)
	got := ListAll(root, 10)
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	// newest first: id3 (30 min), id1 (1 h), id2 (2 h)
	if got[0].ID != id3 || got[1].ID != id1 || got[2].ID != id2 {
		t.Fatalf("order %v %v %v", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[1].Title != "Named alpha thread" {
		t.Fatalf("index name not used: %q", got[1].Title)
	}
	if got[0].Title != "Second alpha task" {
		t.Fatalf("title from first user message: %q", got[0].Title)
	}
	if got[2].CWD != `C:\work\beta` || got[2].Title != "Beta task" {
		t.Fatalf("compressed rollout / verbatim prefix: %+v", got[2])
	}
	if limited := ListAll(root, 1); len(limited) != 1 || limited[0].ID != id3 {
		t.Fatalf("limit: %+v", limited)
	}
}

func TestListForDirMatchesFolderCaseInsensitively(t *testing.T) {
	root := fixture(t)
	got := ListForDir(root, `C:\work\alpha`)
	if len(got) != 2 || got[0].ID != id3 || got[1].ID != id1 {
		t.Fatalf("%+v", got)
	}
	if other := ListForDir(root, `C:\work\gamma`); len(other) != 0 {
		t.Fatalf("%+v", other)
	}
}

func TestFindAndIDValidation(t *testing.T) {
	root := fixture(t)
	r, ok := Find(root, id2)
	if !ok || r.CWD != `C:\work\beta` {
		t.Fatalf("%+v %v", r, ok)
	}
	for _, bad := range []string{"", "../" + id1, id1 + "/..", "--last", "not-an-id", id4} {
		if _, ok := Find(root, bad); ok {
			t.Errorf("Find(%q) succeeded", bad)
		}
	}
	for _, bad := range []string{"--last", "abc", id1 + " --yolo", id1 + "\n"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q)", bad)
		}
	}
}

func TestMissingRootIsEmpty(t *testing.T) {
	if got := ListAll(filepath.Join(t.TempDir(), "nope"), 5); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestRolloutID(t *testing.T) {
	for name, want := range map[string]bool{
		"rollout-2026-06-16T23-59-22-" + id1 + ".jsonl":     true,
		"rollout-2026-06-16T23-59-22-" + id1 + ".jsonl.zst": true,
		"rollout-2026-06-16T23-59-22-" + id1 + ".txt":       false,
		"other-" + id1 + ".jsonl":                           false,
		"rollout-short.jsonl":                               false,
	} {
		if _, ok := rolloutID(name); ok != want {
			t.Errorf("%s: got %v", name, ok)
		}
	}
}
