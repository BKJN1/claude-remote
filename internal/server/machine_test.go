package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-remote/internal/machine"
)

// fakeMachine records every call so tests can prove that something did NOT happen.
type fakeMachine struct {
	mu        sync.Mutex
	status    machine.Status
	locks     int
	sleeps    []bool // hibernate flag per call
	shutdowns []struct {
		restart bool
		delay   int
		force   bool
	}
	cancels   int
	keep      bool
	failSleep error
	cancelErr error
	slept     chan struct{}
}

func newFake() *fakeMachine {
	return &fakeMachine{
		status: machine.Status{
			Battery: &machine.Battery{Percent: 82, OnAC: false, MinutesLeft: 190},
			Thermal: &machine.Thermal{Celsius: 53.1, PassiveLimit: 100},
			Disks:   []machine.Disk{{Name: "C:", TotalGB: 470, FreeGB: 41, Percent: 91}},
		},
		slept: make(chan struct{}, 4),
	}
}

func (f *fakeMachine) Status() (machine.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.status
	s.KeepAwake = f.keep
	return s, nil
}
func (f *fakeMachine) Lock() error { f.mu.Lock(); defer f.mu.Unlock(); f.locks++; return nil }
func (f *fakeMachine) Sleep(h bool) error {
	f.mu.Lock()
	f.sleeps = append(f.sleeps, h)
	err := f.failSleep
	f.mu.Unlock()
	f.slept <- struct{}{}
	return err
}
func (f *fakeMachine) Shutdown(restart bool, delay int, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdowns = append(f.shutdowns, struct {
		restart bool
		delay   int
		force   bool
	}{restart, delay, force})
	return nil
}
func (f *fakeMachine) CancelShutdown() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return f.cancelErr
}
func (f *fakeMachine) SetKeepAwake(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keep = on
	return nil
}
func (f *fakeMachine) KeepAwake() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.keep }

func (f *fakeMachine) counts() (locks, sleeps, shutdowns int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.locks, len(f.sleeps), len(f.shutdowns)
}

func machineEnv(t *testing.T, mod func(*Options)) (*env, *fakeMachine) {
	t.Helper()
	old := powerActDelay
	powerActDelay = 20 * time.Millisecond
	t.Cleanup(func() { powerActDelay = old })
	f := newFake()
	e := newEnv(t, func(o *Options) {
		o.Machine = f
		if mod != nil {
			mod(o)
		}
	})
	return e, f
}

type prepared struct {
	Code         string `json:"code"`
	NeedsPIN     bool   `json:"needs_pin"`
	Irreversible bool   `json:"irreversible"`
	DelaySec     int    `json:"delay_sec"`
}

func prepare(t *testing.T, e *env, action string) (int, prepared) {
	t.Helper()
	resp, body := call(t, e, "POST", "/api/power/prepare", []byte(`{"action":"`+action+`"}`), true, nil)
	var p prepared
	json.Unmarshal(body, &p)
	return resp.StatusCode, p
}

func execute(t *testing.T, e *env, body string) (int, string) {
	t.Helper()
	resp, b := call(t, e, "POST", "/api/power/execute", []byte(body), true, nil)
	return resp.StatusCode, string(b)
}

func TestMachineStatusEndpoint(t *testing.T) {
	e, _ := machineEnv(t, nil)
	resp, body := call(t, e, "GET", "/api/machine", nil, false, nil)
	s := string(body)
	if resp.StatusCode != 200 || !strings.Contains(s, `"percent":82`) || !strings.Contains(s, `"celsius":53.1`) ||
		!strings.Contains(s, `"power_enabled":true`) || !strings.Contains(s, `"name":"C:"`) {
		t.Fatalf("%d %s", resp.StatusCode, s)
	}
	off := newEnv(t, nil)
	if resp, _ := call(t, off, "GET", "/api/machine", nil, false, nil); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("without a machine: %d", resp.StatusCode)
	}
}

func TestEveryMachineRouteNeedsAuthAndCSRF(t *testing.T) {
	e, f := machineEnv(t, nil)
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/machine"}, {"POST", "/api/machine/lock"}, {"POST", "/api/machine/keepawake"},
		{"POST", "/api/power/prepare"}, {"POST", "/api/power/execute"}, {"POST", "/api/power/cancel"},
	} {
		req, _ := http.NewRequest(c.m, e.ts.URL+c.p, strings.NewReader(`{"action":"shutdown"}`))
		req.Header.Set("X-CR", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without auth: %d", c.m, c.p, resp.StatusCode)
		}
	}
	// Authenticated but without the CSRF header: refused, nothing happens.
	for _, p := range []string{"/api/machine/lock", "/api/power/prepare", "/api/power/execute", "/api/power/cancel"} {
		if resp, _ := call(t, e, "POST", p, []byte(`{"action":"shutdown"}`), false, nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s without CSRF header: %d", p, resp.StatusCode)
		}
	}
	if l, s, d := f.counts(); l+s+d != 0 || f.cancels != 0 {
		t.Fatal("an action ran without CSRF protection")
	}
}

func TestSleepNeedsPrepareThenExecute(t *testing.T) {
	e, f := machineEnv(t, nil)
	// Executing without a code does nothing.
	if code, _ := execute(t, e, `{"action":"sleep","code":"nope"}`); code != http.StatusBadRequest {
		t.Fatalf("no prepare: %d", code)
	}
	time.Sleep(60 * time.Millisecond)
	if _, s, _ := f.counts(); s != 0 {
		t.Fatal("slept without a confirmation code")
	}

	code, p := prepare(t, e, "sleep")
	if code != 200 || p.Code == "" || !p.Irreversible || p.NeedsPIN {
		t.Fatalf("%d %+v", code, p)
	}
	status, body := execute(t, e, `{"action":"sleep","code":"`+p.Code+`"}`)
	if status != 200 || !strings.Contains(body, "scheduled") {
		t.Fatalf("%d %s", status, body)
	}
	select {
	case <-f.slept:
	case <-time.After(2 * time.Second):
		t.Fatal("sleep never started")
	}
	if len(f.sleeps) != 1 || f.sleeps[0] {
		t.Fatalf("sleep calls %v", f.sleeps)
	}
}

func TestHibernateUsesTheHibernateFlag(t *testing.T) {
	e, f := machineEnv(t, nil)
	_, p := prepare(t, e, "hibernate")
	execute(t, e, `{"action":"hibernate","code":"`+p.Code+`"}`)
	select {
	case <-f.slept:
	case <-time.After(2 * time.Second):
		t.Fatal("never started")
	}
	if len(f.sleeps) != 1 || !f.sleeps[0] {
		t.Fatalf("%v", f.sleeps)
	}
}

func TestCodeIsSingleUseAndBoundToTheAction(t *testing.T) {
	e, f := machineEnv(t, nil)
	_, p := prepare(t, e, "shutdown")
	// A code issued for shutdown cannot restart or sleep the machine.
	for _, act := range []string{"restart", "sleep", "hibernate"} {
		if code, _ := execute(t, e, `{"action":"`+act+`","code":"`+p.Code+`"}`); code != http.StatusBadRequest {
			t.Errorf("%s with a shutdown code: %d", act, code)
		}
	}
	if l, s, d := f.counts(); l+s+d != 0 {
		t.Fatal("a mismatched code triggered something")
	}
	if code, _ := execute(t, e, `{"action":"shutdown","code":"`+p.Code+`"}`); code != 200 {
		t.Fatalf("valid: %d", code)
	}
	// Replaying it does nothing.
	if code, _ := execute(t, e, `{"action":"shutdown","code":"`+p.Code+`"}`); code != http.StatusBadRequest {
		t.Fatalf("replay: %d", code)
	}
	if _, _, d := f.counts(); d != 1 {
		t.Fatalf("shutdown ran %d times", d)
	}
}

func TestShutdownAndRestartUseAFixedDelayAndOptionalForce(t *testing.T) {
	e, f := machineEnv(t, nil)
	_, p := prepare(t, e, "restart")
	if p.DelaySec != 30 || p.Irreversible {
		t.Fatalf("%+v", p)
	}
	status, body := execute(t, e, `{"action":"restart","code":"`+p.Code+`","force":true}`)
	if status != 200 || !strings.Contains(body, `"cancellable":true`) {
		t.Fatalf("%d %s", status, body)
	}
	_, p2 := prepare(t, e, "shutdown")
	execute(t, e, `{"action":"shutdown","code":"`+p2.Code+`"}`)
	if len(f.shutdowns) != 2 {
		t.Fatalf("%+v", f.shutdowns)
	}
	if got := f.shutdowns[0]; !got.restart || got.delay != 30 || !got.force {
		t.Fatalf("restart: %+v", got)
	}
	if got := f.shutdowns[1]; got.restart || got.delay != 30 || got.force {
		t.Fatalf("shutdown: %+v", got)
	}
}

func TestUnknownActionsAreRefused(t *testing.T) {
	e, f := machineEnv(t, nil)
	for _, a := range []string{"", "poweroff", "SLEEP", "sleep; shutdown", "../shutdown", "format"} {
		if code, _ := prepare(t, e, a); code != http.StatusBadRequest {
			t.Errorf("prepare %q: %d", a, code)
		}
		if st, _ := execute(t, e, `{"action":"`+a+`","code":"x"}`); st != http.StatusBadRequest {
			t.Errorf("execute %q: %d", a, st)
		}
	}
	if l, s, d := f.counts(); l+s+d != 0 {
		t.Fatal("an unknown action did something")
	}
}

func TestCodeExpires(t *testing.T) {
	old := powerCodeTTL
	powerCodeTTL = 50 * time.Millisecond
	defer func() { powerCodeTTL = old }()
	e, f := machineEnv(t, nil)
	_, p := prepare(t, e, "shutdown")
	time.Sleep(120 * time.Millisecond)
	if code, _ := execute(t, e, `{"action":"shutdown","code":"`+p.Code+`"}`); code != http.StatusBadRequest {
		t.Fatalf("expired code accepted: %d", code)
	}
	if _, _, d := f.counts(); d != 0 {
		t.Fatal("ran on an expired code")
	}
}

// A code belongs to whoever asked for it.
func TestCodeCannotBeUsedByAnotherLogin(t *testing.T) {
	e, f := machineEnv(t, nil)
	cookieA, _ := e.srv.o.Auth.NewSession()
	cookieB, _ := e.srv.o.Auth.NewSession()
	resp, body := asBrowser(t, e, cookieA, "POST", "/api/power/prepare", []byte(`{"action":"shutdown"}`), nil)
	var p prepared
	json.Unmarshal(body, &p)
	if resp.StatusCode != 200 || p.Code == "" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if r, _ := asBrowser(t, e, cookieB, "POST", "/api/power/execute", []byte(`{"action":"shutdown","code":"`+p.Code+`"}`), nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("another login used the code: %d", r.StatusCode)
	}
	if _, _, d := f.counts(); d != 0 {
		t.Fatal("ran for the wrong caller")
	}
	if r, _ := asBrowser(t, e, cookieA, "POST", "/api/power/execute", []byte(`{"action":"shutdown","code":"`+p.Code+`"}`), nil); r.StatusCode != 200 {
		t.Fatalf("owner refused: %d", r.StatusCode)
	}
}

func TestPINIsRequiredForPowerActionsWhenSet(t *testing.T) {
	e, cookie := lockEnv(t, time.Minute, nil)
	f := newFake()
	e.srv.o.Machine = f
	old := powerActDelay
	powerActDelay = 10 * time.Millisecond
	defer func() { powerActDelay = old }()

	_, key := unlock(t, e, cookie, testPIN)
	hdr := map[string]string{"X-Unlock": key}
	resp, body := asBrowser(t, e, cookie, "POST", "/api/power/prepare", []byte(`{"action":"shutdown"}`), hdr)
	var p prepared
	json.Unmarshal(body, &p)
	if resp.StatusCode != 200 || !p.NeedsPIN {
		t.Fatalf("prepare: %d %+v", resp.StatusCode, p)
	}
	exec := func(pin string) int {
		r, _ := asBrowser(t, e, cookie, "POST", "/api/power/execute", []byte(`{"action":"shutdown","code":"`+p.Code+`","pin":"`+pin+`"}`), hdr)
		return r.StatusCode
	}
	// Unlocked page, no PIN: refused (an empty PIN is a wrong PIN).
	if st := exec(""); st != http.StatusForbidden {
		t.Fatalf("no PIN: %d", st)
	}
	if st := exec("000001"); st != http.StatusForbidden {
		t.Fatalf("wrong PIN: %d", st)
	}
	if _, _, d := f.counts(); d != 0 {
		t.Fatal("ran without the right PIN")
	}
	// The code survives a wrong PIN, so a typo does not force starting over.
	if st := exec(testPIN); st != 200 {
		t.Fatalf("right PIN: %d", st)
	}
	if _, _, d := f.counts(); d != 1 {
		t.Fatalf("shutdowns: %d", d)
	}
}

func TestWrongPINsOnPowerActionsShareTheLockout(t *testing.T) {
	e, cookie := lockEnv(t, time.Minute, nil)
	f := newFake()
	e.srv.o.Machine = f
	_, key := unlock(t, e, cookie, testPIN)
	hdr := map[string]string{"X-Unlock": key}
	_, body := asBrowser(t, e, cookie, "POST", "/api/power/prepare", []byte(`{"action":"sleep"}`), hdr)
	var p prepared
	json.Unmarshal(body, &p)
	var last int
	for i := 0; i < 6; i++ {
		r, _ := asBrowser(t, e, cookie, "POST", "/api/power/execute", []byte(`{"action":"sleep","code":"`+p.Code+`","pin":"000001"}`), hdr)
		last = r.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("PIN guessing through the power endpoint is not limited: %d", last)
	}
	if _, s, _ := f.counts(); s != 0 {
		t.Fatal("slept")
	}
}

func TestLockedPageCannotReachPowerEndpoints(t *testing.T) {
	e, cookie := lockEnv(t, time.Minute, nil)
	f := newFake()
	e.srv.o.Machine = f
	for _, p := range []string{"/api/power/prepare", "/api/power/execute", "/api/machine/lock", "/api/power/cancel"} {
		if r, _ := asBrowser(t, e, cookie, "POST", p, []byte(`{"action":"sleep"}`), nil); r.StatusCode != http.StatusLocked {
			t.Errorf("%s while locked: %d", p, r.StatusCode)
		}
	}
	if r, _ := asBrowser(t, e, cookie, "GET", "/api/machine", nil, nil); r.StatusCode != http.StatusLocked {
		t.Errorf("status while locked: %d", r.StatusCode)
	}
}

func TestNoPowerFlagRemovesDestructiveActionsOnly(t *testing.T) {
	e, f := machineEnv(t, func(o *Options) { o.NoPower = true })
	if code, _ := prepare(t, e, "shutdown"); code != http.StatusForbidden {
		t.Fatalf("prepare: %d", code)
	}
	if st, _ := execute(t, e, `{"action":"shutdown","code":"x"}`); st != http.StatusForbidden {
		t.Fatalf("execute: %d", st)
	}
	_, body := call(t, e, "GET", "/api/machine", nil, false, nil)
	if !strings.Contains(string(body), `"power_enabled":false`) {
		t.Fatalf("%s", body)
	}
	// Monitoring, screen lock and keep-awake still work.
	if resp, _ := call(t, e, "POST", "/api/machine/lock", nil, true, nil); resp.StatusCode != 200 {
		t.Fatalf("lock: %d", resp.StatusCode)
	}
	if resp, _ := call(t, e, "POST", "/api/machine/keepawake", []byte(`{"on":true}`), true, nil); resp.StatusCode != 200 || !f.keep {
		t.Fatalf("keepawake: %d", resp.StatusCode)
	}
	if l, s, d := f.counts(); l != 1 || s != 0 || d != 0 {
		t.Fatalf("calls %d %d %d", l, s, d)
	}
}

func TestCancelShutdown(t *testing.T) {
	e, f := machineEnv(t, nil)
	if resp, _ := call(t, e, "POST", "/api/power/cancel", nil, true, nil); resp.StatusCode != 200 || f.cancels != 1 {
		t.Fatalf("%d", resp.StatusCode)
	}
	f.cancelErr = errors.New("no shutdown in progress")
	if resp, _ := call(t, e, "POST", "/api/power/cancel", nil, true, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("nothing to cancel: %d", resp.StatusCode)
	}
}

func TestPowerActionsAreLoggedWithoutSecrets(t *testing.T) {
	e, _ := machineEnv(t, nil)
	_, p := prepare(t, e, "shutdown")
	execute(t, e, `{"action":"shutdown","code":"`+p.Code+`","pin":"123789"}`)
	logs := e.logs.String()
	if !strings.Contains(logs, "power action") || !strings.Contains(logs, "action=shutdown") {
		t.Fatalf("not logged:\n%s", logs)
	}
	for _, secret := range []string{p.Code, "123789", testToken} {
		if strings.Contains(logs, secret) {
			t.Errorf("log leaks %q", secret)
		}
	}
}

// With a PIN set, a stolen token alone must not be able to switch the machine off.
func TestPINIsRequiredEvenForBearerCallers(t *testing.T) {
	e, _ := lockEnv(t, time.Minute, nil)
	f := newFake()
	e.srv.o.Machine = f
	// Bearer callers are not gated by the unlock screen, but power actions still need the PIN.
	resp, body := call(t, e, "POST", "/api/power/prepare", []byte(`{"action":"shutdown"}`), true, nil)
	var p prepared
	json.Unmarshal(body, &p)
	if resp.StatusCode != 200 || !p.NeedsPIN {
		t.Fatalf("prepare: %d %s", resp.StatusCode, body)
	}
	if r, _ := call(t, e, "POST", "/api/power/execute", []byte(`{"action":"shutdown","code":"`+p.Code+`"}`), true, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("no PIN with a token: %d", r.StatusCode)
	}
	if _, _, d := f.counts(); d != 0 {
		t.Fatal("a token alone switched the machine off")
	}
	if r, _ := call(t, e, "POST", "/api/power/execute", []byte(`{"action":"shutdown","code":"`+p.Code+`","pin":"`+testPIN+`"}`), true, nil); r.StatusCode != 200 {
		t.Fatalf("token + PIN: %d", r.StatusCode)
	}
	if _, _, d := f.counts(); d != 1 {
		t.Fatalf("shutdowns %d", d)
	}
}

func TestPendingCodesAreCapped(t *testing.T) {
	e, _ := machineEnv(t, nil)
	var last int
	for i := 0; i < maxPendingCodes+5; i++ {
		last, _ = prepare(t, e, "sleep")
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("prepare spam not capped: %d", last)
	}
}
