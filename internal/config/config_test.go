package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenGeneratedOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cr")
	c1, created, err := Load(dir)
	if err != nil || !created {
		t.Fatalf("first load: created=%v err=%v", created, err)
	}
	if len(c1.Token) < 43 { // 32 bytes base64url
		t.Fatalf("token too short: %d", len(c1.Token))
	}
	c2, created, err := Load(dir)
	if err != nil || created {
		t.Fatalf("second load: created=%v err=%v", created, err)
	}
	if c1.Token != c2.Token {
		t.Fatal("token changed between loads")
	}
}

func TestRotateChangesToken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cr")
	c1, _, _ := Load(dir)
	c2, err := Rotate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c1.Token == c2.Token {
		t.Fatal("rotate kept the same token")
	}
	c3, _, _ := Load(dir)
	if c3.Token != c2.Token {
		t.Fatal("rotated token not persisted")
	}
}

func TestShortTokenRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cr")
	if _, _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(`{"token":"abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir); err == nil {
		t.Fatal("expected error for short token")
	}
}
