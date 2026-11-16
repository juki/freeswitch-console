package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/juki/freeswitch-console/internal/store"
)

// In demo mode (Config.Demo) nobody signs in: a request without a valid
// session gets one for the built-in account demo, an admin. At most
// demoMaxSessions are open at once; a new one beyond that ends the oldest,
// whose visitor gets a new one on the next request. Changes to accounts,
// instances and files are refused with 403.

const demoMaxSessions = 5000

// demoAccount has no password: its hash matches no password, and its
// sessions are bound to this value as others are to their hash.
var demoAccount = account{User: store.User{ID: "demo", Username: "demo", Role: store.RoleAdmin, PasswordHash: "demo"}, Managed: true}

// demoDisabled are the routes refused in demo mode.
var demoDisabled = map[string]bool{
	"POST /api/instances":                      true,
	"PUT /api/instances/{id}":                  true,
	"DELETE /api/instances/{id}":               true,
	"POST /api/users":                          true,
	"PUT /api/users/{id}":                      true,
	"DELETE /api/users/{id}":                   true,
	"POST /api/me/password":                    true,
	"PUT /api/instances/{id}/files/content":    true,
	"DELETE /api/instances/{id}/files/content": true,
	"POST /api/instances/{id}/files/restore":   true,
}

func disabledInDemo(w http.ResponseWriter, _ *http.Request, _ user) {
	writeError(w, http.StatusForbidden, "disabled in the demo")
}

// demoSession opens a session of the demo account for r, sets its cookie
// on w and on r, so the handler sees it as any other session. When
// demoMaxSessions are open it ends the oldest first.
func (s *Server) demoSession(w http.ResponseWriter, r *http.Request) (user, bool) {
	b := make([]byte, tokenLength)
	if _, err := rand.Read(b); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create session")
		return user{}, false
	}
	token := hex.EncodeToString(b)
	now := time.Now()
	s.sessMu.Lock()
	// demoOrder also holds tokens of sessions that have ended since; they
	// are skipped here, and dropped once it has grown to twice the bound.
	for len(s.sessions) >= demoMaxSessions && len(s.demoOrder) > 0 {
		delete(s.sessions, s.demoOrder[0])
		s.demoOrder = s.demoOrder[1:]
	}
	if len(s.demoOrder) >= 2*demoMaxSessions {
		open := make([]string, 0, len(s.sessions))
		for _, t := range s.demoOrder {
			if _, ok := s.sessions[t]; ok {
				open = append(open, t)
			}
		}
		s.demoOrder = open
	}
	s.sessions[token] = session{userID: demoAccount.ID, hash: demoAccount.PasswordHash, expires: now.Add(sessionTTL)}
	s.demoOrder = append(s.demoOrder, token)
	s.sessMu.Unlock()
	http.SetCookie(w, sessionCookie(r, token, int(sessionTTL/time.Second)))
	others := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range others {
		if c.Name != cookieName {
			r.AddCookie(c)
		}
	}
	r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	return demoAccount.user(), true
}
