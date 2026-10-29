// Package push sends Web Push notifications (VAPID) to the user's phone.
package push

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"claude-remote/internal/config"
)

const (
	fileName      = "push.json"
	maxSubs       = 10
	sendTimeout   = 10 * time.Second
	ttlSeconds    = 600
	vapidSubject  = "mailto:claude-remote@example.com"
	maxFieldBytes = 512
)

// pushHosts are the browser push services. A subscription's endpoint is a URL
// the SERVER will POST to, so accepting arbitrary hosts would let anyone with
// a token make this machine send requests to internal addresses (SSRF).
var pushHosts = []string{
	"fcm.googleapis.com",
	"android.googleapis.com",
	"push.services.mozilla.com",
	"push.apple.com",
	"notify.windows.com",
}

// Subscription is the browser's PushSubscription.
type Subscription = webpush.Subscription

// Notification is what the phone shows.
type Notification struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Tag     string `json:"tag"`     // same tag replaces the previous notification
	Session string `json:"session"` // terminal session to open on tap
}

type store struct {
	Public  string         `json:"public"`
	Private string         `json:"private"`
	Subs    []Subscription `json:"subs"`
}

type sender func(ctx context.Context, payload []byte, sub *Subscription, pub, priv string) (status int, err error)

type Service struct {
	dir  string
	log  *slog.Logger
	send sender

	mu sync.Mutex
	st store
}

// Load reads (or creates) the VAPID key pair and saved subscriptions from
// dir. The file holds the VAPID private key, so it is written private to the
// current user.
func Load(dir string, log *slog.Logger) (*Service, error) {
	s := &Service{dir: dir, log: log, send: defaultSend}
	data, err := config.ReadPrivateFile(dir, fileName)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &s.st); err != nil {
			return nil, fmt.Errorf("parse %s: %w", fileName, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		priv, pub, gerr := webpush.GenerateVAPIDKeys()
		if gerr != nil {
			return nil, gerr
		}
		s.st = store{Public: pub, Private: priv}
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	if s.st.Public == "" || s.st.Private == "" {
		return nil, fmt.Errorf("%s is incomplete; delete it to generate new keys", fileName)
	}
	return s, nil
}

func (s *Service) saveLocked() error {
	data, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	return config.WritePrivateFile(s.dir, fileName, data)
}

// PublicKey is the VAPID application server key the browser subscribes with.
func (s *Service) PublicKey() string { return s.st.Public }

// Count returns the number of saved subscriptions.
func (s *Service) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.st.Subs)
}

// Subscribe stores (or refreshes) a subscription after validating it.
func (s *Service) Subscribe(sub Subscription) error {
	if err := validate(sub); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, old := range s.st.Subs {
		if old.Endpoint == sub.Endpoint {
			s.st.Subs[i] = sub
			return s.saveLocked()
		}
	}
	if len(s.st.Subs) >= maxSubs {
		return errors.New("too many subscriptions")
	}
	s.st.Subs = append(s.st.Subs, sub)
	return s.saveLocked()
}

// Unsubscribe removes a subscription by endpoint.
func (s *Service) Unsubscribe(endpoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(endpoint)
}

func (s *Service) removeLocked(endpoint string) {
	kept := s.st.Subs[:0]
	for _, x := range s.st.Subs {
		if x.Endpoint != endpoint {
			kept = append(kept, x)
		}
	}
	if len(kept) != len(s.st.Subs) {
		s.st.Subs = kept
		if err := s.saveLocked(); err != nil {
			s.log.Error("save push subscriptions failed", "err", err)
		}
	}
}

func validate(sub Subscription) error {
	if len(sub.Endpoint) > 1024 || len(sub.Keys.Auth) > maxFieldBytes || len(sub.Keys.P256dh) > maxFieldBytes {
		return errors.New("subscription too large")
	}
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return errors.New("endpoint must be an https URL")
	}
	if !hostAllowed(strings.ToLower(u.Hostname())) {
		return errors.New("endpoint host is not a known push service")
	}
	for _, k := range []string{sub.Keys.Auth, sub.Keys.P256dh} {
		if k == "" {
			return errors.New("missing subscription keys")
		}
		if _, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k, "=")); err != nil {
			return errors.New("bad subscription key encoding")
		}
	}
	return nil
}

func hostAllowed(host string) bool {
	for _, h := range pushHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// Notify sends n to every subscription in the background. Subscriptions the
// push service reports as gone (404/410) are dropped.
func (s *Service) Notify(n Notification) {
	s.mu.Lock()
	subs := append([]Subscription(nil), s.st.Subs...)
	pub, priv := s.st.Public, s.st.Private
	s.mu.Unlock()
	if len(subs) == 0 {
		return
	}
	payload, _ := json.Marshal(n)
	for i := range subs {
		sub := subs[i]
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
			defer cancel()
			status, err := s.send(ctx, payload, &sub, pub, priv)
			switch {
			case err != nil:
				s.log.Warn("push send failed", "err", err)
			case status == http.StatusNotFound || status == http.StatusGone:
				s.log.Info("push subscription expired, removing")
				s.Unsubscribe(sub.Endpoint)
			case status >= 300:
				s.log.Warn("push service rejected message", "status", status)
			}
		}()
	}
}

func defaultSend(ctx context.Context, payload []byte, sub *Subscription, pub, priv string) (int, error) {
	resp, err := webpush.SendNotificationWithContext(ctx, payload, sub, &webpush.Options{
		HTTPClient: &http.Client{
			Timeout: sendTimeout,
			// Never follow redirects: the host allow-list only covers the endpoint itself.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Subscriber:      vapidSubject,
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
		TTL:             ttlSeconds,
		Urgency:         webpush.UrgencyHigh,
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}
