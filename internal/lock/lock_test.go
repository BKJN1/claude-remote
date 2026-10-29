package lock

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newLock(t *testing.T, idle time.Duration) (*Lock, string) {
	t.Helper()
	dir := t.TempDir()
	if err := Set(dir, "493021"); err != nil {
		t.Fatal(err)
	}
	return New(dir, idle), dir
}

func TestValidatePIN(t *testing.T) {
	for _, bad := range []string{"", "12345", "1234567890123", "12a456", "000000", "123456", "987654", "11111111", "12 345",
		"121212", "123123", "482482", "112233", "111222", "7070707070"} {
		if ValidatePIN(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"493021", "135790", "20240229"} {
		if err := ValidatePIN(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
}

func TestPINIsStoredHashedAndPrivate(t *testing.T) {
	_, dir := newLock(t, time.Minute)
	data, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "493021") {
		t.Fatal("the PIN is stored in clear text")
	}
}

func TestDisabledWithoutPINFile(t *testing.T) {
	l := New(t.TempDir(), time.Minute)
	if l.Enabled() {
		t.Fatal("enabled without a PIN")
	}
	if _, _, err := l.Unlock("sess", "493021"); err != ErrDisabled {
		t.Fatalf("got %v", err)
	}
}

func TestUnlockRightAndWrongPIN(t *testing.T) {
	l, _ := newLock(t, time.Minute)
	if !l.Enabled() {
		t.Fatal("not enabled")
	}
	if _, _, err := l.Unlock("sess", "000001"); err != ErrBadPIN {
		t.Fatalf("wrong PIN: %v", err)
	}
	key, _, err := l.Unlock("sess", "493021")
	if err != nil || key == "" {
		t.Fatalf("right PIN: %v", err)
	}
	if !l.Check("sess", key) {
		t.Fatal("fresh key rejected")
	}
}

func TestKeyIsBoundToItsLoginSession(t *testing.T) {
	l, _ := newLock(t, time.Minute)
	key, _, _ := l.Unlock("sess-a", "493021")
	if l.Check("sess-b", key) {
		t.Fatal("a key worked for a different login session")
	}
	if l.Check("sess-a", "") || l.Check("sess-a", "guess") {
		t.Fatal("bogus key accepted")
	}
}

func TestIdleTimeoutSlidingAndValidDoesNotTouch(t *testing.T) {
	l, _ := newLock(t, 5*time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }
	key, _, _ := l.Unlock("s", "493021")

	now = now.Add(4 * time.Minute)
	if !l.Valid("s", key) { // does not count as activity
		t.Fatal("expired too early")
	}
	now = now.Add(2 * time.Minute) // 6 minutes since the last real activity
	if l.Check("s", key) {
		t.Fatal("key survived the idle timeout (Valid must not extend it)")
	}

	key, _, _ = l.Unlock("s", "493021")
	for i := 0; i < 4; i++ { // activity every 4 minutes keeps it alive
		now = now.Add(4 * time.Minute)
		if !l.Check("s", key) {
			t.Fatalf("expired during activity at step %d", i)
		}
	}
}

func TestZeroIdleNeverExpires(t *testing.T) {
	l, _ := newLock(t, 0)
	now := time.Now()
	l.now = func() time.Time { return now }
	key, _, _ := l.Unlock("s", "493021")
	now = now.Add(48 * time.Hour)
	if !l.Check("s", key) {
		t.Fatal("expired although idle timeout is disabled")
	}
}

func TestLockNowAndForget(t *testing.T) {
	l, _ := newLock(t, time.Minute)
	k1, _, _ := l.Unlock("s", "493021")
	k2, _, _ := l.Unlock("other", "493021")
	l.Lock("s")
	if l.Check("s", k1) || !l.Check("other", k2) {
		t.Fatal("lock-now affected the wrong session")
	}
}

func TestTooManyFailuresEndsTheSession(t *testing.T) {
	l, _ := newLock(t, time.Minute)
	var tooMany bool
	for i := 0; i < MaxFailures; i++ {
		_, tooMany, _ = l.Unlock("s", "000001")
	}
	if !tooMany {
		t.Fatal("failure limit not reported")
	}
	// A different session is unaffected.
	if _, tooMany, _ := l.Unlock("fresh", "000001"); tooMany {
		t.Fatal("limit leaked across sessions")
	}
	l.Forget("s")
	if _, tooMany, _ := l.Unlock("s", "000001"); tooMany {
		t.Fatal("counter not reset by Forget")
	}
}

func TestChangingThePINTakesEffectWithoutRestart(t *testing.T) {
	l, dir := newLock(t, time.Minute)
	if _, _, err := l.Unlock("s", "493021"); err != nil {
		t.Fatal(err)
	}
	if err := Set(dir, "815260"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Unlock("s", "493021"); err != ErrBadPIN {
		t.Fatalf("old PIN still works: %v", err)
	}
	if _, _, err := l.Unlock("s", "815260"); err != nil {
		t.Fatalf("new PIN rejected: %v", err)
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if l.Enabled() {
		t.Fatal("still enabled after removal")
	}
}

// A lock file that exists but cannot be understood must not silently disable the lock.
func TestCorruptLockFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(dir, time.Minute)
	if !l.Enabled() {
		t.Fatal("a corrupt lock file disabled the lock")
	}
	if _, _, err := l.Unlock("s", "493021"); err == nil {
		t.Fatal("unlocked with a corrupt lock file")
	}
}

// Parallel guesses must not all get hashed: the attempt is counted first.
func TestParallelGuessesAreBoundedBeforeHashing(t *testing.T) {
	l, _ := newLock(t, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.Unlock("burst", "000001") }()
	}
	wg.Wait()
	if n := l.hashes.Load(); n > MaxFailures {
		t.Fatalf("%d hashes computed for one session (limit %d): memory DoS", n, MaxFailures)
	}
	// The session is now out of attempts, even with the right PIN.
	if _, tooMany, err := l.Unlock("burst", "493021"); err != ErrBadPIN || !tooMany {
		t.Fatalf("right PIN accepted after the limit: %v %v", err, tooMany)
	}
}

// Anything other than a clean "does not exist" must keep the lock ON.
func TestOnlyMissingFileDisablesTheLock(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, time.Minute)
	if l.Enabled() {
		t.Fatal("enabled without a file")
	}
	// A directory where the file should be: exists, cannot be read as a PIN.
	if err := os.Mkdir(filepath.Join(dir, fileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if !l.Enabled() {
		t.Fatal("an unreadable lock file switched the lock off")
	}
}
