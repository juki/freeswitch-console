package server

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/juki/freeswitch-console/internal/esl"
	"github.com/juki/freeswitch-console/internal/fsapi"
	"github.com/juki/freeswitch-console/internal/store"
)

func withRecentCalls(n int) func(*Config) {
	return func(c *Config) { c.RecentCalls = n }
}

// hangupEvent is a CHANNEL_HANGUP_COMPLETE as FreeSWITCH 1.11.3 sends it for
// a SIP call that got a 486, trimmed; extra adds or replaces headers.
func hangupEvent(uuid string, extra ...string) esl.Message {
	h := map[string]string{
		"Event-Name":                               "CHANNEL_HANGUP_COMPLETE",
		"Core-UUID":                                "b0d7b8a6-0000-4000-8000-000000000000",
		"Unique-ID":                                uuid,
		"Call-Direction":                           "outbound",
		"Channel-Name":                             "sofia/external/1002@192.0.2.7",
		"Channel-State":                            "CS_REPORTING",
		"Answer-State":                             "hangup",
		"Hangup-Cause":                             "USER_BUSY",
		"Caller-Caller-ID-Name":                    "Alice",
		"Caller-Caller-ID-Number":                  "1001",
		"Caller-Destination-Number":                "1002",
		"Caller-Context":                           "default",
		"Caller-Unique-ID":                         uuid,
		"Other-Leg-Unique-ID":                      "",
		"variable_uuid":                            uuid,
		"variable_hangup_cause":                    "USER_BUSY",
		"variable_hangup_cause_q850":               "17",
		"variable_sip_term_status":                 "486",
		"variable_proto_specific_hangup_cause":     "sip:486",
		"variable_sip_hangup_disposition":          "recv_refuse",
		"variable_duration":                        "4",
		"variable_billsec":                         "0",
		"variable_answer_epoch":                    "0",
		"variable_sofia_profile_name":              "external",
		"variable_sip_gateway_name":                "carrier1",
		"variable_sip_network_ip":                  "192.0.2.7",
		"variable_sip_user_agent":                  "Carrier SBC 2.1",
		"variable_read_codec":                      "PCMA",
		"variable_bridge_uuid":                     "",
		"variable_signal_bond":                     "a1b2c3d4-0000-4000-8000-000000000000",
		"variable_rtp_audio_in_mos":                "4.50",
		"variable_rtp_audio_in_quality_percentage": "100.00",
		"variable_sip_auth_password":               "gw-S3cret",
		"variable_originate_string":                "{sip_auth_username=u,sip_auth_password=gw-S3cret}sofia/gateway/carrier1/1002",
		"variable_cred":                            "c-S3cret",
	}
	for k := 0; k+1 < len(extra); k += 2 {
		h[extra[k]] = extra[k+1]
	}
	return esl.Message{Kind: esl.KindEvent, Headers: h}
}

func TestNewHangup(t *testing.T) {
	now := time.UnixMilli(1794793998813)
	log := []logEvent{{Level: 6, Text: "a", Seq: 1, UUID: "u-1"}}
	c := newHangup(hangupEvent("u-1").Headers, now, log)
	want := hangupRow{
		UUID: "u-1", T: 1794793998813, Direction: "outbound", Name: "sofia/external/1002@192.0.2.7",
		CIDName: "Alice", CIDNum: "1001", Dest: "1002", Context: "default",
		Cause: "USER_BUSY", Q850: 17, SIPStatus: 486, Disposition: "recv_refuse",
		Duration: 4, Billsec: 0, Answered: false,
		Profile: "external", Gateway: "carrier1", RemoteIP: "192.0.2.7", UserAgent: "Carrier SBC 2.1", Codec: "PCMA",
		OtherUUID: "a1b2c3d4-0000-4000-8000-000000000000", MOS: 4.5, Quality: 100, LogLines: 1,
	}
	if c.row != want {
		t.Fatalf("row\n got %+v\nwant %+v", c.row, want)
	}
	if !reflect.DeepEqual(c.log, log) {
		t.Fatalf("log %+v", c.log)
	}
	// Variables lose the prefix, Caller-* headers are kept, other headers
	// are not; secrets are masked whole or inline.
	for name, v := range map[string]string{
		"hangup_cause":              "USER_BUSY",
		"Caller-Caller-ID-Name":     "Alice",
		"Caller-Unique-ID":          "u-1",
		"sip_auth_password":         fsapi.Mask,
		"cred":                      fsapi.Mask,
		"originate_string":          "{sip_auth_username=u,sip_auth_password=" + fsapi.Mask + "}sofia/gateway/carrier1/1002",
		"rtp_audio_in_mos":          "4.50",
		"sip_hangup_disposition":    "recv_refuse",
		"sip_term_status":           "486",
		"Caller-Destination-Number": "1002",
	} {
		if got, ok := c.vars[name]; !ok || got != v {
			t.Errorf("vars[%s] = %q %v, want %q", name, got, ok, v)
		}
	}
	for _, name := range []string{"Event-Name", "Unique-ID", "Channel-Name", "variable_uuid", "Other-Leg-Unique-ID"} {
		if _, ok := c.vars[name]; ok {
			t.Errorf("vars has %s", name)
		}
	}
	if b, _ := json.Marshal(c.vars); strings.Contains(string(b), "S3cret") {
		t.Fatalf("vars leak a secret: %s", b)
	}

	// Answered by billsec or by answer_epoch; the other-leg fallbacks; bad
	// numbers count as 0.
	for _, tc := range []struct {
		extra    []string
		answered bool
		other    string
	}{
		{[]string{"variable_billsec", "3"}, true, "a1b2c3d4-0000-4000-8000-000000000000"},
		{[]string{"variable_answer_epoch", "1794793989", "variable_bridge_uuid", "b-2"}, true, "b-2"},
		{[]string{"Other-Leg-Unique-ID", "o-3", "variable_bridge_uuid", "b-2"}, false, "o-3"},
		{[]string{"variable_billsec", "x", "variable_answer_epoch", "", "variable_signal_bond", ""}, false, ""},
		{[]string{"variable_signal_bond", "", "variable_other_loopback_leg_uuid", "l-4"}, false, "l-4"},
		{[]string{"variable_other_loopback_leg_uuid", "l-4"}, false, "a1b2c3d4-0000-4000-8000-000000000000"},
	} {
		r := newHangup(hangupEvent("u", tc.extra...).Headers, now, nil).row
		if r.Answered != tc.answered || r.OtherUUID != tc.other {
			t.Errorf("%q: answered %v other %q", tc.extra, r.Answered, r.OtherUUID)
		}
	}
	r := newHangup(hangupEvent("u", "variable_rtp_audio_in_mos", "NaN", "variable_rtp_audio_in_quality_percentage", "x", "variable_hangup_cause_q850", "").Headers, now, nil).row
	if r.MOS != 0 || r.Quality != 0 || r.Q850 != 0 {
		t.Errorf("bad numbers: %+v", r)
	}
	// The originate failure, unless it is the leg's own cause or NONE.
	for _, tc := range []struct {
		extra []string
		want  string
	}{
		{nil, ""},
		{[]string{"Hangup-Cause", "NORMAL_CLEARING", "variable_originate_failed_cause", "USER_NOT_REGISTERED"}, "USER_NOT_REGISTERED"},
		{[]string{"variable_originate_failed_cause", "USER_BUSY"}, ""},
		{[]string{"Hangup-Cause", "NORMAL_CLEARING", "variable_originate_failed_cause", "NONE"}, ""},
		{[]string{"Hangup-Cause", "NORMAL_CLEARING", "variable_originate_failed_cause", ""}, ""},
	} {
		if r := newHangup(hangupEvent("u", tc.extra...).Headers, now, nil).row; r.OriginateFailure != tc.want {
			t.Errorf("%q: originate failure %q, want %q", tc.extra, r.OriginateFailure, tc.want)
		}
	}
	if b, _ := json.Marshal(c.row); !strings.Contains(string(b), `"originateFailure":""`) {
		t.Errorf("row JSON %s", b)
	}
	// A summary value gets the inline masking too.
	r = newHangup(hangupEvent("u", "Caller-Destination-Number", "pin=4321").Headers, now, nil).row
	if r.Dest != "pin="+fsapi.Mask {
		t.Errorf("dest %q", r.Dest)
	}
}

func TestSIPStatus(t *testing.T) {
	for _, tc := range []struct {
		h    map[string]string
		want int
	}{
		{map[string]string{}, 0},
		{map[string]string{"variable_sip_term_status": "200", "variable_sip_invite_failure_status": "486"}, 200},
		{map[string]string{"variable_sip_term_status": "", "variable_sip_invite_failure_status": "487"}, 487},
		{map[string]string{"variable_proto_specific_hangup_cause": "sip:503", "variable_last_bridge_proto_specific_hangup_cause": "sip:486"}, 503},
		{map[string]string{"variable_last_bridge_proto_specific_hangup_cause": "sip:486"}, 486},
		{map[string]string{"variable_sip_term_status": "x", "variable_proto_specific_hangup_cause": "sip:404"}, 404},
		{map[string]string{"variable_proto_specific_hangup_cause": "q850:17"}, 0},
		{map[string]string{"variable_proto_specific_hangup_cause": "sip:"}, 0},
	} {
		if got := sipStatus(tc.h); got != tc.want {
			t.Errorf("%v: %d, want %d", tc.h, got, tc.want)
		}
	}
}

func TestHangupVarsBounds(t *testing.T) {
	// More than hangupMaxVars: the first ones in sorted order are kept.
	h := map[string]string{}
	for n := 0; n < 450; n++ {
		h[fmt.Sprintf("variable_v%03d", n)] = "x"
	}
	vars := hangupVars(h)
	if len(vars) != hangupMaxVars {
		t.Fatalf("%d vars", len(vars))
	}
	if _, ok := vars["v399"]; !ok {
		t.Fatal("v399 dropped")
	}
	if _, ok := vars["v400"]; ok {
		t.Fatal("v400 kept")
	}

	// More than hangupMaxVarSize: an entry that does not fit is dropped, a
	// later smaller one still fits. The same input always keeps the same.
	big := strings.Repeat("b", 30<<10)
	h = map[string]string{"variable_a": big, "variable_b": big, "variable_c": big, "variable_d": "small", "Caller-Context": "default"}
	vars = hangupVars(h)
	size := 0
	var names []string
	for k, v := range vars {
		size += len(k) + len(v)
		names = append(names, k)
	}
	slices.Sort(names)
	if size > hangupMaxVarSize || strings.Join(names, " ") != "Caller-Context a b d" || vars["b"] != big || vars["d"] != "small" {
		t.Fatalf("kept %q, %d bytes", names, size)
	}
	for range 20 {
		if again := hangupVars(h); !reflect.DeepEqual(again, vars) {
			t.Fatal("not deterministic")
		}
	}
}

func userLog(level, text, uuid string) esl.Message {
	m := logMsg(level, text)
	m.Headers["User-Data"] = uuid
	return m
}

func TestCallLog(t *testing.T) {
	h := newHarness(t, withBacklog(1000, 7), withRecentCalls(5))
	f := h.fake(0)
	inst := h.instance(h.id)
	for n := 1; n <= 250; n++ {
		f.publish(userLog("6", fmt.Sprint("line ", n), "u-1"))
		f.publish(userLog("6", "other", "u-2"))
	}
	f.publish(logMsg("6", "no uuid"))
	waitFor(t, "stored lines", func() bool { return inst.lastSeq() == 501 })
	inst.logMu.Lock()
	lines, none := inst.callLog("u-1"), inst.callLog("u-3")
	inst.logMu.Unlock()
	if len(lines) != hangupMaxLog || lines[0].Text != "line 51" || lines[199].Text != "line 250" || lines[0].UUID != "u-1" {
		t.Fatalf("%d lines, %+v .. %+v", len(lines), lines[0], lines[len(lines)-1])
	}
	if none == nil || len(none) != 0 {
		t.Fatalf("no lines: %#v", none)
	}
}

func (i *instance) setLateLog(d time.Duration) {
	i.logMu.Lock()
	defer i.logMu.Unlock()
	i.lateLog = d
}

func (i *instance) recentUUIDs() string {
	i.logMu.Lock()
	defer i.logMu.Unlock()
	var out []string
	for uuid := range i.recent {
		out = append(out, uuid)
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

func logTexts(lines []logEvent) string {
	var out []string
	for _, ev := range lines {
		out = append(out, ev.Text)
	}
	return strings.Join(out, " ")
}

// TestHangupLateLog: FreeSWITCH sends a channel's last lines after its
// CHANNEL_HANGUP_COMPLETE; the lines kept in the backlog within lateLog of
// the hang-up are attached to the record and counted.
func TestHangupLateLog(t *testing.T) {
	h := newHarness(t, withBacklog(1000, 6), withRecentCalls(10))
	f := h.fake(0)
	inst := h.instance(h.id)
	admin := h.login("admin", adminPW)
	row := func(uuid string) hangupRow {
		t.Helper()
		for _, r := range h.hangups(admin, "").Rows {
			if r.UUID == uuid {
				return r
			}
		}
		t.Fatalf("no record of %s", uuid)
		return hangupRow{}
	}
	seq := inst.lastSeq()
	// publish waits until the collector has stored the log lines, and so
	// handled every message before them.
	publish := func(msgs ...esl.Message) {
		t.Helper()
		for _, m := range msgs {
			if m.Kind == esl.KindLog && m.Headers["Log-Level"] != "7" {
				seq++
			}
			f.publish(m)
		}
		waitFor(t, "stored lines", func() bool { return inst.lastSeq() == seq })
	}

	// A late line is attached; one of another channel, one without a uuid
	// and one not kept in the backlog are not.
	publish(userLog("6", "before", "call-1"), hangupEvent("call-1"), userLog("6", "after", "call-1"),
		userLog("6", "other", "call-2"), logMsg("6", "no uuid"), userLog("7", "debug, not kept", "call-1"))
	if got := logTexts(inst.hangup("call-1").log); got != "before after" {
		t.Fatalf("call-1 log %q", got)
	}
	if got := row("call-1").LogLines; got != 2 {
		t.Fatalf("call-1 logLines %d", got)
	}

	// A line after the window is not attached, and the lookup drops the
	// expired channel.
	inst.setLateLog(200 * time.Millisecond)
	publish(hangupEvent("call-3"), userLog("6", "in time", "call-3"))
	time.Sleep(300 * time.Millisecond)
	publish(userLog("6", "too late", "call-3"))
	if got := logTexts(inst.hangup("call-3").log); got != "in time" || row("call-3").LogLines != 1 {
		t.Fatalf("call-3 log %q", got)
	}
	if got := inst.recentUUIDs(); got != "call-1" {
		t.Fatalf("recent after lookup %q", got)
	}
	// A new hang-up drops the expired ones.
	publish(hangupEvent("call-4"), logMsg("6", "sync"))
	if got := inst.recentUUIDs(); got != "call-4" {
		t.Fatalf("recent after hang-up %q", got)
	}

	// The cap holds across the late lines: the newest hangupMaxLog, oldest
	// first.
	inst.setLateLog(hangupLateLog)
	var msgs []esl.Message
	for n := 1; n <= 250; n++ {
		if n == 151 {
			msgs = append(msgs, hangupEvent("call-5"))
		}
		msgs = append(msgs, userLog("6", fmt.Sprint("line ", n), "call-5"))
	}
	publish(msgs...)
	lines := inst.hangup("call-5").log
	if len(lines) != hangupMaxLog || lines[0].Text != "line 51" || lines[len(lines)-1].Text != "line 250" || row("call-5").LogLines != hangupMaxLog {
		t.Fatalf("%d lines, %+v .. %+v", len(lines), lines[0], lines[len(lines)-1])
	}

	// Reads while late lines are attached; the race detector checks them.
	publish(hangupEvent("call-6"), logMsg("6", "sync"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 0; n < 300; n++ {
			f.publish(userLog("6", "race", "call-6"))
		}
	}()
	for reading := true; reading; {
		select {
		case <-done:
			reading = false
		default:
		}
		expect(t, h.do("GET", "/api/instances/"+h.id+"/hangups/call-6", admin, ""), 200, "")
		h.hangups(admin, "")
	}
	seq += 300
	waitFor(t, "race lines", func() bool { return inst.lastSeq() == seq })
	if got := row("call-6").LogLines; got != hangupMaxLog {
		t.Fatalf("call-6 logLines %d", got)
	}
}

// TestHangupsUnderLogFlood: log lines that overflow the collector's buffer
// are dropped, the hang-ups sent in between are all recorded.
func TestHangupsUnderLogFlood(t *testing.T) {
	const lines, calls = 3000, 30
	h := newHarness(t, withBacklog(lines, 6), withRecentCalls(calls))
	f := h.fake(0)
	inst := h.instance(h.id)
	viewer := h.login("viewer", viewerPW)

	// The collector waits for logMu on the first message while the rest
	// pile up behind it.
	inst.logMu.Lock()
	var want []string
	for n := 1; n <= lines; n++ {
		f.send(logMsg("6", fmt.Sprint("line ", n)))
		if n%(lines/calls) == 0 {
			id := fmt.Sprint("call-", n/(lines/calls))
			f.send(hangupEvent(id))
			want = append([]string{id}, want...)
		}
	}
	inst.logMu.Unlock()

	waitFor(t, "every record", func() bool { return len(h.hangups(viewer, "").Rows) == calls })
	if got := rowUUIDs(h.hangups(viewer, "").Rows); got != strings.Join(want, " ") {
		t.Fatalf("rows %s", got)
	}
	// One message in hand and a full buffer of lines: the rest were dropped.
	if n := inst.lastSeq(); n > 1025 {
		t.Fatalf("%d of %d lines stored: the flood did not overflow the buffer", n, lines)
	}
}

// hangupsReply is the answer of GET .../hangups.
type hangupsReply struct {
	Enabled  bool        `json:"enabled"`
	Capacity int         `json:"capacity"`
	Rows     []hangupRow `json:"rows"`
}

func (h *harness) hangups(token, query string) hangupsReply {
	h.t.Helper()
	r := h.do("GET", "/api/instances/"+h.id+"/hangups"+query, token, "")
	expect(h.t, r, 200, "")
	var out hangupsReply
	r.json(h.t, &out)
	return out
}

func rowUUIDs(rows []hangupRow) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.UUID)
	}
	return strings.Join(out, " ")
}

func TestHangupRoutes(t *testing.T) {
	h := newHarness(t, withBacklog(100, 6), withRecentCalls(3))
	viewer := h.login("viewer", viewerPW)
	f := h.fake(0)
	p := "/api/instances/" + h.id + "/hangups"

	if got := h.hangups(viewer, ""); !got.Enabled || got.Capacity != 3 || got.Rows == nil || len(got.Rows) != 0 {
		t.Fatalf("empty %+v", got)
	}
	f.publish(userLog("6", "dialing", "call-1"))
	f.publish(userLog("7", "debug, not kept", "call-1"))
	for n := 1; n <= 5; n++ {
		f.publish(hangupEvent(fmt.Sprint("call-", n)))
	}
	f.publish(hangupEvent("")) // no Unique-ID: not stored
	waitFor(t, "five records", func() bool { return strings.HasPrefix(rowUUIDs(h.hangups(viewer, "").Rows), "call-5") })

	// The ring keeps the last three, newest first; limit cuts and is capped
	// at the capacity.
	if got := rowUUIDs(h.hangups(viewer, "").Rows); got != "call-5 call-4 call-3" {
		t.Fatalf("rows %s", got)
	}
	if got := rowUUIDs(h.hangups(viewer, "?limit=2").Rows); got != "call-5 call-4" {
		t.Fatalf("limit=2 %s", got)
	}
	if got := rowUUIDs(h.hangups(viewer, "?limit=1000").Rows); got != "call-5 call-4 call-3" {
		t.Fatalf("limit=1000 %s", got)
	}
	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=x", "?limit=1.5"} {
		expect(t, h.do("GET", p+q, viewer, ""), 400, `{"error":"limit must be a positive number"}`)
	}

	// Detail: 404 for an evicted or unknown record, 400 for a bad uuid, 404
	// for an unknown instance.
	expect(t, h.do("GET", p+"/call-1", viewer, ""), 404, `{"error":"hang-up not found"}`)
	expect(t, h.do("GET", p+"/call_1", viewer, ""), 400, `{"error":"invalid channel uuid"}`)
	expect(t, h.do("GET", p+"/call%201", viewer, ""), 400, `{"error":"invalid channel uuid"}`)
	expect(t, h.do("GET", "/api/instances/0000000000000000/hangups/call-5", viewer, ""), 404, `{"error":"instance not found"}`)
	expect(t, h.do("GET", "/api/instances/0000000000000000/hangups", viewer, ""), 404, `{"error":"instance not found"}`)

	// Records survive a reconnect and are served while disconnected.
	f.set(func(f *fakeSwitch) { f.state.Connected = false })
	f.publish(esl.Message{Kind: esl.KindState, Headers: map[string]string{"connected": "false"}})
	if got := rowUUIDs(h.hangups(viewer, "").Rows); got != "call-5 call-4 call-3" {
		t.Fatalf("disconnected %s", got)
	}
	f.set(func(f *fakeSwitch) { f.state.Connected = true })
	f.publish(esl.Message{Kind: esl.KindState, Headers: map[string]string{"connected": "true"}})
	f.publish(hangupEvent("call-6"))
	waitFor(t, "record after reconnect", func() bool { return strings.HasPrefix(rowUUIDs(h.hangups(viewer, "").Rows), "call-6") })
	if got := rowUUIDs(h.hangups(viewer, "").Rows); got != "call-6 call-5 call-4" {
		t.Fatalf("after reconnect %s", got)
	}

	// The instance is deleted: its records go.
	admin := h.login("admin", adminPW)
	expect(t, h.do("DELETE", "/api/instances/"+h.id, admin, ""), 204, "")
	expect(t, h.do("GET", p, viewer, ""), 404, "")
}

func TestHangupLogGating(t *testing.T) {
	var auditPath string
	h := newHarness(t, withBacklog(100, 6), withRecentCalls(10), withAudit(t, &auditPath))
	h.addUser("op", store.RoleOperator)
	f := h.fake(0)
	f.publish(userLog("6", "New Channel", "call-1"))
	f.publish(userLog("5", "Hangup [USER_BUSY]", "call-1"))
	f.publish(userLog("6", "someone else", "call-2"))
	f.publish(hangupEvent("call-1"))
	f.publish(hangupEvent("call-2", "Hangup-Cause", "NORMAL_CLEARING"))
	viewer := h.login("viewer", viewerPW)
	waitFor(t, "records", func() bool { return len(h.hangups(viewer, "").Rows) == 2 })
	if got := h.hangups(viewer, "").Rows[1]; got.UUID != "call-1" || got.LogLines != 2 {
		t.Fatalf("row %+v", got)
	}

	type detail struct {
		Row  hangupRow         `json:"row"`
		Vars map[string]string `json:"vars"`
		Log  *[]logEvent       `json:"log"`
	}
	get := func(token, uuid string) (detail, string) {
		t.Helper()
		r := h.do("GET", "/api/instances/"+h.id+"/hangups/"+uuid, token, "")
		expect(t, r, 200, "")
		var d detail
		r.json(t, &d)
		return d, r.body
	}
	d, body := get(viewer, "call-1")
	if d.Log != nil || !strings.Contains(body, `"log":null`) || d.Row.UUID != "call-1" || d.Vars["sip_auth_password"] != fsapi.Mask {
		t.Fatalf("viewer %s", body)
	}
	if strings.Contains(body, "New Channel") {
		t.Fatal("viewer got log lines")
	}
	for _, tok := range []string{h.login("op", userPW), h.login("admin", adminPW)} {
		d, body = get(tok, "call-1")
		if d.Log == nil || len(*d.Log) != 2 || (*d.Log)[0].Text != "New Channel" || (*d.Log)[1].Text != "Hangup [USER_BUSY]" || (*d.Log)[0].UUID != "call-1" {
			t.Fatalf("log lines %s", body)
		}
	}
	// A record without lines has an empty list, not null.
	if d, body = get(h.login("op", userPW), "call-2"); d.Log == nil || len(*d.Log) != 1 {
		t.Fatalf("call-2 %s", body)
	}
	f.publish(hangupEvent("call-3"))
	waitFor(t, "call-3", func() bool { return len(h.hangups(viewer, "").Rows) == 3 })
	if _, body = get(h.login("op", userPW), "call-3"); !strings.Contains(body, `"log":[]`) {
		t.Fatalf("call-3 %s", body)
	}

	// Neither route is audited.
	if data := must(t, auditPath); strings.Contains(string(data), "hangups") {
		t.Fatalf("audited: %s", data)
	}
}

func TestHangupsOff(t *testing.T) {
	h := newHarness(t, withBacklog(100, 6))
	viewer := h.login("viewer", viewerPW)
	h.fake(0).publish(hangupEvent("call-1"))
	h.fake(0).publish(heartbeat)
	waitFor(t, "heartbeat", func() bool { return len(h.instance(h.id).samples()) == 1 })
	expect(t, h.do("GET", "/api/instances/"+h.id+"/hangups", viewer, ""), 200, `{"enabled":false,"capacity":0,"rows":[]}`)
	expect(t, h.do("GET", "/api/instances/"+h.id+"/hangups?limit=5", viewer, ""), 200, `{"enabled":false,"capacity":0,"rows":[]}`)
	expect(t, h.do("GET", "/api/instances/"+h.id+"/hangups/call-1", viewer, ""), 404, "")

	st := newHarness(t).st
	if _, err := New(Config{Store: st, Dial: func(string, string) Switch { return newFake("", "") }, Static: h.srv.cfg.Static, RecentCalls: -1}); err == nil {
		t.Fatal("negative recent calls accepted")
	}
}

// TestLogEventUUID: the stream carries the uuid of a line about a channel and
// omits the field otherwise.
func TestLogEventUUID(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	f := h.fake(0)
	ch, cancel := h.openStream(admin, "?loglevel=7&backlog=0")
	defer cancel()
	replay(t, ch)
	f.publish(userLog("6", "about a call", "1ad48789-b2b1-4f88-8206-099360ce06f4"))
	f.publish(logMsg("6", "not about a call"))
	if ev := next(t, ch); !strings.Contains(ev.data, `"uuid":"1ad48789-b2b1-4f88-8206-099360ce06f4"`) {
		t.Fatalf("line with uuid %s", ev.data)
	}
	if ev := next(t, ch); strings.Contains(ev.data, "uuid") {
		t.Fatalf("line without uuid %s", ev.data)
	}
}
