package push

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

var quiet = slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

func sub(endpoint string) Subscription {
	return Subscription{
		Endpoint: endpoint,
		Keys:     webpush.Keys{Auth: "dGVzdC1hdXRoLXNlY3JldA", P256dh: "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM"},
	}
}

func TestSubscribeValidation(t *testing.T) {
	s, err := Load(t.TempDir(), quiet)
	if err != nil {
		t.Fatal(err)
	}
	good := []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://updates.push.services.mozilla.com/wpush/v2/abc",
		"https://web.push.apple.com/abc",
		"https://wns2-par02p.notify.windows.com/abc",
	}
	for _, e := range good {
		if err := s.Subscribe(sub(e)); err != nil {
			t.Errorf("%s rejected: %v", e, err)
		}
	}
	bad := []string{
		"http://fcm.googleapis.com/x",                // not https
		"https://127.0.0.1:8080/x",                   // internal address (SSRF)
		"https://169.254.169.254/latest/meta-data",   // cloud metadata
		"https://evil.example/x",                     // unknown host
		"https://fcm.googleapis.com.evil.example/x",  // suffix trick
		"https://user:pw@fcm.googleapis.com/x",       // credentials in URL
		"https://notfcm.googleapis.com.example.com/", // look-alike
		"ftp://fcm.googleapis.com/x",
		"",
	}
	for _, e := range bad {
		if err := s.Subscribe(sub(e)); err == nil {
			t.Errorf("%q accepted", e)
		}
	}
	nokeys := sub("https://fcm.googleapis.com/x")
	nokeys.Keys.Auth = ""
	if s.Subscribe(nokeys) == nil {
		t.Error("subscription without keys accepted")
	}
}

func TestSubscriptionLimitAndRefresh(t *testing.T) {
	s, _ := Load(t.TempDir(), quiet)
	for i := 0; i < maxSubs; i++ {
		if err := s.Subscribe(sub("https://fcm.googleapis.com/" + strings.Repeat("a", i+1))); err != nil {
			t.Fatal(err)
		}
	}
	if s.Subscribe(sub("https://fcm.googleapis.com/overflow")) == nil {
		t.Fatal("limit not enforced")
	}
	// Re-subscribing the same endpoint refreshes instead of growing.
	if err := s.Subscribe(sub("https://fcm.googleapis.com/a")); err != nil || s.Count() != maxSubs {
		t.Fatalf("count %d err %v", s.Count(), err)
	}
}

func TestKeysPersistAndFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	s1, err := Load(dir, quiet)
	if err != nil {
		t.Fatal(err)
	}
	s1.Subscribe(sub("https://fcm.googleapis.com/keep"))
	s2, err := Load(dir, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if s1.PublicKey() == "" || s1.PublicKey() != s2.PublicKey() || s2.Count() != 1 {
		t.Fatalf("not persisted: %q %q %d", s1.PublicKey(), s2.PublicKey(), s2.Count())
	}
	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyDropsGoneSubscriptionsAndSendsPayload(t *testing.T) {
	s, _ := Load(t.TempDir(), quiet)
	s.Subscribe(sub("https://fcm.googleapis.com/alive"))
	s.Subscribe(sub("https://fcm.googleapis.com/gone"))

	var mu sync.Mutex
	got := map[string]string{}
	done := make(chan struct{}, 2)
	s.send = func(ctx context.Context, payload []byte, sb *Subscription, pub, priv string) (int, error) {
		defer func() { done <- struct{}{} }()
		mu.Lock()
		got[sb.Endpoint] = string(payload)
		mu.Unlock()
		if strings.HasSuffix(sb.Endpoint, "/gone") {
			return http.StatusGone, nil
		}
		return http.StatusCreated, nil
	}
	s.Notify(Notification{Title: "claude", Body: "needs approval", Tag: "t", Session: "abc"})
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("send not called")
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.Count() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("expired subscription not removed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(got["https://fcm.googleapis.com/alive"], `"body":"needs approval"`) {
		t.Fatalf("payload %q", got)
	}
}
