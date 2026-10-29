// Package history lists Claude Code conversations stored on disk so the UI
// can offer "resume" like `claude --resume`.
//
// Claude Code keeps one JSONL file per conversation under
// ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl. This is Claude's
// internal format, not a public API: anything unexpected is skipped rather
// than treated as an error, and a format change only degrades the list.
package history

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Item is one resumable conversation.
type Item struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Modified time.Time `json:"modified"`
}

var idRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidID reports whether s looks like a Claude session id. It is checked
// before an id is ever placed on a command line.
func ValidID(s string) bool { return idRe.MatchString(s) }

// EncodeDir mimics how Claude Code names a project's folder: every character
// that is not a letter or digit becomes '-'.
func EncodeDir(path string) string {
	var b strings.Builder
	for _, r := range path {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// DefaultRoot is ~/.claude/projects.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

const (
	aiTitleScanBytes = 256 << 10
	maxItems         = 30
	headBytes        = 512 << 10 // how much of a file is scanned for the title
	titleRunes       = 90
	maxLineBytes     = 1 << 20
)

// List returns the most recent conversations for workdir, newest first.
// Conversations without any user message (never really started) are omitted.
func List(root, workdir string) ([]Item, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	dir, err := findProjectDir(root, abs)
	if err != nil || dir == "" {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id  string
		mod time.Time
	}
	var cands []cand
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if !ValidID(id) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, cand{id, info.ModTime()})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	var out []Item
	for _, c := range cands {
		if len(out) >= maxItems {
			break
		}
		title := firstUserMessage(filepath.Join(dir, c.id+".jsonl"))
		if title == "" {
			continue
		}
		out = append(out, Item{ID: c.id, Title: title, Modified: c.mod})
	}
	return out, nil
}

// findProjectDir locates the folder for abs. Windows paths are case
// insensitive, and the drive letter case Claude used may differ from ours, so
// compare names case-insensitively.
func findProjectDir(root, abs string) (string, error) {
	want := EncodeDir(abs)
	if d := filepath.Join(root, want); isDir(d) {
		return d, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() && strings.EqualFold(e.Name(), want) {
			return filepath.Join(root, e.Name()), nil
		}
	}
	return "", nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

type line struct {
	AITitle string `json:"aiTitle"` // Claude's own conversation title (type "ai-title")
	CWD     string `json:"cwd"`
	Type    string `json:"type"`
	IsMeta  bool   `json:"isMeta"`
	Side    bool   `json:"isSidechain"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// firstUserMessage returns a one-line title from the first real user prompt.
func firstUserMessage(path string) string {
	title, _ := scan(path)
	return title
}

// scan reads the head of a conversation file and returns its title and the
// working directory Claude recorded for it.
//
// The title is the one Claude itself generated (an "ai-title" entry, the text
// `claude --resume` lists). Only when a file has none is the first real user
// prompt used.
func scan(path string) (title, cwd string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	var aiTitle, userTitle string
	read := 0
	for sc.Scan() {
		read += len(sc.Bytes())
		if read > headBytes {
			break
		}
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if cwd == "" && l.CWD != "" {
			cwd = l.CWD
		}
		if l.Type == "ai-title" && aiTitle == "" {
			aiTitle = cleanTitle(l.AITitle)
		}
		if userTitle == "" && l.Type == "user" && !l.IsMeta && !l.Side {
			userTitle = textOf(l.Message.Content)
		}
		if aiTitle != "" && cwd != "" {
			return aiTitle, cwd
		}
		// The generated title is written early; do not read a huge file to the
		// end looking for one that is not there.
		if aiTitle == "" && userTitle != "" && cwd != "" && read > aiTitleScanBytes {
			break
		}
	}
	if aiTitle != "" {
		return aiTitle, cwd
	}
	return userTitle, cwd
}

func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return cleanTitle(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		for _, p := range parts {
			if p.Type == "text" {
				if t := cleanTitle(p.Text); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// cleanTitle collapses whitespace and skips Claude's own system-ish messages
// (slash-command echoes, caveats), which start with '<' or "Caveat".
func cleanTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || strings.HasPrefix(s, "<") || strings.HasPrefix(s, "Caveat:") {
		return ""
	}
	if utf8.RuneCountInString(s) > titleRunes {
		r := []rune(s)
		s = string(r[:titleRunes]) + "…"
	}
	return s
}

// Recent is a conversation from any folder on this machine.
type Recent struct {
	ID       string
	Title    string
	Modified time.Time
	CWD      string // the folder the conversation was started in
}

type fileRef struct {
	path string
	id   string
	mod  time.Time
}

// listFiles returns every conversation file under root, newest first.
func listFiles(root string) []fileRef {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []fileRef
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if !ValidID(id) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			out = append(out, fileRef{filepath.Join(root, d.Name(), name), id, info.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
	return out
}

// maxScan bounds how many files are opened for one listing.
const maxScan = 400

// ListAll returns the most recent conversations from every project folder,
// newest first. Conversations that never got a user message, or whose folder
// Claude did not record, are omitted (they cannot be resumed from here).
func ListAll(root string, limit int) []Recent {
	var out []Recent
	for i, f := range listFiles(root) {
		if len(out) >= limit || i >= maxScan {
			break
		}
		title, cwd := scan(f.path)
		if title == "" || cwd == "" {
			continue
		}
		out = append(out, Recent{ID: f.id, Title: title, Modified: f.mod, CWD: cwd})
	}
	return out
}

// Find locates one conversation by id in any project folder. The id is a
// validated UUID, so it can only name a file directly inside a project folder.
func Find(root, id string) (Recent, bool) {
	if !ValidID(id) {
		return Recent{}, false
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return Recent{}, false
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		p := filepath.Join(root, d.Name(), id+".jsonl")
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		title, cwd := scan(p)
		if cwd == "" {
			return Recent{}, false
		}
		if title == "" {
			title = "Разговор без названия"
		}
		return Recent{ID: id, Title: title, Modified: info.ModTime(), CWD: cwd}, true
	}
	return Recent{}, false
}
