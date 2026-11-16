package server

import (
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/juki/freeswitch-console/internal/fsapi"
)

// Bounds of one hang-up record, and the rows of an answer by default.
const (
	hangupMaxVars    = 400
	hangupMaxVarSize = 64 << 10 // bytes of names and values
	hangupMaxLog     = 200
	hangupsLimit     = 200
	// FreeSWITCH sends a channel's last log lines after its
	// CHANNEL_HANGUP_COMPLETE; they are attached to the record this long.
	hangupLateLog = 5 * time.Second
)

// hangupRow summarises one channel that ended, from its
// CHANNEL_HANGUP_COMPLETE event.
type hangupRow struct {
	UUID        string  `json:"uuid"`
	T           int64   `json:"t"` // unix ms, server receive time
	Direction   string  `json:"direction"`
	Name        string  `json:"name"`
	CIDName     string  `json:"cidName"`
	CIDNum      string  `json:"cidNum"`
	Dest        string  `json:"dest"`
	Context     string  `json:"context"`
	Cause       string  `json:"cause"`
	Q850        int     `json:"q850"`
	SIPStatus   int     `json:"sipStatus"` // 0 when unknown
	Disposition string  `json:"disposition"`
	Duration    int     `json:"duration"` // seconds
	Billsec     int     `json:"billsec"`
	Answered    bool    `json:"answered"`
	Profile     string  `json:"profile"`
	Gateway     string  `json:"gateway"`
	RemoteIP    string  `json:"remoteIP"`
	UserAgent   string  `json:"userAgent"`
	Codec       string  `json:"codec"`
	OtherUUID   string  `json:"otherUUID"`
	MOS         float64 `json:"mos"`
	Quality     float64 `json:"quality"`
	LogLines    int     `json:"logLines"`
	// OriginateFailure is why a bridge from this leg failed, when that
	// differs from Cause: the stock dialplan goes on to voicemail and the leg
	// ends NORMAL_CLEARING.
	OriginateFailure string `json:"originateFailure"`
}

// hangup is one record. Once stored only row.LogLines and log change, under
// callsMu, while late lines are attached.
type hangup struct {
	row  hangupRow
	vars map[string]string // variables without the "variable_" prefix, and the Caller-* headers
	log  []logEvent        // never nil
}

// recentHangup is a record that late log lines are still attached to.
type recentHangup struct {
	c  *hangup
	at time.Time // when it was stored
}

// newHangup builds the record of the event headers h, received at now, with
// the call's log lines. Values are masked here, so nothing unmasked is kept:
// a variable with a secret-looking name is masked whole, every other value
// gets the inline masking of the dialplan view.
func newHangup(h map[string]string, now time.Time, log []logEvent) *hangup {
	get := func(header string) string {
		return fsapi.MaskValue(strings.TrimPrefix(header, "variable_"), h[header])
	}
	first := func(headers ...string) string {
		for _, k := range headers {
			if v := get(k); v != "" {
				return v
			}
		}
		return ""
	}
	billsec := integer(h["variable_billsec"])
	row := hangupRow{
		UUID:        get("Unique-ID"),
		T:           now.UnixMilli(),
		Direction:   get("Call-Direction"),
		Name:        get("Channel-Name"),
		CIDName:     get("Caller-Caller-ID-Name"),
		CIDNum:      get("Caller-Caller-ID-Number"),
		Dest:        get("Caller-Destination-Number"),
		Context:     get("Caller-Context"),
		Cause:       get("Hangup-Cause"),
		Q850:        integer(h["variable_hangup_cause_q850"]),
		SIPStatus:   sipStatus(h),
		Disposition: get("variable_sip_hangup_disposition"),
		Duration:    integer(h["variable_duration"]),
		Billsec:     billsec,
		Answered:    billsec > 0 || integer(h["variable_answer_epoch"]) > 0,
		Profile:     get("variable_sofia_profile_name"),
		Gateway:     get("variable_sip_gateway_name"),
		RemoteIP:    get("variable_sip_network_ip"),
		UserAgent:   get("variable_sip_user_agent"),
		Codec:       get("variable_read_codec"),
		OtherUUID:   first("Other-Leg-Unique-ID", "variable_bridge_uuid", "variable_signal_bond", "variable_other_loopback_leg_uuid"),
		MOS:         number(h["variable_rtp_audio_in_mos"]),
		Quality:     number(h["variable_rtp_audio_in_quality_percentage"]),
		LogLines:    len(log),
	}
	if v := get("variable_originate_failed_cause"); v != "NONE" && v != row.Cause {
		row.OriginateFailure = v
	}
	return &hangup{row: row, vars: hangupVars(h), log: log}
}

// sipStatus is the SIP response code that ended the call: the first of
// sip_term_status, sip_invite_failure_status and the "sip:486" form of
// proto_specific_hangup_cause and last_bridge_proto_specific_hangup_cause
// that holds one; 0 when none does.
func sipStatus(h map[string]string) int {
	for _, k := range []string{"variable_sip_term_status", "variable_sip_invite_failure_status"} {
		if n := integer(h[k]); n > 0 {
			return n
		}
	}
	for _, k := range []string{"variable_proto_specific_hangup_cause", "variable_last_bridge_proto_specific_hangup_cause"} {
		if code, ok := strings.CutPrefix(h[k], "sip:"); ok {
			if n := integer(code); n > 0 {
				return n
			}
		}
	}
	return 0
}

// hangupVars keeps every variable_* header, without the prefix, and every
// Caller-* header, masked. Names are taken in sorted order; an entry that
// would pass hangupMaxVarSize is dropped, and taking stops at hangupMaxVars.
// A Caller-* header wins over a variable of the same name.
func hangupVars(h map[string]string) map[string]string {
	all := map[string]string{}
	for k, v := range h {
		if name, ok := strings.CutPrefix(k, "variable_"); ok && name != "" {
			all[name] = v
		}
	}
	for k, v := range h {
		if strings.HasPrefix(k, "Caller-") {
			all[k] = v
		}
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make(map[string]string, min(len(names), hangupMaxVars))
	size := 0
	for _, name := range names {
		if len(out) == hangupMaxVars {
			break
		}
		v := fsapi.MaskValue(name, all[name])
		if size+len(name)+len(v) > hangupMaxVarSize {
			continue
		}
		size += len(name) + len(v)
		out[name] = v
	}
	return out
}

func integer(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// number parses a float; NaN and Inf parse but cannot be encoded as JSON.
func number(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// callLog copies the backlog lines of the channel uuid, oldest first; when
// there are more than hangupMaxLog it keeps the last ones, nearest the
// hang-up. Never nil. logMu must be held.
func (i *instance) callLog(uuid string) []logEvent {
	var lines []logEvent
	n := len(i.backlog)
	for k := 0; k < n; k++ {
		if ev := i.backlog[(i.backlogNext+k)%n]; ev.UUID == uuid {
			lines = append(lines, ev)
		}
	}
	return append([]logEvent{}, lines[max(0, len(lines)-hangupMaxLog):]...)
}

// addHangup stores the record of a CHANNEL_HANGUP_COMPLETE event; the
// newest records win. The backlog lines are copied and the channel is
// registered for late lines in one critical section, so no line is both
// copied and attached, and none falls between the two.
func (i *instance) addHangup(h map[string]string) {
	uuid := h["Unique-ID"]
	if i.callsSize == 0 || uuid == "" {
		return
	}
	now := time.Now()
	c := newHangup(h, now, nil)
	i.logMu.Lock()
	c.log = i.callLog(uuid)
	c.row.LogLines = len(c.log)
	for id, r := range i.recent {
		if now.Sub(r.at) > i.lateLog {
			delete(i.recent, id)
		}
	}
	i.recent[uuid] = recentHangup{c: c, at: now}
	i.logMu.Unlock()

	i.callsMu.Lock()
	defer i.callsMu.Unlock()
	if len(i.calls) < i.callsSize {
		i.calls = append(i.calls, c)
		return
	}
	i.calls[i.callsNext] = c
	i.callsNext = (i.callsNext + 1) % i.callsSize
}

// hangups returns at most limit rows, newest first; never nil.
func (i *instance) hangups(limit int) []hangupRow {
	i.callsMu.Lock()
	defer i.callsMu.Unlock()
	n := len(i.calls)
	out := make([]hangupRow, 0, min(limit, n))
	for k := 1; k <= n && len(out) < limit; k++ {
		out = append(out, i.calls[(i.callsNext-k+n)%n].row)
	}
	return out
}

// hangup returns a copy of the newest record of the channel uuid, or nil.
func (i *instance) hangup(uuid string) *hangup {
	i.callsMu.Lock()
	defer i.callsMu.Unlock()
	n := len(i.calls)
	for k := 1; k <= n; k++ {
		if c := i.calls[(i.callsNext-k+n)%n]; c.row.UUID == uuid {
			return &hangup{row: c.row, vars: c.vars, log: append([]logEvent{}, c.log...)}
		}
	}
	return nil
}

// attachLate appends a backlog line to the record of its channel when that
// hung up at most lateLog ago. logMu must be held; callsMu is taken inside.
func (i *instance) attachLate(ev logEvent) {
	if ev.UUID == "" || len(i.recent) == 0 {
		return
	}
	r, ok := i.recent[ev.UUID]
	if !ok {
		return
	}
	if time.Since(r.at) > i.lateLog {
		delete(i.recent, ev.UUID)
		return
	}
	i.callsMu.Lock()
	defer i.callsMu.Unlock()
	r.c.log = append(r.c.log, ev)
	if len(r.c.log) > hangupMaxLog {
		r.c.log = r.c.log[len(r.c.log)-hangupMaxLog:]
	}
	r.c.row.LogLines = len(r.c.log)
}

// handleHangups answers while disconnected too: the records are local state.
func (s *Server) handleHangups(w http.ResponseWriter, r *http.Request, _ user) {
	limit := hangupsLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = n
	}
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Enabled  bool        `json:"enabled"`
		Capacity int         `json:"capacity"`
		Rows     []hangupRow `json:"rows"`
	}{inst.callsSize > 0, inst.callsSize, inst.hangups(min(limit, inst.callsSize))})
}

// handleHangupDetail sends the call's log lines only to a role with capLogs; for
// the others log is null.
func (s *Server) handleHangupDetail(w http.ResponseWriter, r *http.Request, u user) {
	uuid := r.PathValue("uuid")
	if !fsapi.ValidUUID(uuid) {
		writeError(w, http.StatusBadRequest, "invalid channel uuid")
		return
	}
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	c := inst.hangup(uuid)
	if c == nil {
		writeError(w, http.StatusNotFound, "hang-up not found")
		return
	}
	resp := struct {
		Row  hangupRow         `json:"row"`
		Vars map[string]string `json:"vars"`
		Log  []logEvent        `json:"log"`
	}{Row: c.row, Vars: c.vars}
	if allowed(u.Role, capLogs) {
		resp.Log = c.log
	}
	writeJSON(w, http.StatusOK, resp)
}
