package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const codexID = "019ed1cd-5c58-71a1-bf33-fb580807e9f5"

func TestCodexArgsNewAndResume(t *testing.T) {
	got := codexArgs(`C:\tools\claude-remote.exe`, "abc123", "", true)
	if len(got) != 2 || got[0] != "-c" || got[1] != "notify=['C:/tools/claude-remote.exe','notify-codex','abc123']" {
		t.Fatalf("new: %q", got)
	}
	got = codexArgs(`C:\tools\claude-remote.exe`, "abc123", codexID, true)
	if len(got) != 4 || got[0] != "resume" || got[1] != "-c" || got[3] != codexID {
		t.Fatalf("resume: %q", got)
	}
	if got := codexArgs(`C:\x\a.exe`, "s", "", false); len(got) != 0 {
		t.Fatalf("notify disabled: %q", got)
	}
}

// Nothing a client controls may turn into an option or break out of the TOML value.
func TestCodexArgsRefuseUnsafeInput(t *testing.T) {
	for _, bad := range []string{"--yolo", "--dangerously-bypass-approvals-and-sandbox", "last", codexID + " --yolo", "../x"} {
		got := codexArgs(`C:\x\a.exe`, "s", bad, true)
		for _, a := range got {
			if a == bad || a == "resume" {
				t.Errorf("unsafe resume id %q reached the command line: %q", bad, got)
			}
		}
	}
	if got := codexArgs(`C:\it's\a.exe`, "s", "", true); len(got) != 0 {
		t.Fatalf("quote in the program path must disable notify instead of breaking out: %q", got)
	}
	if got := codexArgs(`C:\x\a.exe`, "s'] ; evil", "", true); len(got) != 0 {
		t.Fatalf("quote in the session id: %q", got)
	}
}

func TestNotifyCodexSendsOnlyTurnCompletion(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 200)
		n, _ := r.Body.Read(b)
		hits = append(hits, r.Header.Get("Authorization")+" "+string(b[:n]))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv(envNotifyURL, srv.URL)
	t.Setenv(envNotifySecret, "s3cret")

	if code := notifyCodexCmd([]string{"abc123", `{"type":"agent-turn-complete","thread-id":"x"}`}); code != 0 {
		t.Fatal(code)
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "Bearer s3cret") || !strings.Contains(hits[0], `"session":"abc123"`) || !strings.Contains(hits[0], `"event":"stop"`) {
		t.Fatalf("%q", hits)
	}
	notifyCodexCmd([]string{"abc123", `{"type":"something-else"}`})
	notifyCodexCmd([]string{"abc123", `not json`})
	notifyCodexCmd([]string{`{"type":"agent-turn-complete"}`}) // too few arguments
	if len(hits) != 1 {
		t.Fatalf("unexpected notifications: %q", hits)
	}
	// Never fails the tool, even without the environment.
	t.Setenv(envNotifyURL, "")
	if code := notifyCodexCmd([]string{"abc123", `{"type":"agent-turn-complete"}`}); code != 0 {
		t.Fatal("must exit 0")
	}
}
