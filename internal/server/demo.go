package server

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/juki/freeswitch-console/internal/store"
)

// In demo mode (Config.Demo) nobody signs in: a request without a valid
// session gets one for the built-in account demo, an admin. At most
// demoMaxSessions are open at once; a new one beyond that ends the oldest,
// whose visitor gets a new one on the next request. Changes to accounts,
// instances, files and the alert settings, and webhook tests, are refused
// with 403. The alert history starts with a few incidents of the hours
// before (demoAlertHistory).

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
	"PUT /api/alerts/settings":                 true,
	"POST /api/alerts/test":                    true,
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

// demoIncidents are the alerts the demo history starts with: what happened
// on the simulated switches in the hours before the start, by instance
// name. ago is when the condition began, lasted how long it held; fired and
// resolved carry the details of the two entries. Each fired after the
// default 60 s.
var demoIncidents = []struct {
	instance, rule, subject string
	ago, lasted             time.Duration
	fired, resolved         alertView
}{
	{"pbx-west-2", ruleGatewayDown, "carrier-a", 6*time.Hour + 41*time.Minute, 17*time.Minute + 24*time.Second,
		alertView{State: "FAIL_WAIT", Ping: "DOWN"}, alertView{State: "REGED", Ping: "UP"}},
	{"pbx-east-1", ruleIdleCPULow, "", 4*time.Hour + 6*time.Minute, 11*time.Minute + 40*time.Second,
		alertView{Value: 6.4, Threshold: 10}, alertView{Value: 38.5, Threshold: 10}},
	{"pbx-west-2", ruleInstanceDown, "", 2*time.Hour + 32*time.Minute, 2*time.Minute + 51*time.Second,
		alertView{Reason: "esl: no answer from FreeSWITCH"}, alertView{}},
}

// demoAlertHistory fills the history with demoIncidents, before the
// evaluator adds what it finds at its first evaluation.
func (s *Server) demoAlertHistory(now time.Time) {
	ids := map[string]string{}
	for _, in := range s.cfg.Store.List() {
		ids[in.Name] = in.ID
	}
	var entries []*alertEntry
	pairs := map[*alertEntry]*alertEntry{} // firing -> resolved
	for _, inc := range demoIncidents {
		id, ok := ids[inc.instance]
		if !ok {
			continue
		}
		since := now.Add(-inc.ago)
		fired := since.Add(time.Minute)
		entry := func(v alertView, event string, at time.Time) *alertEntry {
			v.Rule, v.InstanceID, v.Instance, v.Subject = inc.rule, id, inc.instance, inc.subject
			v.Since, v.FiredAt = since.UnixMilli(), fired.UnixMilli()
			return &alertEntry{alertView: v, T: at.UnixMilli(), Event: event, Deliveries: []alertDelivery{}}
		}
		f, r := entry(inc.fired, "firing", fired), entry(inc.resolved, "resolved", fired.Add(inc.lasted))
		r.DurationMs = inc.lasted.Milliseconds()
		pairs[f] = r
		entries = append(entries, f, r)
	}
	slices.SortFunc(entries, func(a, b *alertEntry) int { return cmp.Compare(a.T, b.T) })
	s.alertMu.Lock()
	defer s.alertMu.Unlock()
	for _, e := range entries {
		s.alertSeq++
		e.Seq = s.alertSeq
		if r := pairs[e]; r != nil {
			e.ID = strconv.FormatUint(e.Seq, 10)
			r.ID = e.ID
		}
		s.pushAlertEntry(e)
	}
}
