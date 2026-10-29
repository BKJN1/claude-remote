package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claude-remote/internal/codexhist"
)

// codexArgs builds the extra arguments for a Codex terminal.
//
//   - A new conversation:      codex -c notify=[...]
//   - Continuing one:           codex resume -c notify=[...] <id>
//
// notify is Codex's own "a turn finished" hook. It REPLACES any notify program
// set in ~/.codex/config.toml, but only for terminals started here (it is a
// per-run override, nothing is written to the config file). TOML literal
// (single-quoted) strings are used so the value survives the cmd.exe layer
// that wraps npm's codex.cmd shim.
func codexArgs(exe, sessionID, resume string, withNotify bool) []string {
	var args []string
	if resume != "" && codexhist.ValidID(resume) {
		args = append(args, "resume")
	}
	if withNotify && exe != "" && !strings.ContainsAny(exe+sessionID, "'\n\r") {
		args = append(args, "-c", "notify=['"+filepath.ToSlash(exe)+"','notify-codex','"+sessionID+"']")
	}
	if resume != "" && codexhist.ValidID(resume) {
		args = append(args, resume) // re-validated here: it is placed on a command line
	}
	return args
}

// notifyCodexCmd is the hook helper Codex runs when a turn finishes:
// `claude-remote notify-codex <session-id> <json>`. Like the Claude helper it
// must never block or fail the tool, so every problem ends in exit 0.
func notifyCodexCmd(args []string) int {
	if len(args) < 2 {
		return 0
	}
	session, payload := args[0], args[len(args)-1]
	url, secret := os.Getenv(envNotifyURL), os.Getenv(envNotifySecret)
	if url == "" || secret == "" || session == "" {
		return 0
	}
	var in struct {
		Type string `json:"type"`
	}
	json.Unmarshal([]byte(payload), &in)
	if in.Type != "agent-turn-complete" {
		return 0
	}
	body, _ := json.Marshal(map[string]string{"session": session, "event": "stop"})
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
