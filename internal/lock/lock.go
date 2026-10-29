// Package lock is the optional PIN lock in front of the web UI.
//
// A cookie login alone is not enough to use the app once a PIN is set: the
// page must also present an unlock key, which it receives by entering the PIN
// and keeps only in memory. Reopening the page therefore asks for the PIN
// again, and the server expires the key after a period without input.
//
// The PIN is set and removed only on the laptop (`claude-remote pin set`):
// if the phone could change it, whoever holds an unlocked phone could replace
// it. It is stored as an argon2id hash in a file readable only by the user.
package lock

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/argon2"

	"claude-remote/internal/config"
)

const (
	fileName = "lock.json"
	// MaxFailures is how many wrong PINs one login session gets before the
	// caller must end that session (forcing the token to be entered again).
	MaxFailures = 10
)

var (
	ErrBadPIN   = errors.New("wrong PIN")
	ErrDisabled = errors.New("PIN lock is not enabled")
)

type record struct {
	Version int    `json:"version"`
	Salt    string `json:"salt"`
	Hash    string `json:"hash"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"` // KiB
	Threads uint8  `json:"threads"`
}

// ValidatePIN requires 6 to 12 digits and rejects the most guessable ones.
func ValidatePIN(pin string) error {
	if len(pin) < 6 || len(pin) > 12 {
		return errors.New("PIN должен состоять из 6–12 цифр")
	}
	same, asc, desc := true, true, true
	for i := 0; i < len(pin); i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return errors.New("PIN должен состоять только из цифр")
		}
		if i > 0 {
			same = same && pin[i] == pin[0]
			asc = asc && pin[i] == pin[i-1]+1
			desc = desc && pin[i] == pin[i-1]-1
		}
	}
	if same || asc || desc {
		return errors.New("слишком простой PIN (одинаковые цифры или последовательность)")
	}
	distinct := map[byte]bool{}
	for i := 0; i < len(pin); i++ {
		distinct[pin[i]] = true
	}
	if len(distinct) < 3 {
		return errors.New("слишком простой PIN: используйте хотя бы три разные цифры")
	}
	// A short block repeated (121212, 123123, 482482) is as guessable as the block.
	for n := 1; n <= len(pin)/2; n++ {
		if len(pin)%n == 0 && pin == strings.Repeat(pin[:n], len(pin)/n) {
			return errors.New("слишком простой PIN (повторяющийся фрагмент)")
		}
	}
	switch pin {
	case "112233", "223344", "334455", "445566", "556677", "667788", "778899", "998877", "887766", "776655", "665544", "554433", "443322", "332211":
		return errors.New("слишком простой PIN")
	}
	return nil
}

func hashPIN(pin string, salt []byte, r record) []byte {
	return argon2.IDKey([]byte(pin), salt, r.Time, r.Memory, r.Threads, 32)
}

// Set stores a new PIN in dir (used by the CLI on the laptop).
func Set(dir, pin string) error {
	if err := ValidatePIN(pin); err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	r := record{Version: 1, Time: 2, Memory: 64 * 1024, Threads: 2, Salt: base64.RawStdEncoding.EncodeToString(salt)}
	r.Hash = base64.RawStdEncoding.EncodeToString(hashPIN(pin, salt, r))
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return config.WritePrivateFile(dir, fileName, data)
}

// Remove deletes the PIN (disables the lock).
func Remove(dir string) error {
	err := os.Remove(filepath.Join(dir, fileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

type grant struct {
	sess [32]byte // hash of the login-session cookie the key is bound to
	last time.Time
}

// Lock verifies PINs and tracks unlock keys. The PIN file is re-read when it
// changes, so `pin set` takes effect without restarting the server.
type Lock struct {
	dir  string
	idle time.Duration
	now  func() time.Time

	mu      sync.Mutex
	rec     *record
	recMod  time.Time
	recSize int64
	keys    map[[32]byte]*grant // sha256(key) -> grant
	fails   map[[32]byte]int    // wrong PINs per login session

	hashes atomic.Int64 // number of PIN hashes computed (tests)
}

// New creates a Lock. idle is the inactivity after which a key stops working
// (0 means keys never expire on their own).
func New(dir string, idle time.Duration) *Lock {
	return &Lock{dir: dir, idle: idle, now: time.Now, keys: map[[32]byte]*grant{}, fails: map[[32]byte]int{}}
}

// Idle returns the inactivity timeout.
func (l *Lock) Idle() time.Duration { return l.idle }

func sid(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// load refreshes the cached PIN record when the file changed.
func (l *Lock) load() *record {
	fi, err := os.Stat(filepath.Join(l.dir, fileName))
	if err != nil {
		l.rec = nil
		return nil
	}
	if l.rec != nil && fi.ModTime().Equal(l.recMod) && fi.Size() == l.recSize {
		return l.rec
	}
	data, err := config.ReadPrivateFile(l.dir, fileName)
	if err != nil {
		l.rec = nil // unreadable or not private: treat as "cannot verify", see Enabled
		return nil
	}
	var r record
	if json.Unmarshal(data, &r) != nil || r.Salt == "" || r.Hash == "" {
		l.rec = nil
		return nil
	}
	l.rec, l.recMod, l.recSize = &r, fi.ModTime(), fi.Size()
	return l.rec
}

// Enabled reports whether a PIN is set. A lock file that exists but cannot be
// read counts as enabled: failing open would silently remove the lock.
func (l *Lock) Enabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.load() != nil {
		return true
	}
	// The file could not be understood or even examined (permission, a
	// sharing violation while `pin set` renames it, antivirus): only a clean
	// "does not exist" may switch the lock off.
	_, err := os.Stat(filepath.Join(l.dir, fileName))
	return !errors.Is(err, fs.ErrNotExist)
}

// Unlock checks the PIN for the login session and returns a fresh unlock key.
// After MaxFailures wrong PINs it returns ErrBadPIN together with tooMany=true:
// the caller must then end the login session.
func (l *Lock) Unlock(session, pin string) (key string, tooMany bool, err error) {
	l.mu.Lock()
	rec := l.load()
	if rec == nil {
		l.mu.Unlock()
		return "", false, ErrDisabled
	}
	s := sid(session)
	r := *rec
	if l.fails[s] >= MaxFailures {
		l.mu.Unlock()
		return "", true, ErrBadPIN
	}
	// The attempt is counted BEFORE the (memory-hungry) hash is computed, so a
	// burst of parallel guesses cannot exceed the limit or pile up hashes.
	l.fails[s]++
	n := l.fails[s]
	l.mu.Unlock()

	salt, e1 := base64.RawStdEncoding.DecodeString(r.Salt)
	want, e2 := base64.RawStdEncoding.DecodeString(r.Hash)
	if e1 != nil || e2 != nil {
		return "", false, fmt.Errorf("corrupt lock file")
	}
	l.hashes.Add(1)
	ok := len(pin) <= 64 && subtle.ConstantTimeCompare(hashPIN(pin, salt, r), want) == 1

	l.mu.Lock()
	defer l.mu.Unlock()
	if !ok {
		return "", n >= MaxFailures, ErrBadPIN
	}
	delete(l.fails, s)
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", false, err
	}
	key = base64.RawURLEncoding.EncodeToString(b)
	now := l.now()
	for k, g := range l.keys { // drop expired keys
		if l.expired(g, now) {
			delete(l.keys, k)
		}
	}
	l.keys[sha256.Sum256([]byte(key))] = &grant{sess: s, last: now}
	return key, false, nil
}

func (l *Lock) expired(g *grant, now time.Time) bool {
	return l.idle > 0 && now.Sub(g.last) > l.idle
}

// Valid reports whether key is a live key of this login session, without
// counting as activity (used by periodic checks).
func (l *Lock) Valid(session, key string) bool { return l.check(session, key, false) }

// Check is Valid, and also records activity (sliding idle timeout).
func (l *Lock) Check(session, key string) bool { return l.check(session, key, true) }

func (l *Lock) check(session, key string, touch bool) bool {
	if key == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	k := sha256.Sum256([]byte(key))
	g, ok := l.keys[k]
	if !ok {
		return false
	}
	s := sid(session)
	if subtle.ConstantTimeCompare(g.sess[:], s[:]) != 1 {
		return false
	}
	now := l.now()
	if l.expired(g, now) {
		delete(l.keys, k)
		return false
	}
	if touch {
		g.last = now
	}
	return true
}

// Lock revokes every key of the login session ("lock now").
func (l *Lock) Lock(session string) {
	s := sid(session)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, g := range l.keys {
		if g.sess == s {
			delete(l.keys, k)
		}
	}
}

// Forget drops all state of an ended login session.
func (l *Lock) Forget(session string) {
	l.Lock(session)
	l.mu.Lock()
	delete(l.fails, sid(session))
	l.mu.Unlock()
}
