package session

import (
	"bytes"
	"strings"
	"testing"
)

func TestRingOrderNoOverflow(t *testing.T) {
	r := NewRing(10)
	r.Write([]byte("abc"))
	r.Write([]byte("def"))
	if got := string(r.Snapshot()); got != "abcdef" {
		t.Fatalf("got %q", got)
	}
}

func TestRingOverflowKeepsNewest(t *testing.T) {
	r := NewRing(8)
	r.Write([]byte("12345"))
	r.Write([]byte("67890")) // wraps
	if got := string(r.Snapshot()); got != "34567890" {
		t.Fatalf("got %q", got)
	}
	r.Write([]byte(strings.Repeat("x", 20))) // larger than the whole ring
	if got := string(r.Snapshot()); got != "xxxxxxxx" {
		t.Fatalf("got %q", got)
	}
	r.Write([]byte("AB"))
	if got := string(r.Snapshot()); got != "xxxxxxAB" {
		t.Fatalf("got %q", got)
	}
}

func TestReplayTrimsToLineBoundaryAfterOverflow(t *testing.T) {
	r := NewRing(16)
	// The ring will start in the middle of the colour sequence.
	r.Write([]byte("\x1b[31mred\x1b[0m\nline2\nline3\n"))
	got := r.Replay()
	if !bytes.HasPrefix(got, []byte(resetSeq)) {
		t.Fatal("replay must start with a terminal reset")
	}
	body := string(got[len(resetSeq):])
	if body != "line2\nline3\n" && body != "line3\n" {
		t.Fatalf("replay does not start on a line boundary: %q", body)
	}
}

func TestReplayNotTrimmedWithoutOverflow(t *testing.T) {
	r := NewRing(64)
	r.Write([]byte("\x1b[31mred\x1b[0m"))
	if got := string(r.Replay()); got != resetSeq+"\x1b[31mred\x1b[0m" {
		t.Fatalf("got %q", got)
	}
}

func TestReplayNoNewlineAfterOverflowIsEmpty(t *testing.T) {
	r := NewRing(4)
	r.Write([]byte("abcdefgh"))
	if got := string(r.Replay()); got != resetSeq {
		t.Fatalf("got %q", got)
	}
}
