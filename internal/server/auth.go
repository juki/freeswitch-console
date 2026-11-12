package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
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

type loginKey struct{ username, ip string }

// loginRecord holds the recent failures of a key, oldest first, at most
// loginMaxFails of them.
type loginRecord struct {
	fails []time.Time
	until time.Time // blocked before this time
}

func (rec *loginRecord) expired(now time.Time, window time.Duration) bool {
	return !now.Before(rec.until) && !now.Before(rec.fails[len(rec.fails)-1].Add(window))
}

// loginAttempt charges a failure to the key before the password is checked,
// so concurrent guesses cannot all pass the check before any is counted. It
// returns how long the key stays blocked; a blocked attempt is not counted
// and does not extend the block.
func (s *Server) loginAttempt(k loginKey, now time.Time) time.Duration {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	rec := s.loginFails[k]
	if rec != nil && now.Before(rec.until) {
		return rec.until.Sub(now)
	}
	if rec == nil {
		if len(s.loginFails) >= s.loginMaxKeys {
			s.pruneLogins(now)
		}
		rec = &loginRecord{}
		s.loginFails[k] = rec
	}
	recent := rec.fails[:0]
	for _, t := range rec.fails {
		if now.Before(t.Add(s.loginWindow)) {
			recent = append(recent, t)
		}
	}
	rec.fails = append(recent, now)
	if n := len(rec.fails); n >= s.loginMaxFails {
		rec.fails = rec.fails[n-s.loginMaxFails:]
		rec.until = now.Add(s.loginBlock)
	}
	return 0
}

// pruneLogins drops expired keys, then the least recently failed one if the
// table is still full.
func (s *Server) pruneLogins(now time.Time) {
	var oldest loginKey
	var oldestAt time.Time
	for k, rec := range s.loginFails {
		if rec.expired(now, s.loginWindow) {
			delete(s.loginFails, k)
			continue
		}
		if at := rec.fails[len(rec.fails)-1]; oldestAt.IsZero() || at.Before(oldestAt) {
			oldest, oldestAt = k, at
		}
	}
	if len(s.loginFails) >= s.loginMaxKeys {
		delete(s.loginFails, oldest)
	}
}

func (s *Server) loginSucceeded(k loginKey) {
	s.loginMu.Lock()
	delete(s.loginFails, k)
	s.loginMu.Unlock()
}

// clientKey keys the rate limit on the username and the client address
// without its port. The username is truncated: it comes from the client and
// would otherwise let the capped table grow by the body limit per key.
func clientKey(r *http.Request, username string) loginKey {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if a, err := netip.ParseAddr(host); err == nil {
		host = a.Unmap().String()
	}
	if len(username) > 64 {
		username = username[:64]
	}
	return loginKey{username: username, ip: host}
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
	key := clientKey(r, req.Username)
	if wait := s.loginAttempt(key, time.Now()); wait > 0 {
		secs := int((wait + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("too many failed attempts; try again in %ds", secs))
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
	s.loginSucceeded(key)
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
