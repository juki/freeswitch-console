package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/juki/fsdeck/internal/audit"
	"github.com/juki/fsdeck/internal/store"
)

const errManaged = "this account is set by the environment and cannot be changed here"

func writeUserStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalidUser):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "cannot save users: "+err.Error())
	}
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, me user) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	entry := audit.Entry{User: me.Username, Action: "password.change", Target: me.Username}
	refuse := func(code int, result, msg string) {
		entry.Result, entry.Error = result, msg
		s.audit(r, entry)
		writeError(w, code, msg)
	}
	if me.Managed {
		refuse(http.StatusForbidden, audit.Denied, errManaged)
		return
	}
	if err := checkPassword(req.New); err != nil {
		refuse(http.StatusBadRequest, audit.Error, err.Error())
		return
	}
	if !s.acquireVerify(r) {
		writeBusy(w)
		return
	}
	// Guesses at the current password count against the sign-in limit of
	// the same account and address.
	key := clientKey(r, me.Username)
	if wait := s.loginAttempt(key, time.Now()); wait > 0 {
		s.releaseVerify()
		secs := writeBlocked(w, wait)
		entry.Result, entry.Error = audit.Denied, fmt.Sprintf("too many failed attempts; retry in %ds", secs)
		s.audit(r, entry)
		return
	}
	acc, ok := s.accountByID(me.ID)
	if !ok || !verifyPassword(acc.PasswordHash, req.Current) {
		s.releaseVerify()
		refuse(http.StatusBadRequest, audit.Denied, "current password is wrong")
		return
	}
	s.loginSucceeded(key)
	hash, err := hashPassword(req.New)
	s.releaseVerify()
	if err != nil {
		refuse(http.StatusInternalServerError, audit.Error, "cannot hash the password")
		return
	}
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	cur, ok := s.cfg.Store.User(me.ID)
	if !ok || cur.PasswordHash != acc.PasswordHash {
		refuse(http.StatusConflict, audit.Error, "the password was changed meanwhile; sign in again")
		return
	}
	cur.PasswordHash = hash
	if err := s.cfg.Store.UpdateUser(cur); err != nil {
		entry.Result, entry.Error = audit.Error, err.Error()
		s.audit(r, entry)
		writeUserStoreError(w, err)
		return
	}
	s.endSessions(me.ID, sessionToken(r), hash)
	entry.Result = audit.OK
	s.audit(r, entry)
	w.WriteHeader(http.StatusNoContent)
}

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

// truncate shortens client-supplied text for the audit log.
func truncate(v string, n int) string {
	if len(v) > n {
		return v[:n]
	}
	return v
}
