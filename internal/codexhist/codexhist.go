// Package codexhist lists Codex CLI conversations stored on disk so the UI can
// offer "resume" for them like it does for Claude Code.
//
// Codex keeps one rollout file per conversation under
// ~/.codex/sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl (older ones are
// compressed to .jsonl.zst) and a names index in session_index.jsonl. This is
// Codex's internal format, not a public API: anything unexpected is skipped
// rather than treated as an error.
package codexhist

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
)

// Recent is one Codex conversation.
type Recent struct {
	ID       string
	Title    string
	Modified time.Time
	CWD      string // the folder the conversation was started in
}

var idRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidID reports whether s looks like a Codex session id (a UUID). It is
// checked before an id is ever placed on a command line.
func ValidID(s string) bool { return idRe.MatchString(s) }

// DefaultRoot is $CODEX_HOME, or ~/.codex.
func DefaultRoot() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

const (
	maxScan     = 600     // rollout files examined per listing
	maxLine     = 4 << 20 // the first line carries long instructions
	maxTitleLns = 400     // lines scanned for a first user message
	titleRunes  = 90
	maxIndex    = 20000    // index lines read
	zstdMemory  = 64 << 20 // decoder memory cap
)

type fileRef struct {
	path string
	id   string
	mod  time.Time
}

// rolloutID extracts the id from "rollout-<time>-<id>.jsonl[.zst]".
func rolloutID(name string) (string, bool) {
	if !strings.HasPrefix(name, "rollout-") {
		return "", false
	}
	base := strings.TrimSuffix(strings.TrimSuffix(name, ".zst"), ".jsonl")
	if base == name || len(base) < 36 {
		return "", false
	}
	id := base[len(base)-36:]
	if !ValidID(id) {
		return "", false
	}
	return id, true
}

func listFiles(root string) []fileRef {
	var out []fileRef
	filepath.WalkDir(filepath.Join(root, "sessions"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		id, ok := rolloutID(d.Name())
		if !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 {
			return nil
		}
		out = append(out, fileRef{p, id, info.ModTime()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
	return out
}

// names reads session_index.jsonl: id -> thread name.
func names(root string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(filepath.Join(root, "session_index.jsonl"))
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 0; sc.Scan() && n < maxIndex; n++ {
		var e struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && ValidID(e.ID) && strings.TrimSpace(e.Name) != "" {
			m[e.ID] = clean(e.Name)
		}
	}
	return m
}

// open returns a reader for a rollout file, decompressing .zst on the fly.
func open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".zst") {
		return f, nil
	}
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(zstdMemory))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &zstReader{dec: dec, f: f}, nil
}

type zstReader struct {
	dec *zstd.Decoder
	f   *os.File
}

func (z *zstReader) Read(p []byte) (int, error) { return z.dec.Read(p) }
func (z *zstReader) Close() error               { z.dec.Close(); return z.f.Close() }

type metaLine struct {
	Type    string `json:"type"`
	Payload struct {
		CWD  string `json:"cwd"`
		Role string `json:"role"`
		Text []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"payload"`
}

// scan reads the folder (from session_meta) and, if wantTitle, the first real
// user message of a rollout.
func scan(path string, wantTitle bool) (cwd, title string) {
	rc, err := open(path)
	if err != nil {
		return "", ""
	}
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 256<<10)
	for n := 0; n < maxTitleLns; n++ {
		line, err := readLine(br)
		if len(line) > 0 {
			var m metaLine
			if json.Unmarshal(line, &m) == nil {
				if m.Type == "session_meta" && cwd == "" {
					cwd = normalizeCWD(m.Payload.CWD)
					if !wantTitle {
						return cwd, ""
					}
				}
				if wantTitle && m.Type == "response_item" && m.Payload.Role == "user" {
					for _, c := range m.Payload.Text {
						if t := clean(c.Text); t != "" {
							return cwd, t
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return cwd, ""
}

// readLine reads one line, capped at maxLine (longer lines are skipped).
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(buf)+len(chunk) <= maxLine {
			buf = append(buf, chunk...)
		}
		if err != nil || !isPrefix {
			return buf, err
		}
	}
}

// normalizeCWD strips the Windows verbatim prefix (\\?\C:\x -> C:\x).
func normalizeCWD(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, `\\?\`) && len(p) > 6 && p[5] == ':' {
		return p[4:]
	}
	return p
}

// clean collapses whitespace and skips Codex's own injected context blocks
// (they start with '<', e.g. <environment_context>).
func clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || strings.HasPrefix(s, "<") {
		return ""
	}
	if utf8.RuneCountInString(s) > titleRunes {
		s = string([]rune(s)[:titleRunes]) + "…"
	}
	return s
}

func sameDir(a, b string) bool {
	return strings.EqualFold(filepath.Clean(normalizeCWD(a)), filepath.Clean(normalizeCWD(b)))
}

func build(root string, f fileRef, idx map[string]string) (Recent, bool) {
	cwd, title := scan(f.path, idx[f.id] == "")
	if cwd == "" {
		return Recent{}, false
	}
	if t := idx[f.id]; t != "" {
		title = t
	}
	if title == "" {
		title = "Разговор Codex"
	}
	return Recent{ID: f.id, Title: title, Modified: f.mod, CWD: cwd}, true
}

// ListAll returns the most recent conversations from every folder, newest first.
func ListAll(root string, limit int) []Recent {
	idx := names(root)
	var out []Recent
	for i, f := range listFiles(root) {
		if len(out) >= limit || i >= maxScan {
			break
		}
		if r, ok := build(root, f, idx); ok {
			out = append(out, r)
		}
	}
	return out
}

// ListForDir returns the conversations that were started in dir, newest first.
func ListForDir(root, dir string) []Recent {
	idx := names(root)
	var out []Recent
	for i, f := range listFiles(root) {
		if len(out) >= 30 || i >= maxScan {
			break
		}
		if r, ok := build(root, f, idx); ok && sameDir(r.CWD, dir) {
			out = append(out, r)
		}
	}
	return out
}

// Find locates one conversation by id. The id is a validated UUID, so it can
// only select a file by name; it is never used as a path.
func Find(root, id string) (Recent, bool) {
	if !ValidID(id) {
		return Recent{}, false
	}
	idx := names(root)
	for _, f := range listFiles(root) {
		if strings.EqualFold(f.id, id) {
			return build(root, f, idx)
		}
	}
	return Recent{}, false
}
