package record

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritesAndRotates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rec")
	w, err := Open(dir, "my proj/../x", "abc123", 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := w.Write([]byte(strings.Repeat("x", 40))); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected log + one backup, got %d files", len(entries))
	}
	var total int64
	for _, e := range entries {
		if strings.Contains(e.Name(), "..") || strings.ContainsAny(e.Name(), `/\`) {
			t.Fatalf("unsafe file name %q", e.Name())
		}
		fi, _ := e.Info()
		if fi.Size() > 100 {
			t.Fatalf("%s exceeds the limit: %d", e.Name(), fi.Size())
		}
		total += fi.Size()
	}
	if total == 0 {
		t.Fatal("nothing recorded")
	}
	if _, err := w.Write([]byte("late")); err == nil {
		t.Fatal("write after close succeeded")
	}
}
