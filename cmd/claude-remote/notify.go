package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claude-remote/internal/config"
)

const (
	envNotifyURL    = "CLAUDE_REMOTE_NOTIFY_URL"
	envNotifySecret = "CLAUDE_REMOTE_NOTIFY_SECRET"
	envSession      = "CLAUDE_REMOTE_SESSION"
	hooksFile       = "claude-hooks.json"
)

// writeHooks writes the settings file passed to Claude Code with --settings.
// Its Notification and Stop hooks run `claude-remote notify`, which tells the
// server (on the loopback listener) that Claude needs attention or is done.
// That is Claude's own supported signal, so no terminal text is scraped.
func writeHooks(dir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Forward slashes work in both cmd.exe and bash, which Claude may use to run hooks.
	cmd := `"` + filepath.ToSlash(exe) + `" notify`
	hook := []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}}}
	data, err := json.MarshalIndent(map[string]any{
		// UserPromptSubmit marks "working", Notification "needs you", Stop "done":
		// together they drive the session pulse and the push notifications.
		"hooks": map[string]any{"UserPromptSubmit": hook, "Notification": hook, "Stop": hook},
	}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := config.WritePrivateFile(dir, hooksFile, data); err != nil {
		return "", err
	}
	return filepath.Join(dir, hooksFile), nil
}

// notifyCmd is the hook helper. It must never block or fail Claude Code, so
// every problem ends in a silent exit 0.
func notifyCmd() int {
	url, secret, sess := os.Getenv(envNotifyURL), os.Getenv(envNotifySecret), os.Getenv(envSession)
	if url == "" || secret == "" || sess == "" {
		return 0
	}
	var in struct {
		Event   string `json:"hook_event_name"`
		Message string `json:"message"`
	}
	data, _ := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	json.Unmarshal(data, &in)
	var event string
	switch strings.ToLower(in.Event) {
	case "stop":
		event = "stop"
	case "notification":
		event = "notification"
	case "userpromptsubmit":
		event = "working"
	default:
		return 0
	}
	body, _ := json.Marshal(map[string]string{"session": sess, "event": event, "message": in.Message})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return 0
}
