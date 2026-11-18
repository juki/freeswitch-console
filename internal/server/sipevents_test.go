package server

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/juki/freeswitch-console/internal/esl"
	"github.com/juki/freeswitch-console/internal/fsapi"
	"github.com/juki/freeswitch-console/internal/store"
)

func withSipEvents(n int) func(*Config) {
	return func(c *Config) { c.SipEvents = n }
}

// sipMsg is a CUSTOM event of subclass with the headers every event carries
// and kv, as FreeSWITCH 1.11.3 sends it in JSON.
func sipMsg(subclass string, kv ...string) esl.Message {
	h := map[string]string{
		"Event-Name":                "CUSTOM",
		"Core-UUID":                 "b0d7b8a6-0000-4000-8000-000000000000",
		"FreeSWITCH-Hostname":       "fs01",
		"FreeSWITCH-Switchname":     "fs01",
		"FreeSWITCH-IPv4":           "192.0.2.10",
		"FreeSWITCH-IPv6":           "::1",
		"Event-Date-Local":          "2026-11-18 07:00:27",
		"Event-Date-GMT":            "Wed, 18 Nov 2026 07:00:27 GMT",
		"Event-Date-Timestamp":      "1794985227881000",
		"Event-Calling-File":        "sofia_reg.c",
		"Event-Calling-Function":    "sofia_reg_handle_register_token",
		"Event-Calling-Line-Number": "1792",
		"Event-Sequence":            "1234",
		"Event-Subclass":            subclass,
	}
	for k := 0; k+1 < len(kv); k += 2 {
		h[kv[k]] = kv[k+1]
	}
	return esl.Message{Kind: esl.KindEvent, Headers: h}
}

// The events of each kind. register_failure, gateway_add and gateway_delete
// are as the test switch sent them; the others have the headers sofia_reg.c
// gives them.
var (
	evRegister = sipMsg("sofia::register",
		"profile-name", "internal", "from-user", "1001", "from-host", "192.0.2.10", "presence-hosts", "192.0.2.10,192.0.2.10",
		"contact", `"1001" <sip:1001@198.51.100.7:5060>`, "call-id", "reg-1", "rpid", "unknown", "status", "Registered(UDP)",
		"expires", "3600", "to-user", "1001", "to-host", "192.0.2.10", "network-ip", "198.51.100.7", "network-port", "5060",
		"username", "1001", "realm", "192.0.2.10", "user-agent", "DeskPhone 2.4 token=ua-S3cret",
		"sip_auth_username", "1001", "effective_caller_id_name", "Reception", "x_api_token", "var-S3cret")
	evUnregister = sipMsg("sofia::unregister",
		"profile-name", "internal", "username", "1005", "from-user", "1005", "from-host", "192.0.2.10",
		"contact", "<sip:1005@203.0.113.15:5062;transport=tcp>", "call-id", "reg-2", "rpid", "unknown", "realm", "192.0.2.10",
		"network-ip", "203.0.113.15", "network-port", "5062", "user-agent", "SoftDial 5.2.3", "expires", "0")
	evUnregisterSocket = sipMsg("sofia::unregister",
		"profile-name", "internal", "from-user", "1006", "from-host", "192.0.2.10", "call-id", "reg-3", "rpid", "unknown", "reason", "socket-disconnection")
	evExpire = sipMsg("sofia::expire",
		"profile-name", "internal", "call-id", "reg-4", "user", "1008", "username", "1008", "host", "192.0.2.10",
		"contact", "<sip:1008@203.0.113.88:5060>", "expires", "1794985300", "user-agent", "MobileDial 2.9.4", "realm", "192.0.2.10",
		"network-ip", "203.0.113.88", "network-port", "5060")
	evFailure = sipMsg("sofia::register_failure",
		"network-ip", "172.17.0.2", "network-port", "42374", "profile-name", "internal", "registration-type", "REGISTER",
		"to-host", "172.17.0.2", "to-user", "1001", "user-agent", "iter8-probe/1.0")
	evGatewayState = sipMsg("sofia::gateway_state",
		"Gateway", "carrier1", "State", "FAILED", "Ping-Status", "DOWN", "Register-Network-IP", "203.0.113.9",
		"Register-Network-Port", "5060", "Phrase", "Request Timeout", "Status", "408")
	evGatewayAdd    = sipMsg("sofia::gateway_add", "Gateway", "example.com", "profile-name", "external")
	evGatewayDelete = sipMsg("sofia::gateway_delete", "Gateway", "example.com", "profile-name", "external")
)

func TestNewSipEvent(t *testing.T) {
	now := time.UnixMilli(1794985227881)
	for _, tc := range []struct {
		m    esl.Message
		want sipEventRow
	}{
		{evRegister, sipEventRow{Kind: "register", Profile: "internal", User: "1001", Domain: "192.0.2.10", IP: "198.51.100.7", Port: 5060,
			UserAgent: "DeskPhone 2.4 token=" + fsapi.Mask, Contact: `"1001" <sip:1001@198.51.100.7:5060>`, Expires: 3600}},
		{evUnregister, sipEventRow{Kind: "unregister", Profile: "internal", User: "1005", Domain: "192.0.2.10", IP: "203.0.113.15", Port: 5062,
			UserAgent: "SoftDial 5.2.3", Contact: "<sip:1005@203.0.113.15:5062;transport=tcp>"}},
		{evUnregisterSocket, sipEventRow{Kind: "unregister", Profile: "internal", User: "1006", Domain: "192.0.2.10"}},
		// The expires of an expire event is a unix time: not kept as seconds.
		{evExpire, sipEventRow{Kind: "expire", Profile: "internal", User: "1008", Domain: "192.0.2.10", IP: "203.0.113.88", Port: 5060,
			UserAgent: "MobileDial 2.9.4", Contact: "<sip:1008@203.0.113.88:5060>"}},
		{evFailure, sipEventRow{Kind: "register_failure", Profile: "internal", User: "1001", Domain: "172.17.0.2", IP: "172.17.0.2", Port: 42374,
			UserAgent: "iter8-probe/1.0", Method: "REGISTER"}},
		{evGatewayState, sipEventRow{Kind: "gateway_state", IP: "203.0.113.9", Port: 5060, Gateway: "carrier1", State: "FAILED", PingStatus: "DOWN",
			Status: 408, Phrase: "Request Timeout"}},
		{evGatewayAdd, sipEventRow{Kind: "gateway_add", Profile: "external", Gateway: "example.com"}},
		{evGatewayDelete, sipEventRow{Kind: "gateway_delete", Profile: "external", Gateway: "example.com"}},
	} {
		kind := sipKind(tc.m.Headers["Event-Subclass"])
		e := newSipEvent(kind, tc.m.Headers, now)
		tc.want.T = 1794985227881
		if e.row != tc.want {
			t.Errorf("%s\n got %+v\nwant %+v", tc.m.Headers["Event-Subclass"], e.row, tc.want)
		}
		// The event's own headers, without the generic ones.
		for k := range tc.m.Headers {
			generic := strings.HasPrefix(k, "Event-") || strings.HasPrefix(k, "Core-") || strings.HasPrefix(k, "FreeSWITCH-")
			if _, ok := e.headers[k]; ok == generic {
				t.Errorf("%s: header %s kept %v", kind, k, ok)
			}
		}
	}
	if got := newSipEvent("register_failure", evFailure.Headers, now).headers; !reflect.DeepEqual(got, map[string]string{
		"network-ip": "172.17.0.2", "network-port": "42374", "profile-name": "internal", "registration-type": "REGISTER",
		"to-host": "172.17.0.2", "to-user": "1001", "user-agent": "iter8-probe/1.0",
	}) {
		t.Errorf("failure headers %v", got)
	}

	// Masked when stored: a secret-looking name whole, other values inline.
	e := newSipEvent("register", evRegister.Headers, now)
	if e.headers["x_api_token"] != fsapi.Mask || e.headers["user-agent"] != "DeskPhone 2.4 token="+fsapi.Mask || e.headers["effective_caller_id_name"] != "Reception" {
		t.Errorf("headers %v", e.headers)
	}
	if strings.Contains(fmt.Sprint(e.row, e.headers), "S3cret") {
		t.Fatalf("a secret is kept: %+v %v", e.row, e.headers)
	}
	if !strings.Contains(mustJSON(t, e.row), `"pingStatus":""`) {
		t.Errorf("row JSON %s", mustJSON(t, e.row))
	}

	// Subclasses that are not kept, and events that are not CUSTOM.
	for _, sub := range []string{"sofia::pre_register", "sofia::register_attempt", "sofia::gateway_invalid_digest_req", "", "register"} {
		if k := sipKind(sub); k != "" {
			t.Errorf("%q kept as %q", sub, k)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSipHeadersBound(t *testing.T) {
	h := map[string]string{"Event-Name": "CUSTOM", "Core-UUID": "x", "FreeSWITCH-IPv4": "x"}
	for n := 0; n < 100; n++ {
		h[fmt.Sprintf("h%03d", n)] = "x"
	}
	got := sipHeaders(h)
	if len(got) != sipEventMaxHeaders {
		t.Fatalf("%d headers", len(got))
	}
	if _, ok := got["h059"]; !ok {
		t.Fatal("h059 dropped")
	}
	if _, ok := got["h060"]; ok {
		t.Fatal("h060 kept")
	}
}

// TestSipEventSize: a register_failure carries values chosen by the sender;
// its record is bounded.
func TestSipEventSize(t *testing.T) {
	now := time.UnixMilli(1794985227881)
	h := maps.Clone(evFailure.Headers)
	h["user-agent"] = strings.Repeat("A", 10000)
	for n := 0; n < 200; n++ {
		h[fmt.Sprintf("h%03d", n)] = strings.Repeat("v", 300)
	}
	h[strings.Repeat("a", 64)] = "64"
	h[strings.Repeat("b", 65)] = "65"
	e := newSipEvent("register_failure", h, now)
	if e.row.UserAgent != strings.Repeat("A", 256) || e.row.User != "1001" || e.row.Method != "REGISTER" {
		t.Fatalf("row %+v", e.row)
	}
	size := 0
	for k, v := range e.headers {
		size += len(k) + len(v)
		if len(v) > sipEventMaxValue {
			t.Errorf("%s: %d characters", k, len(v))
		}
	}
	if len(e.headers) > sipEventMaxHeaders || size > sipEventMaxBytes {
		t.Fatalf("%d headers, %d bytes", len(e.headers), size)
	}
	// Sorted order: the 64-character name and h000-h014 (260 bytes each) fill
	// the budget; h015 on and user-agent are skipped, the short ones after
	// them kept.
	if e.headers[strings.Repeat("a", 64)] != "64" || e.headers["h000"] != strings.Repeat("v", 256) || e.headers["h014"] == "" {
		t.Fatalf("kept %v", slices.Sorted(maps.Keys(e.headers)))
	}
	for _, k := range []string{strings.Repeat("b", 65), "h015", "h199", "user-agent"} {
		if _, ok := e.headers[k]; ok {
			t.Errorf("%s kept", k)
		}
	}
	if e.headers["network-ip"] != "172.17.0.2" || e.headers["to-user"] != "1001" || e.headers["registration-type"] != "REGISTER" {
		t.Fatalf("short headers after the budget dropped: %v", slices.Sorted(maps.Keys(e.headers)))
	}
	// Deterministic.
	if !reflect.DeepEqual(newSipEvent("register_failure", h, now), e) {
		t.Fatal("not deterministic")
	}

	// Multi-byte values are cut on a character boundary.
	for _, v := range []string{strings.Repeat("é", 300), "a" + strings.Repeat("€", 300), strings.Repeat("😀", 257)} {
		e := newSipEvent("register_failure", map[string]string{"to-user": v, "user-agent": v}, now)
		for _, got := range []string{e.row.User, e.row.UserAgent, e.headers["to-user"], e.headers["user-agent"]} {
			if !utf8.ValidString(got) || utf8.RuneCountInString(got) != sipEventMaxValue || !strings.HasPrefix(v, got) {
				t.Errorf("%q cut to %q", v[:8], got)
			}
		}
	}
	if got := cutValue(strings.Repeat("x", 256)); got != strings.Repeat("x", 256) {
		t.Errorf("256 characters cut to %d", len(got))
	}

	// Ordinary events are unchanged: every own header kept, masked only.
	for _, m := range []esl.Message{evRegister, evUnregister, evExpire, evFailure, evGatewayState} {
		want := map[string]string{}
		for k, v := range m.Headers {
			if !strings.HasPrefix(k, "Event-") && !strings.HasPrefix(k, "Core-") && !strings.HasPrefix(k, "FreeSWITCH-") {
				want[k] = fsapi.MaskValue(k, v)
			}
		}
		if got := sipHeaders(m.Headers); !reflect.DeepEqual(got, want) {
			t.Errorf("%s\n got %v\nwant %v", m.Headers["Event-Subclass"], got, want)
		}
	}
}

// TestSipEventMethod: a register_failure names the request that failed.
func TestSipEventMethod(t *testing.T) {
	now := time.UnixMilli(1794985227881)
	for _, tc := range []struct {
		kind, regType, want string
	}{
		{"register_failure", "REGISTER", "REGISTER"},
		{"register_failure", "INVITE", "INVITE"},
		{"register_failure", "", ""},
		{"register", "REGISTER", ""},
		{"unregister", "REGISTER", ""},
		{"expire", "REGISTER", ""},
		{"gateway_state", "REGISTER", ""},
	} {
		h := map[string]string{"to-user": "1001", "network-ip": "203.0.113.45"}
		if tc.regType != "" {
			h["registration-type"] = tc.regType
		}
		e := newSipEvent(tc.kind, h, now)
		if e.row.Method != tc.want {
			t.Errorf("%s %q: method %q", tc.kind, tc.regType, e.row.Method)
		}
		if !strings.Contains(mustJSON(t, e.row), `"method":"`+tc.want+`"`) {
			t.Errorf("row JSON %s", mustJSON(t, e.row))
		}
	}
}

// sipReply is the answer of GET .../sipevents.
type sipReply struct {
	Enabled  bool          `json:"enabled"`
	Capacity int           `json:"capacity"`
	Rows     []sipEventRow `json:"rows"`
}

func (h *harness) sipEvents(token, query string) sipReply {
	h.t.Helper()
	r := h.do("GET", "/api/instances/"+h.id+"/sipevents"+query, token, "")
	expect(h.t, r, 200, "")
	var out sipReply
	r.json(h.t, &out)
	return out
}

func rowSeqs(rows []sipEventRow) string {
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%d %s", r.Seq, r.Kind))
	}
	return strings.Join(out, " ")
}

func TestSipEventRoutes(t *testing.T) {
	var auditPath string
	h := newHarness(t, withSipEvents(4), withAudit(t, &auditPath))
	viewer := h.login("viewer", viewerPW)
	f := h.fake(0)
	p := "/api/instances/" + h.id + "/sipevents"

	if got := h.sipEvents(viewer, ""); !got.Enabled || got.Capacity != 4 || got.Rows == nil || len(got.Rows) != 0 {
		t.Fatalf("empty %+v", got)
	}
	expect(t, h.do("GET", p+"/failures", viewer, ""), 200, `{"minutes":60,"rows":[]}`)

	// Other events, subclasses not kept and a CUSTOM event without one are
	// not stored.
	f.publish(sipMsg("sofia::pre_register", "to-user", "1001"))
	f.publish(sipMsg(""))
	f.publish(hangupEvent("call-1"))
	for _, m := range []esl.Message{evRegister, evFailure, evGatewayState, evUnregister, evExpire, evFailure} {
		f.publish(m)
	}
	waitFor(t, "six records", func() bool { return strings.HasPrefix(rowSeqs(h.sipEvents(viewer, "").Rows), "6 ") })

	// The ring keeps the last four, newest first; limit cuts and is capped
	// at the capacity; kind filters.
	if got := rowSeqs(h.sipEvents(viewer, "").Rows); got != "6 register_failure 5 expire 4 unregister 3 gateway_state" {
		t.Fatalf("rows %s", got)
	}
	if got := rowSeqs(h.sipEvents(viewer, "?limit=2").Rows); got != "6 register_failure 5 expire" {
		t.Fatalf("limit=2 %s", got)
	}
	if got := rowSeqs(h.sipEvents(viewer, "?limit=1000").Rows); got != "6 register_failure 5 expire 4 unregister 3 gateway_state" {
		t.Fatalf("limit=1000 %s", got)
	}
	if got := rowSeqs(h.sipEvents(viewer, "?kind=gateway_state,register_failure").Rows); got != "6 register_failure 3 gateway_state" {
		t.Fatalf("kind %s", got)
	}
	if got := rowSeqs(h.sipEvents(viewer, "?kind=expire&limit=1").Rows); got != "5 expire" {
		t.Fatalf("kind and limit %s", got)
	}
	if got := h.sipEvents(viewer, "?kind=register"); len(got.Rows) != 0 || got.Rows == nil {
		t.Fatalf("evicted kind %+v", got)
	}
	r := h.do("GET", p, viewer, "")
	if strings.Contains(r.body, "headers") || strings.Contains(r.body, "registration-type") {
		t.Fatalf("list rows carry headers: %s", r.body)
	}
	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=x"} {
		expect(t, h.do("GET", p+q, viewer, ""), 400, `{"error":"limit must be a positive number"}`)
	}
	expect(t, h.do("GET", p+"?kind=register,nope", viewer, ""), 400, `{"error":"unknown kind \"nope\""}`)
	expect(t, h.do("GET", p+"?kind=register,", viewer, ""), 400, `{"error":"unknown kind \"\""}`)
	expect(t, h.do("GET", p+"?kind=sofia::register", viewer, ""), 400, `{"error":"unknown kind \"sofia::register\""}`)

	// Detail by seq: the row and the masked headers; 404 once evicted.
	var d struct {
		Row     sipEventRow
		Headers map[string]string
	}
	r = h.do("GET", p+"/6", viewer, "")
	expect(t, r, 200, "")
	r.json(t, &d)
	if d.Row.Seq != 6 || d.Row.Kind != "register_failure" || d.Headers["registration-type"] != "REGISTER" || d.Headers["Event-Name"] != "" {
		t.Fatalf("detail %s", r.body)
	}
	r = h.do("GET", p+"/3", viewer, "")
	r.json(t, &d)
	if d.Row.Seq != 3 || d.Row.Kind != "gateway_state" || d.Headers["Phrase"] != "Request Timeout" {
		t.Fatalf("detail 3 %s", r.body)
	}
	expect(t, h.do("GET", p+"/1", viewer, ""), 404, `{"error":"SIP event not found"}`)
	expect(t, h.do("GET", p+"/99", viewer, ""), 404, `{"error":"SIP event not found"}`)
	for _, seq := range []string{"0", "-1", "x", "1.5", "99999999999999999999"} {
		expect(t, h.do("GET", p+"/"+seq, viewer, ""), 400, `{"error":"invalid sequence number"}`)
	}
	for _, q := range []string{"", "/failures", "/6"} {
		expect(t, h.do("GET", "/api/instances/0000000000000000/sipevents"+q, viewer, ""), 404, `{"error":"instance not found"}`)
	}

	// Failures: minutes is bounded.
	for _, q := range []string{"?minutes=0", "?minutes=1441", "?minutes=x", "?minutes=-5"} {
		expect(t, h.do("GET", p+"/failures"+q, viewer, ""), 400, `{"error":"minutes must be 1 to 1440"}`)
	}
	var fails struct {
		Minutes int
		Rows    []sipFailure
	}
	h.do("GET", p+"/failures?minutes=1440", viewer, "").json(t, &fails)
	if fails.Minutes != 1440 || len(fails.Rows) != 1 || fails.Rows[0].IP != "172.17.0.2" || fails.Rows[0].Attempts != 1 || fails.Rows[0].Users[0] != "1001" {
		t.Fatalf("failures %+v", fails)
	}

	// Records survive a reconnect and are served while disconnected.
	f.set(func(f *fakeSwitch) { f.state.Connected = false })
	f.publish(esl.Message{Kind: esl.KindState, Headers: map[string]string{"connected": "false"}})
	if got := rowSeqs(h.sipEvents(viewer, "").Rows); got != "6 register_failure 5 expire 4 unregister 3 gateway_state" {
		t.Fatalf("disconnected %s", got)
	}
	expect(t, h.do("GET", p+"/5", viewer, ""), 200, "")
	f.set(func(f *fakeSwitch) { f.state.Connected = true })
	f.publish(esl.Message{Kind: esl.KindState, Headers: map[string]string{"connected": "true"}})
	f.publish(evGatewayAdd)
	waitFor(t, "record after reconnect", func() bool { return strings.HasPrefix(rowSeqs(h.sipEvents(viewer, "").Rows), "7 ") })
	if got := rowSeqs(h.sipEvents(viewer, "").Rows); got != "7 gateway_add 6 register_failure 5 expire 4 unregister" {
		t.Fatalf("after reconnect %s", got)
	}

	// Not audited.
	if data := must(t, auditPath); strings.Contains(string(data), "sipevents") {
		t.Fatalf("audited: %s", data)
	}

	// The instance is deleted: its records go.
	admin := h.login("admin", adminPW)
	expect(t, h.do("DELETE", "/api/instances/"+h.id, admin, ""), 204, "")
	expect(t, h.do("GET", p, viewer, ""), 404, "")
}

func TestSipEventsOff(t *testing.T) {
	h := newHarness(t)
	viewer := h.login("viewer", viewerPW)
	h.fake(0).publish(evFailure)
	h.fake(0).publish(heartbeat)
	waitFor(t, "heartbeat", func() bool { return len(h.instance(h.id).samples()) == 1 })
	p := "/api/instances/" + h.id + "/sipevents"
	expect(t, h.do("GET", p, viewer, ""), 200, `{"enabled":false,"capacity":0,"rows":[]}`)
	expect(t, h.do("GET", p+"?limit=5&kind=register", viewer, ""), 200, `{"enabled":false,"capacity":0,"rows":[]}`)
	expect(t, h.do("GET", p+"/failures", viewer, ""), 200, `{"minutes":60,"rows":[]}`)
	expect(t, h.do("GET", p+"/1", viewer, ""), 404, "")
	inst := h.instance(h.id)
	inst.sipMu.Lock()
	n := len(inst.sipCounts)
	inst.sipMu.Unlock()
	if n != 0 {
		t.Fatalf("counted %d kinds while off", n)
	}

	st := newHarness(t).st
	if _, err := New(Config{Store: st, Dial: func(string, string) Switch { return newFake("", "") }, Static: h.srv.cfg.Static, SipEvents: -1}); err == nil {
		t.Fatal("negative SIP events accepted")
	}
}

// failure is a register_failure record from ip for user, received at.
func failure(ip, user, agent string, at time.Time) *sipEvent {
	return newSipEvent("register_failure", map[string]string{"network-ip": ip, "to-user": user, "user-agent": agent}, at)
}

func TestSipFailures(t *testing.T) {
	inst := newInstance(newFake("a", "b"), 0, 0, 0, 1000, nil, nil)
	defer inst.close()
	now := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	put := func(e *sipEvent) {
		inst.sipMu.Lock()
		defer inst.sipMu.Unlock()
		inst.storeSipEvent(e)
	}
	// Outside the window: not counted.
	put(failure("203.0.113.45", "old", "old-agent", ago(2*time.Hour)))
	// 198.51.100.177: 3 attempts; 203.0.113.45: 3 attempts, later; 192.0.2.250: 1.
	put(failure("198.51.100.177", "1001", "sipcli/v1.8", ago(50*time.Minute)))
	put(failure("203.0.113.45", "admin", "friendly-scanner", ago(40*time.Minute)))
	put(newSipEvent("register", map[string]string{"network-ip": "203.0.113.45", "from-user": "1001"}, ago(35*time.Minute)))
	put(failure("198.51.100.177", "1002", "sipcli/v1.8", ago(30*time.Minute)))
	put(failure("203.0.113.45", "100", "friendly-scanner", ago(20*time.Minute)))
	put(failure("192.0.2.250", "test", "VaxSIPUserAgent/3.1", ago(15*time.Minute)))
	put(failure("198.51.100.177", "1001", "sipcli/v1.9", ago(10*time.Minute)))
	put(failure("203.0.113.45", "admin", "friendly-scanner-2", ago(5*time.Minute)))

	want := []sipFailure{
		{IP: "203.0.113.45", Attempts: 3, Users: []string{"admin", "100"}, UserAgent: "friendly-scanner-2", Last: ago(5 * time.Minute).UnixMilli()},
		{IP: "198.51.100.177", Attempts: 3, Users: []string{"1001", "1002"}, UserAgent: "sipcli/v1.9", Last: ago(10 * time.Minute).UnixMilli()},
		{IP: "192.0.2.250", Attempts: 1, Users: []string{"test"}, UserAgent: "VaxSIPUserAgent/3.1", Last: ago(15 * time.Minute).UnixMilli()},
	}
	if got := inst.sipFailures(ago(time.Hour)); !reflect.DeepEqual(got, want) {
		t.Fatalf("an hour\n got %+v\nwant %+v", got, want)
	}
	// A shorter window.
	if got := inst.sipFailures(ago(12 * time.Minute)); len(got) != 2 || got[0].IP != "203.0.113.45" || got[0].Attempts != 1 || got[1].IP != "198.51.100.177" || got[1].Users[0] != "1001" {
		t.Fatalf("12 minutes %+v", got)
	}
	// Equal attempts and times: by address.
	tie := newInstance(newFake("a", "b"), 0, 0, 0, 10, nil, nil)
	defer tie.close()
	tie.sipMu.Lock()
	tie.storeSipEvent(failure("192.0.2.9", "u", "", ago(time.Minute)))
	tie.storeSipEvent(failure("192.0.2.1", "u", "", ago(time.Minute)))
	tie.sipMu.Unlock()
	if got := tie.sipFailures(ago(time.Hour)); len(got) != 2 || got[0].IP != "192.0.2.1" {
		t.Fatalf("tie %+v", got)
	}

	// Caps: at most sipFailuresRows sources, sipFailuresUsers users each, the
	// most frequent first, then by name.
	many := newInstance(newFake("a", "b"), 0, 0, 0, 1000, nil, nil)
	defer many.close()
	many.sipMu.Lock()
	for n := 0; n < sipFailuresRows+5; n++ {
		ip := fmt.Sprintf("198.51.100.%d", n)
		for k := 0; k <= n%3; k++ {
			many.storeSipEvent(failure(ip, "u", "", ago(time.Minute)))
		}
	}
	for n := 0; n < 15; n++ {
		for k := 0; k <= n/5; k++ {
			many.storeSipEvent(failure("203.0.113.1", fmt.Sprintf("user%02d", n), "", ago(time.Minute)))
		}
	}
	many.sipMu.Unlock()
	got := many.sipFailures(ago(time.Hour))
	if len(got) != sipFailuresRows || got[0].IP != "203.0.113.1" || got[0].Attempts != 30 {
		t.Fatalf("%d rows, first %+v", len(got), got[0])
	}
	if strings.Join(got[0].Users, " ") != "user10 user11 user12 user13 user14 user05 user06 user07 user08 user09" {
		t.Fatalf("users %v", got[0].Users)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Attempts > got[i-1].Attempts {
			t.Fatalf("row %d %+v after %+v", i, got[i], got[i-1])
		}
	}
	if got[len(got)-1].Attempts != 1 {
		t.Fatalf("last row %+v: the sources with fewest attempts are dropped first", got[len(got)-1])
	}
}

// TestSipEventPrefill: events a switch knew before it was dialled are kept
// oldest first, numbered from 1, and not counted.
func TestSipEventPrefill(t *testing.T) {
	at := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	past := []pastSipEvent{
		{at, evFailure.Headers},
		{at.Add(time.Minute), sipMsg("sofia::pre_register").Headers},
		{at.Add(2 * time.Minute), evGatewayState.Headers},
	}
	inst := newInstance(newFake("a", "b"), 0, 0, 0, 10, nil, past)
	defer inst.close()
	rows := inst.sipRows(10, nil)
	if rowSeqs(rows) != "2 gateway_state 1 register_failure" || rows[0].T != at.Add(2*time.Minute).UnixMilli() {
		t.Fatalf("rows %+v", rows)
	}
	inst.sipMu.Lock()
	defer inst.sipMu.Unlock()
	if len(inst.sipCounts) != 0 {
		t.Fatalf("counted %v", inst.sipCounts)
	}
	off := newInstance(newFake("a", "b"), 0, 0, 0, 0, nil, past)
	defer off.close()
	if len(off.sipRows(10, nil)) != 0 {
		t.Fatal("prefilled while off")
	}
}

// TestSipEventRoles: every role reads the events.
func TestSipEventRoles(t *testing.T) {
	h := newHarness(t, withSipEvents(10))
	h.addUser("op", store.RoleOperator)
	h.fake(0).publish(evFailure)
	viewer := h.login("viewer", viewerPW)
	waitFor(t, "record", func() bool { return len(h.sipEvents(viewer, "").Rows) == 1 })
	for _, tok := range []string{viewer, h.login("op", userPW), h.login("admin", adminPW)} {
		expect(t, h.do("GET", "/api/instances/"+h.id+"/sipevents/1", tok, ""), 200, "")
	}
	expect(t, h.do("GET", "/api/instances/"+h.id+"/sipevents/1", "", ""), 401, "")
}
