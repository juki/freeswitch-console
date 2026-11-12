package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"fsdeck/internal/audit"
	"fsdeck/internal/store"
)

const (
	cookieName  = "fsdeck_session"
	sessionTTL  = 12 * time.Hour
	tokenLength = 32

	// IDs of the environment-managed accounts; file IDs are 16 hex chars.
	managedAdminID  = "env-admin"
	managedViewerID = "env-viewer"
)

// user is the signed-in account as handlers see it.
type user struct {
	ID       string `json:"-"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Managed  bool   `json:"managed"`
}

// account is an account that can sign in: a file user, or one managed by
// the environment (FSDECK_ADMIN_PASSWORD, FSDECK_VIEWER_PASSWORD).
type account struct {
	store.User
	Managed bool
}

func (a account) user() user {
	return user{ID: a.ID, Username: a.Username, Role: a.Role, Managed: a.Managed}
}

type session struct {
	userID string
	// hash is the password hash the session was opened against. A session
	// whose account has another hash now is over, which also ends one
	// opened by a sign-in that raced with a password change.
	hash    string
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

// acquireVerify takes one of the password-hashing slots, waiting at most
// verifyWait. PBKDF2 is slow by design; without a cap, sign-ins for rotating
// usernames (each a separate rate-limit key) could keep every core busy.
func (s *Server) acquireVerify(r *http.Request) bool {
	t := time.NewTimer(s.verifyWait)
	defer t.Stop()
	select {
	case s.verifySlots <- struct{}{}:
		return true
	case <-t.C:
	case <-r.Context().Done():
	}
	return false
}

func (s *Server) releaseVerify() { <-s.verifySlots }

func writeBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, "too many password checks at once; try again in a moment")
}

// writeBlocked answers 429 for a key blocked for wait and returns the
// seconds it announced.
func writeBlocked(w http.ResponseWriter, wait time.Duration) int {
	secs := int((wait + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, fmt.Sprintf("too many failed attempts; try again in %ds", secs))
	return secs
}

// clientIP is the client address of r without its port.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if a, err := netip.ParseAddr(host); err == nil {
		host = a.Unmap().String()
	}
	return host
}

// clientKey keys the rate limit on the username and the client address
// without its port. The username is truncated: it comes from the client and
// would otherwise let the capped table grow by the body limit per key.
func clientKey(r *http.Request, username string) loginKey {
	if len(username) > 64 {
		username = username[:64]
	}
	return loginKey{username: username, ip: clientIP(r)}
}

// handle registers h behind capability c. Every route goes through handle
// or handlePublic, so the route table test sees all of them. action names
// the audit event recorded when a signed-in account lacks c ("" for none).
func (s *Server) handle(m *http.ServeMux, pattern string, c capability, action string, h func(http.ResponseWriter, *http.Request, user)) {
	s.routeCaps[pattern] = c
	m.HandleFunc(pattern, s.auth(c, action, h))
}

func (s *Server) handlePublic(m *http.ServeMux, pattern string, h http.HandlerFunc) {
	s.routeCaps[pattern] = capPublic
	m.HandleFunc(pattern, h)
}

// auth requires a session whose role has c.
func (s *Server) auth(c capability, action string, h func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.sessionUser(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if !allowed(u.Role, c) {
			if action != "" {
				s.audit(r, audit.Entry{User: u.Username, Action: action, Detail: r.Method + " " + r.URL.Path, Result: audit.Denied, Error: "permission denied"})
			}
			writeError(w, http.StatusForbidden, "permission denied")
			return
		}
		h(w, r, u)
	}
}

// sessionUser resolves the session cookie to the account as it is now: a
// disabled, deleted or re-passworded account, or one the licence no longer
// covers, has no session.
func (s *Server) sessionUser(r *http.Request) (user, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return user{}, false
	}
	s.sessMu.Lock()
	sess, ok := s.sessions[c.Value]
	if ok && time.Now().After(sess.expires) {
		delete(s.sessions, c.Value)
		ok = false
	}
	s.sessMu.Unlock()
	if !ok {
		return user{}, false
	}
	acc, found := s.accountByID(sess.userID)
	if !found || acc.Disabled || !s.licensed(acc) || subtle.ConstantTimeCompare([]byte(acc.PasswordHash), []byte(sess.hash)) != 1 {
		s.sessMu.Lock()
		delete(s.sessions, c.Value)
		s.sessMu.Unlock()
		return user{}, false
	}
	return acc.user(), true
}

// endSessions ends every session of userID except keep, which is moved to
// hash (the account's new password hash) when not empty.
func (s *Server) endSessions(userID, keep, hash string) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	for token, sess := range s.sessions {
		if sess.userID != userID {
			continue
		}
		if token == keep && hash != "" {
			sess.hash = hash
			s.sessions[token] = sess
			continue
		}
		delete(s.sessions, token)
	}
}

// accountByName prefers the environment-managed account of that name.
func (s *Server) accountByName(username string) (account, bool) {
	for _, a := range s.managed {
		if a.Username == username {
			return a, true
		}
	}
	u, ok := s.cfg.Store.UserByName(username)
	return account{User: u}, ok
}

// accountByID does not find a file user hidden by a managed account of the
// same name.
func (s *Server) accountByID(id string) (account, bool) {
	for _, a := range s.managed {
		if a.ID == id {
			return a, true
		}
	}
	u, ok := s.cfg.Store.User(id)
	if !ok || s.isManagedName(u.Username) {
		return account{}, false
	}
	return account{User: u}, true
}

func (s *Server) isManagedName(username string) bool {
	for _, a := range s.managed {
		if a.Username == username {
			return true
		}
	}
	return false
}

// accounts lists the managed accounts, then the file users they do not hide.
func (s *Server) accounts() []account {
	out := append([]account(nil), s.managed...)
	for _, u := range s.cfg.Store.Users() {
		if !s.isManagedName(u.Username) {
			out = append(out, account{User: u})
		}
	}
	return out
}

func (s *Server) enabledAdmins() int {
	n := 0
	for _, a := range s.accounts() {
		if a.Role == store.RoleAdmin && !a.Disabled {
			n++
		}
	}
	return n
}

// checkLogin costs one password verification whether or not the account
// exists, so the answer time does not tell which usernames are taken.
func (s *Server) checkLogin(username, password string) (account, bool, string) {
	acc, found := s.accountByName(username)
	hash := s.dummyHash
	if found {
		hash = acc.PasswordHash
	}
	ok := verifyPassword(hash, password)
	switch {
	case !found:
		return acc, false, "unknown user"
	case acc.Disabled:
		return acc, false, "account disabled"
	case store.CheckRole(acc.Role) != nil:
		return acc, false, "unknown role"
	case !ok:
		return acc, false, "wrong password"
	}
	return acc, true, ""
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// The audit log names the account only when it exists: a password typed
	// into the username field must not end up there.
	known := ""
	if _, ok := s.accountByName(req.Username); ok {
		known = req.Username
	}
	if !s.acquireVerify(r) {
		writeBusy(w)
		return
	}
	key := clientKey(r, req.Username)
	if wait := s.loginAttempt(key, time.Now()); wait > 0 {
		s.releaseVerify()
		secs := writeBlocked(w, wait)
		s.audit(r, audit.Entry{User: known, Action: "login.blocked", Target: known, Detail: fmt.Sprintf("retry in %ds", secs), Result: audit.Denied})
		return
	}
	acc, ok, why := s.checkLogin(req.Username, req.Password)
	s.releaseVerify()
	if !ok {
		s.audit(r, audit.Entry{User: known, Action: "login.failed", Target: known, Detail: why, Result: audit.Denied})
		select {
		case <-time.After(s.loginDelay):
		case <-r.Context().Done():
		}
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if !s.licensed(acc) {
		// The password was right, so saying why is no oracle for guessing;
		// the attempt stays charged to the rate limit.
		s.audit(r, audit.Entry{User: acc.Username, Action: "login.failed", Target: acc.Username, Detail: "not covered by the licence", Result: audit.Denied})
		writeError(w, http.StatusForbidden, errUnlicensedAccount)
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
	s.sessions[token] = session{userID: acc.ID, hash: acc.PasswordHash, expires: now.Add(sessionTTL)}
	s.sessMu.Unlock()
	s.audit(r, audit.Entry{User: acc.Username, Action: "login", Target: acc.Username, Result: audit.OK})
	http.SetCookie(w, sessionCookie(r, token, int(sessionTTL/time.Second)))
	writeJSON(w, http.StatusOK, acc.user())
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, u user) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessMu.Lock()
		delete(s.sessions, c.Value)
		s.sessMu.Unlock()
	}
	s.audit(r, audit.Entry{User: u.Username, Action: "logout", Target: u.Username, Result: audit.OK})
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

// audit records e with the client address of r.
func (s *Server) audit(r *http.Request, e audit.Entry) {
	e.IP = clientIP(r)
	s.cfg.Audit.Write(e)
}
