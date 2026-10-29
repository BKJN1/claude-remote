package agents

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

const sample = `[
 {"pid":100,"cwd":"C:\\proj\\a","kind":"interactive","startedAt":1793171045708,"sessionId":"c30f70ea-4877-4aa0-8ea7-a19c07c7d6e7","name":"alpha","status":"busy"},
 {"pid":101,"cwd":"C:\\proj\\b","kind":"background","startedAt":1793171122407,"sessionId":"56a6722b-4cd9-40ad-bf98-52fb000ce657","name":"beta","status":"idle"},
 {"pid":0,"cwd":"C:\\x","sessionId":"56a6722b-4cd9-40ad-bf98-52fb000ce657"},
 {"pid":102,"cwd":"C:\\x","sessionId":"--not-a-uuid"},
 {"pid":103,"cwd":"","sessionId":"393e2175-a7cf-4ee7-9bfe-6c6fb0aae155"}
]`

func TestParseKeepsOnlyWellFormedEntries(t *testing.T) {
	got, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[0].Status != "busy" || got[1].Kind != "background" {
		t.Fatalf("%+v", got)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Fatal("garbage accepted")
	}
	if got, err := Parse([]byte(`[]`)); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestUnknownKindTreatedAsInteractive(t *testing.T) {
	got, _ := Parse([]byte(`[{"pid":5,"cwd":"C:\\a","kind":"weird","sessionId":"c30f70ea-4877-4aa0-8ea7-a19c07c7d6e7"}]`))
	if len(got) != 1 || got[0].Kind != "interactive" {
		t.Fatalf("%+v", got)
	}
}

func TestSafeNameBlocksOptionInjection(t *testing.T) {
	for _, bad := range []string{"", "--dangerously-skip-permissions", "-x", "a b", "a&b", "a|b", "%PATH%", `a"b`, strings.Repeat("a", 65)} {
		if SafeName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"versola-d4", "claude-remote-aa", "a.b_c-1"} {
		if !SafeName(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
}

func TestLabel(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\RBk\Desktop\my proj`: "my proj",
		`C:\Users\RBk\x:y*z`:           "x_y_z",
		`C:\`:                          "C_",
		`/home/u/app/`:                 "app",
	} {
		if got := Label(in); got != want {
			t.Errorf("Label(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListUsesRunnerAndPropagatesErrors(t *testing.T) {
	p := New("claude")
	p.run = func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") != "agents --json" {
			t.Errorf("unexpected args %v", args)
		}
		return []byte(sample), nil
	}
	got, err := p.List(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	p.run = func(ctx context.Context, args ...string) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := p.List(context.Background()); err == nil {
		t.Fatal("error swallowed")
	}
}

func currentPID() int { return os.Getpid() }

func TestKillRefusesOwnProcess(t *testing.T) {
	p := New("claude")
	if err := p.Kill(Agent{PID: currentPID()}); err == nil {
		t.Fatal("tried to kill the server itself")
	}
}

func TestUnsafeWorkingDirectoriesAreDropped(t *testing.T) {
	data := `[
	 {"pid":1,"cwd":"\\\\evil-host\\share","sessionId":"c30f70ea-4877-4aa0-8ea7-a19c07c7d6e7"},
	 {"pid":2,"cwd":"\\\\?\\C:\\x","sessionId":"56a6722b-4cd9-40ad-bf98-52fb000ce657"},
	 {"pid":3,"cwd":"relative\\dir","sessionId":"393e2175-a7cf-4ee7-9bfe-6c6fb0aae155"},
	 {"pid":4,"cwd":"//host/share","sessionId":"0de82fda-60a1-4bad-a3a1-ecdc6fcc2848"},
	 {"pid":5,"cwd":"C:\\ok\\dir","sessionId":"052c199a-1c8e-4e48-88b9-47e69f0403eb"}
	]`
	got, err := Parse([]byte(data))
	if err != nil || len(got) != 1 || got[0].PID != 5 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestPreflight(t *testing.T) {
	p := New("definitely-not-installed-xyz")
	if err := p.Preflight(Agent{CWD: t.TempDir()}); err == nil {
		t.Fatal("missing command not detected before closing the window")
	}
	p = New("go") // present on any machine running these tests
	if err := p.Preflight(Agent{CWD: t.TempDir()}); err != nil {
		t.Fatalf("%v", err)
	}
	if err := p.Preflight(Agent{CWD: t.TempDir() + `\missing`}); err == nil {
		t.Fatal("missing folder not detected")
	}
	if err := p.Preflight(Agent{CWD: `\\host\share`}); err == nil {
		t.Fatal("UNC path accepted")
	}
}
