package server

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/juki/freeswitch-console/internal/fsapi"
)

// SipSubclasses are the mod_sofia event subclasses kept as SIP event
// records, subscribed after the word CUSTOM. Each record's kind is the
// subclass without "sofia::". FreeSWITCH (mod_sofia.h, sofia_reg.c) fires:
//   - register: a REGISTER accepted (also each refresh)
//   - unregister: a REGISTER with expires 0, or the TCP/TLS connection of a
//     registration dropped (then only user, host, call-id and reason)
//   - expire: a registration that ran out and was purged
//   - register_failure: a REGISTER or INVITE answered 403 because the
//     password was wrong or the user unknown; a 401 challenge fires nothing
//   - gateway_state: an outbound registration changed state, or its ping
//     status changed
//   - gateway_add, gateway_delete: a gateway was created (start, rescan) or
//     removed (killgw, rescan); a gateway with register="false" fires only
//     these, never gateway_state
var SipSubclasses = []string{
	"sofia::register", "sofia::unregister", "sofia::expire", "sofia::register_failure",
	"sofia::gateway_state", "sofia::gateway_add", "sofia::gateway_delete",
}

// Bounds of one SIP event record, and of the answers. A register_failure
// carries values chosen by whoever sent the request, so every string of a
// record is cut.
const (
	sipEventMaxHeaders = 60
	sipEventMaxValue   = 256  // characters of a row field or header value
	sipEventMaxName    = 64   // characters of a header name; longer ones are dropped
	sipEventMaxBytes   = 4096 // header names plus values of one record
	sipEventsLimit     = 300
	sipFailuresMinutes = 60
	sipFailuresMax     = 1440 // minutes
	sipFailuresRows    = 50
	sipFailuresUsers   = 10
)

// sipKind returns the record kind of a mod_sofia subclass kept, or "".
func sipKind(subclass string) string {
	if slices.Contains(SipSubclasses, subclass) {
		return strings.TrimPrefix(subclass, "sofia::")
	}
	return ""
}

// sipEventRow summarises one SIP event. Fields an event does not carry are
// empty or 0.
type sipEventRow struct {
	Seq        uint64 `json:"seq"` // per instance, from 1
	T          int64  `json:"t"`   // unix ms, server receive time
	Kind       string `json:"kind"`
	Profile    string `json:"profile"`
	User       string `json:"user"`
	Domain     string `json:"domain"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	UserAgent  string `json:"userAgent"`
	Contact    string `json:"contact"`
	Expires    int    `json:"expires"` // seconds
	Gateway    string `json:"gateway"`
	State      string `json:"state"`
	PingStatus string `json:"pingStatus"`
	Status     int    `json:"status"` // SIP status
	Phrase     string `json:"phrase"`
	Method     string `json:"method"` // register_failure: REGISTER or INVITE
}

// sipEvent is one record; immutable once stored.
type sipEvent struct {
	row     sipEventRow
	headers map[string]string
}

// newSipEvent builds the record of the event headers h of the given kind,
// received at now, masked as the hang-up variables are, then cut to
// sipEventMaxValue. The header names are those mod_sofia uses: the
// registration events name the user from-user (register, unregister),
// to-user (register_failure) or user (expire), and the gateway events use
// capitalised names.
func newSipEvent(kind string, h map[string]string, now time.Time) *sipEvent {
	first := func(headers ...string) string {
		for _, k := range headers {
			if v := fsapi.MaskValue(k, h[k]); v != "" {
				return cutValue(v)
			}
		}
		return ""
	}
	row := sipEventRow{
		T:          now.UnixMilli(),
		Kind:       kind,
		Profile:    first("profile-name"),
		User:       first("from-user", "to-user", "user"),
		Domain:     first("from-host", "to-host", "host"),
		IP:         first("network-ip", "Register-Network-IP"),
		Port:       integer(first("network-port", "Register-Network-Port")),
		UserAgent:  first("user-agent"),
		Contact:    first("contact"),
		Gateway:    first("Gateway"),
		State:      first("State"),
		PingStatus: first("Ping-Status"),
		Status:     integer(h["Status"]),
		Phrase:     first("Phrase"),
	}
	// The expires of an expire event is the unix time the registration ran
	// out, not a number of seconds.
	if kind != "expire" {
		row.Expires = integer(h["expires"])
	}
	// mod_sofia fires register_failure for a failed INVITE too.
	if kind == "register_failure" {
		row.Method = first("registration-type")
	}
	return &sipEvent{row: row, headers: sipHeaders(h)}
}

// cutValue returns at most the first sipEventMaxValue characters of s, cut
// on a character boundary.
func cutValue(s string) string {
	n := 0
	for i := range s {
		if n == sipEventMaxValue {
			return s[:i]
		}
		n++
	}
	return s
}

// sipHeaders keeps the event's own headers, masked and cut: not the Event-*,
// Core-* and FreeSWITCH-* ones every event carries, nor a name longer than
// sipEventMaxName. Names are taken in sorted order, at most
// sipEventMaxHeaders; one that would take the names and values past
// sipEventMaxBytes is skipped.
func sipHeaders(h map[string]string) map[string]string {
	var names []string
	for k := range h {
		if !strings.HasPrefix(k, "Event-") && !strings.HasPrefix(k, "Core-") && !strings.HasPrefix(k, "FreeSWITCH-") &&
			utf8.RuneCountInString(k) <= sipEventMaxName {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	out := map[string]string{}
	size := 0
	for _, k := range names {
		v := cutValue(fsapi.MaskValue(k, h[k]))
		if size+len(k)+len(v) > sipEventMaxBytes {
			continue
		}
		out[k] = v
		size += len(k) + len(v)
		if len(out) == sipEventMaxHeaders {
			break
		}
	}
	return out
}

// pastSipEvents is a Switch that knows SIP events from before it was
// dialled: a simulated switch of demo mode (demo.Switch.SipEvents). add gets
// them oldest first, with the subclass in Event-Subclass.
type pastSipEvents interface {
	SipEvents(add func(at time.Time, headers map[string]string))
}

// pastSipEvent is one of them.
type pastSipEvent struct {
	at      time.Time
	headers map[string]string
}

// addSipEvent stores the record of a CUSTOM event of a subclass kept and
// counts it for the metrics.
func (i *instance) addSipEvent(h map[string]string, now time.Time) {
	kind := sipKind(h["Event-Subclass"])
	if i.sipSize == 0 || kind == "" {
		return
	}
	e := newSipEvent(kind, h, now)
	i.sipMu.Lock()
	defer i.sipMu.Unlock()
	i.storeSipEvent(e)
	i.sipCounts[kind]++
}

// storeSipEvent numbers e and puts it in the ring; the newest records win.
// sipMu must be held.
func (i *instance) storeSipEvent(e *sipEvent) {
	i.sipSeq++
	e.row.Seq = i.sipSeq
	if len(i.sipEvents) < i.sipSize {
		i.sipEvents = append(i.sipEvents, e)
		return
	}
	i.sipEvents[i.sipNext] = e
	i.sipNext = (i.sipNext + 1) % i.sipSize
}

// eachSipEvent calls fn with the records newest first until it returns
// false. sipMu must be held.
func (i *instance) eachSipEvent(fn func(e *sipEvent) bool) {
	n := len(i.sipEvents)
	for k := 1; k <= n; k++ {
		if !fn(i.sipEvents[(i.sipNext-k+n)%n]) {
			return
		}
	}
}

// sipRows returns at most limit rows of the given kinds (all when kinds is
// empty), newest first; never nil.
func (i *instance) sipRows(limit int, kinds []string) []sipEventRow {
	i.sipMu.Lock()
	defer i.sipMu.Unlock()
	out := []sipEventRow{}
	i.eachSipEvent(func(e *sipEvent) bool {
		if len(kinds) == 0 || slices.Contains(kinds, e.row.Kind) {
			out = append(out, e.row)
		}
		return len(out) < limit
	})
	return out
}

// sipEvent returns the record seq, or nil. Records are immutable, so it is
// shared.
func (i *instance) sipEvent(seq uint64) *sipEvent {
	i.sipMu.Lock()
	defer i.sipMu.Unlock()
	var found *sipEvent
	i.eachSipEvent(func(e *sipEvent) bool {
		if e.row.Seq == seq {
			found = e
		}
		return found == nil && e.row.Seq > seq
	})
	return found
}

// sipFailure is the failed registrations from one source address.
type sipFailure struct {
	IP        string   `json:"ip"`
	Attempts  int      `json:"attempts"`
	Users     []string `json:"users"`     // the most frequent first, at most sipFailuresUsers
	UserAgent string   `json:"userAgent"` // of the last attempt
	Last      int64    `json:"last"`      // unix ms of the last attempt
}

// sipFailures groups the register_failure records received since since by
// source address: most attempts first (then the latest, then the address),
// at most sipFailuresRows; never nil.
func (i *instance) sipFailures(since time.Time) []sipFailure {
	type source struct {
		sipFailure
		users map[string]int
	}
	by := map[string]*source{}
	from := since.UnixMilli()
	i.sipMu.Lock()
	i.eachSipEvent(func(e *sipEvent) bool {
		if e.row.T < from {
			return false
		}
		if e.row.Kind != "register_failure" {
			return true
		}
		s := by[e.row.IP]
		if s == nil {
			// Newest first: the first record seen is the last attempt.
			s = &source{sipFailure: sipFailure{IP: e.row.IP, UserAgent: e.row.UserAgent, Last: e.row.T}, users: map[string]int{}}
			by[e.row.IP] = s
		}
		s.Attempts++
		s.users[e.row.User]++
		return true
	})
	i.sipMu.Unlock()

	out := []sipFailure{}
	for _, s := range by {
		for u := range s.users {
			s.Users = append(s.Users, u)
		}
		slices.SortFunc(s.Users, func(a, b string) int {
			return cmp.Or(cmp.Compare(s.users[b], s.users[a]), cmp.Compare(a, b))
		})
		s.Users = s.Users[:min(len(s.Users), sipFailuresUsers)]
		out = append(out, s.sipFailure)
	}
	slices.SortFunc(out, func(a, b sipFailure) int {
		return cmp.Or(cmp.Compare(b.Attempts, a.Attempts), cmp.Compare(b.Last, a.Last), cmp.Compare(a.IP, b.IP))
	})
	return out[:min(len(out), sipFailuresRows)]
}

// handleSipEvents answers while disconnected too: the records are local
// state.
func (s *Server) handleSipEvents(w http.ResponseWriter, r *http.Request, _ user) {
	q := r.URL.Query()
	limit := sipEventsLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = n
	}
	var kinds []string
	if v := q.Get("kind"); v != "" {
		kinds = strings.Split(v, ",")
		for _, k := range kinds {
			if sipKind("sofia::"+k) == "" {
				writeError(w, http.StatusBadRequest, "unknown kind "+strconv.Quote(k))
				return
			}
		}
	}
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Enabled  bool          `json:"enabled"`
		Capacity int           `json:"capacity"`
		Rows     []sipEventRow `json:"rows"`
	}{inst.sipSize > 0, inst.sipSize, inst.sipRows(min(limit, inst.sipSize), kinds)})
}

func (s *Server) handleSipFailures(w http.ResponseWriter, r *http.Request, _ user) {
	minutes := sipFailuresMinutes
	if v := r.URL.Query().Get("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > sipFailuresMax {
			writeError(w, http.StatusBadRequest, "minutes must be 1 to 1440")
			return
		}
		minutes = n
	}
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Minutes int          `json:"minutes"`
		Rows    []sipFailure `json:"rows"`
	}{minutes, inst.sipFailures(time.Now().Add(-time.Duration(minutes) * time.Minute))})
}

func (s *Server) handleSipEventDetail(w http.ResponseWriter, r *http.Request, _ user) {
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil || seq == 0 {
		writeError(w, http.StatusBadRequest, "invalid sequence number")
		return
	}
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	e := inst.sipEvent(seq)
	if e == nil {
		writeError(w, http.StatusNotFound, "SIP event not found")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Row     sipEventRow       `json:"row"`
		Headers map[string]string `json:"headers"`
	}{e.row, e.headers})
}
