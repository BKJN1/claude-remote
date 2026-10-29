package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"sync"
	"time"
)

// ErrTooManySessions is returned when the configured limit is reached.
var ErrTooManySessions = errors.New("too many sessions")

// Meta describes one terminal session.
type Meta struct {
	ID      string
	Project string // project name chosen from the operator's list
	Title   string
	Resume  string // Claude conversation id being resumed; "" for a new one
	Attach  string // name of a background session to attach to (claude attach)
	Tool    string // "claude" (default) or "codex"
	Created time.Time
	Workdir string // server-side only, never sent to clients
}

// Entry is a session together with its metadata.
type Entry struct {
	Meta
	S   *Session
	seq int // creation order; timestamps can tie on Windows
}

// Manager owns several independent sessions.
type Manager struct {
	factory  func(Meta) StartFunc
	ringSize int
	max      int

	mu      sync.Mutex
	entries map[string]*Entry
	seq     int
	rec     func(Meta) io.Writer
}

// NewManager builds a manager. factory returns the launcher for a session;
// it is called with server-chosen metadata only.
func NewManager(factory func(Meta) StartFunc, ringSize, max int) *Manager {
	return &Manager{factory: factory, ringSize: ringSize, max: max, entries: make(map[string]*Entry)}
}

// SetRecorderFactory enables full-output recording (--record). The writer is
// closed with the session when it implements io.Closer.
func (m *Manager) SetRecorderFactory(f func(Meta) io.Writer) {
	m.mu.Lock()
	m.rec = f
	m.mu.Unlock()
}

// Create registers a session and starts its process right away. A launch
// failure is not returned: it is recorded in the session (Info().ErrKind) so
// the UI can show it next to the terminal.
func (m *Manager) Create(project, workdir, title, resume string) (*Entry, error) {
	return m.CreateTool("claude", project, workdir, title, resume)
}

// CreateTool is Create for a specific tool ("claude" or "codex").
func (m *Manager) CreateTool(tool, project, workdir, title, resume string) (*Entry, error) {
	if tool == "" {
		tool = "claude"
	}
	return m.create(Meta{Tool: tool, Project: project, Title: title, Resume: resume, Workdir: workdir})
}

// CreateAttach opens a terminal running `claude attach <name>` for a
// background session.
func (m *Manager) CreateAttach(project, workdir, title, attach string) (*Entry, error) {
	return m.create(Meta{Project: project, Title: title, Attach: attach, Workdir: workdir})
}

func (m *Manager) create(meta Meta) (*Entry, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	meta.ID = hex.EncodeToString(b)
	meta.Created = time.Now()
	m.mu.Lock()
	var evicted *Entry
	if len(m.entries) >= m.max {
		// Make room by dropping the oldest session whose process already
		// ended; running ones are never closed behind the user's back.
		for _, e := range m.orderedLocked() {
			if e.S.Info().State == Exited {
				evicted = e
				delete(m.entries, e.ID)
				break
			}
		}
		if evicted == nil {
			m.mu.Unlock()
			return nil, ErrTooManySessions
		}
	}
	s := New(m.factory(meta), m.ringSize, 80, 24)
	if m.rec != nil {
		if w := m.rec(meta); w != nil {
			s.SetRecorder(w)
		}
	}
	m.seq++
	if evicted != nil {
		defer evicted.S.Close() // after the lock is released
	}
	e := &Entry{Meta: meta, S: s, seq: m.seq}
	m.entries[meta.ID] = e
	m.mu.Unlock()
	s.Ensure() // failure is visible through s.Info()
	return e, nil
}

func (m *Manager) Get(id string) (*Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	return e, ok
}

// HasRoom reports whether Create would succeed right now (a free slot, or an
// exited session that can be evicted). Checked before anything irreversible,
// such as closing a window on the laptop.
func (m *Manager) HasRoom() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) < m.max {
		return true
	}
	for _, e := range m.entries {
		if e.S.Info().State == Exited {
			return true
		}
	}
	return false
}

// FindLaptop returns a live session already opened for the same Claude
// conversation (resume id) or background session (attach name).
func (m *Manager) FindLaptop(resume, attach string) (*Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.S.Info().State == Exited {
			continue
		}
		if (resume != "" && e.Resume == resume) || (attach != "" && e.Attach == attach) {
			return e, true
		}
	}
	return nil, false
}

// List returns sessions oldest first.
func (m *Manager) List() []*Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.orderedLocked()
}

func (m *Manager) orderedLocked() []*Entry {
	out := make([]*Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// Close stops one session and its whole process tree.
func (m *Manager) Close(id string) bool {
	m.mu.Lock()
	e, ok := m.entries[id]
	delete(m.entries, id)
	m.mu.Unlock()
	if ok {
		e.S.Close()
	}
	return ok
}

// CloseAll stops every session (server shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := m.entries
	m.entries = make(map[string]*Entry)
	m.mu.Unlock()
	for _, e := range all {
		e.S.Close()
	}
}
