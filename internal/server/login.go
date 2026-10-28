package server

import (
	"bytes"
	"io/fs"
	"net/http"
	"time"

	"claude-remote/internal/auth"
)

// handleLogin serves the login page (GET, public) and validates the token
// (POST). The page uses a plain HTML form so it needs no JavaScript and the
// token never touches localStorage.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderLogin(w, "", http.StatusOK)
	case http.MethodPost:
		s.postLogin(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	ip := auth.ClientIP(r)
	// CSRF: a login POST must come from our own page. Together with the
	// SameSite=Strict cookie this stops cross-site login/logout attacks.
	if !formOriginOK(r) {
		s.o.Log.Warn("login rejected", "ip", ip, "reason", "bad_origin",
			"origin", r.Header.Get("Origin"), "host", r.Host, "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// PostForm only: never read the token from the query string.
	token := r.PostForm.Get("token")
	res, retry := s.o.Auth.Login(r, token)
	switch res {
	case auth.OK:
		id, err := s.o.Auth.NewSession()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		s.o.Log.Info("login ok", "ip", ip)
		http.SetCookie(w, &http.Cookie{
			Name:     auth.CookieName,
			Value:    id,
			Path:     "/",
			MaxAge:   int(auth.SessionTTL.Seconds()),
			HttpOnly: true,
			Secure:   s.o.Secure,
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case auth.Blocked:
		s.o.Log.Warn("login blocked", "ip", ip, "retry_after", retry.Round(time.Second))
		w.Header().Set("Retry-After", itoa(int(retry.Seconds())+1))
		s.renderLogin(w, "Слишком много попыток. Подождите и повторите.", http.StatusTooManyRequests)
	default:
		s.o.Log.Warn("login failed", "ip", ip)
		s.renderLogin(w, "Неверный токен.", http.StatusUnauthorized)
	}
}

func (s *Server) renderLogin(w http.ResponseWriter, msg string, status int) {
	page, err := fs.ReadFile(s.o.Web, "login.html")
	if err != nil {
		http.Error(w, "login page missing", http.StatusInternalServerError)
		return
	}
	// msg is one of our own constant strings, never user input.
	page = bytes.Replace(page, []byte("<!--ERROR-->"), []byte(msg), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(page)
}
