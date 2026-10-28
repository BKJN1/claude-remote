//go:build windows

package config

import (
	"path/filepath"
	"testing"
)

func TestFilePermissionsRestricted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cr")
	if _, _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if err := verifyPrivate(filepath.Join(dir, fileName)); err != nil {
		t.Fatalf("config.json not private: %v", err)
	}
}
