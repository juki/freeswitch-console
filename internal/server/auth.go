package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"time"
)

const (
	roleAdmin   = "admin"
	roleViewer  = "viewer"
	cookieName  = "fsdeck_session"
	sessionTTL  = 12 * time.Hour
	tokenLength = 32
)

type user struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

type session struct {
	user    user
	expires time.Time
}

// auth requires a session, and the admin role when admin is set.
func (s *Server) auth(admin bool, h func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.sessionUser(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if admin && u.Role != roleAdmin {
			writeError(w, http.StatusForbidden, "admin role required")
			return
		}
		h(w, r, u)
	}
}

func (s *Server) sessionUser(r *http.Request) (user, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return user{}, false
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	sess, ok := s.sessions[c.Value]
	if !ok {
		return user{}, false
	}
	if time.Now().After(sess.expires) {
		delete(s.sessions, c.Value)
		return user{}, false
	}
	return sess.user, true
}

// passwordEqual hashes first so the comparison does not leak the length.
func passwordEqual(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (s *Server) checkLogin(username, password string) (user, bool) {
	switch {
	case username == roleAdmin && passwordEqual(password, s.cfg.AdminPassword):
		return user{Username: roleAdmin, Role: roleAdmin}, true
	case username == roleViewer && s.cfg.ViewerPassword != "" && passwordEqual(password, s.cfg.ViewerPassword):
		return user{Username: roleViewer, Role: roleViewer}, true
	}
	return user{}, false
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	u, ok := s.checkLogin(req.Username, req.Password)
	if !ok {
		select {
		case <-time.After(s.loginDelay):
		case <-r.Context().Done():
		}
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	b := make([]byte, tokenLength)
	if _, err := rand.Read(b); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	token := hex.EncodeToString(b)
	now := time.Now()
	s.sessMu.Lock()
	for t, sess := range s.sessions {
		if now.After(sess.expires) {
			delete(s.sessions, t)
		}
	}
	s.sessions[token] = session{user: u, expires: now.Add(sessionTTL)}
	s.sessMu.Unlock()
	http.SetCookie(w, sessionCookie(r, token, int(sessionTTL/time.Second)))
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, _ user) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessMu.Lock()
		delete(s.sessions, c.Value)
		s.sessMu.Unlock()
	}
	http.SetCookie(w, sessionCookie(r, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, _ *http.Request, u user) {
	writeJSON(w, http.StatusOK, u)
}

func sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		// Behind a TLS-terminating proxy r.TLS is nil. Trusting the header is
		// safe: a client that forges it only restricts its own cookie.
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	}
}
