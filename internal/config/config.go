// Package config loads and stores the server's single secret: the access token.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const fileName = "config.json"

// Config is the on-disk configuration. Command and workdir are deliberately
// NOT stored here: they come from launch flags only.
type Config struct {
	Token string `json:"token"`
}

// DefaultDir returns %APPDATA%\claude-remote (or the platform equivalent).
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "claude-remote"), nil
}

// GenerateToken returns 32 random bytes encoded as base64url (43 chars).
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Load reads the config from dir. If no config exists yet, a new token is
// generated and stored; created is then true so the caller can show the token
// exactly once. An existing file with loose permissions is an error.
func Load(dir string) (cfg *Config, created bool, err error) {
	path := filepath.Join(dir, fileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		cfg, err = Rotate(dir)
		return cfg, err == nil, err
	}
	if err != nil {
		return nil, false, err
	}
	if err := verifyPrivate(path); err != nil {
		return nil, false, fmt.Errorf("config file %s is not private: %w", path, err)
	}
	cfg = &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(cfg.Token) < 43 {
		return nil, false, fmt.Errorf("token in %s is too short; run `claude-remote token rotate`", path)
	}
	return cfg, false, nil
}

// Rotate generates a fresh token and atomically replaces the config file.
func Rotate(dir string) (*Config, error) {
	tok, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	cfg := &Config{Token: tok}
	if err := write(dir, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func write(dir string, cfg *Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := restrict(dir); err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	// Write to a temp file whose permissions are restricted BEFORE the token
	// is written, then rename: the secret is never readable by others.
	tmp, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := restrict(tmpName); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	path := filepath.Join(dir, fileName)
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return verifyPrivate(path)
}
