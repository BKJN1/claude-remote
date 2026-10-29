// Package record writes the raw terminal output of a session to disk
// (--record). The file contains everything that was printed, including any
// secrets shown on screen, so it is private to the current user and rotated
// by size.
package record

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"claude-remote/internal/config"
)

// DefaultMaxBytes is the size at which the log rotates (one ".1" backup is kept).
const DefaultMaxBytes = 20 << 20

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

type Writer struct {
	mu   sync.Mutex
	dir  string
	name string
	max  int64
	f    *os.File
	size int64
}

// Open creates a recording for a session: dir/<time>-<project>-<id>.log.
func Open(dir, project, id string, max int64) (*Writer, error) {
	name := fmt.Sprintf("%s-%s-%s.log", time.Now().Format("20060102-150405"), unsafeName.ReplaceAllString(project, "_"), unsafeName.ReplaceAllString(id, "_"))
	w := &Writer{dir: dir, name: name, max: max}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := config.OpenPrivateAppend(w.dir, w.name)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, fi.Size()
	return nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size+int64(len(p)) > w.max {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate keeps a single backup, so disk use is bounded by 2*max per session.
func (w *Writer) rotate() error {
	w.f.Close()
	w.f = nil
	path := filepath.Join(w.dir, w.name)
	if err := os.Rename(path, path+".1"); err != nil {
		return err
	}
	return w.open()
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
