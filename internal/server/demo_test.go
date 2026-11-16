package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/juki/freeswitch-console/internal/demo"
	"github.com/juki/freeswitch-console/internal/esl"
	"github.com/juki/freeswitch-console/internal/store"
)

const (
	demoEast = "192.0.2.11:8021"
	demoLab  = "203.0.113.30:8021"
)

// demoHarness is a server in demo mode on simulated switches whose clocks
// the test moves.
type demoHarness struct {
	*harness
	ids map[string]string // instance name -> id

	mu       sync.Mutex
	switches map[string]*demo.Switch // by address
}

func newDemoHarness(t *testing.T) *demoHarness {
	t.Helper()
	st := store.NewMemory()
	if err := demo.Seed(st); err != nil {
		t.Fatal(err)
	}
	h := &demoHarness{harness: &harness{t: t, st: st}, ids: map[string]string{}, switches: map[string]*demo.Switch{}}
	for _, in := range st.List() {
		h.ids[in.Name] = in.ID
	}
	start := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	srv, err := New(Config{
		Demo:  true,
		Store: st,
		Dial: func(addr, _ string) Switch {
			sw := demo.New(addr, 1, start)
			h.mu.Lock()
			h.switches[addr] = sw
			h.mu.Unlock()
			return sw
		},
		Static:          fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>fsconsole</title>")}},
		LogBacklog:      2000,
		LogBacklogLevel: 6, // as main: debug lines would flood the buffers while a test races the clock
		RecentCalls:     1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	srv.loginDelay = 0
	h.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		srv.Close()
		h.ts.Close()
	})
	return h
}

func (h *demoHarness) sw(addr string) *demo.Switch {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.switches[addr]
}

func sessionOf(r response) string {
	for _, c := range r.resp.Cookies() {
		if c.Name == cookieName {
			return c.Value
		}
	}
	return ""
}

func TestDemoSession(t *testing.T) {
	h := newDemoHarness(t)
	r := h.do("GET", "/api/me", "", "")
	expect(t, r, 200, `{"username":"demo","role":"admin","managed":true}`)
	token := sessionOf(r)
	if token == "" {
		t.Fatal("no session cookie")
	}
	if r = h.do("GET", "/api/me", token, ""); r.code != 200 || sessionOf(r) != "" {
		t.Fatalf("with the cookie: %d, new cookie %q", r.code, sessionOf(r))
	}
	if r = h.do("GET", "/api/me", "deadbeef", ""); r.code != 200 || sessionOf(r) == "" {
		t.Fatalf("with a stale cookie: %d %s", r.code, r.body)
	}
	expect(t, h.do("GET", "/api/version", token, ""), 200, `{"demo":true,"version":"dev"}`)
	expect(t, h.do("POST", "/api/login", "", `{"username":"demo","password":"demo"}`), 401, "")
	expect(t, h.do("POST", "/api/login", "", `{"username":"admin","password":"admin-pw"}`), 401, "")
	var users []userView
	h.do("GET", "/api/users", token, "").json(t, &users)
	if len(users) != 1 || users[0].Username != "demo" || !users[0].Managed {
		t.Fatalf("users %+v", users)
	}
	// A stream opened without a cookie keeps the session it was given.
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/instances/"+h.ids["pbx-east-1"]+"/stream", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream: %v %v", resp, err)
	}
	resp.Body.Close()
}

func TestDemoDisabledRoutes(t *testing.T) {
	h := newDemoHarness(t)
	id := h.ids["pbx-east-1"]
	p := "/api/instances/" + id
	table := []struct{ pattern, method, path, body string }{
		{"POST /api/instances", "POST", "/api/instances", `{"name":"x","host":"192.0.2.1","port":8021,"password":"p"}`},
		{"PUT /api/instances/{id}", "PUT", p, `{"name":"x","host":"192.0.2.1","port":8021,"password":"p"}`},
		{"DELETE /api/instances/{id}", "DELETE", p, ""},
		{"POST /api/users", "POST", "/api/users", `{"username":"visitor","role":"admin","password":"a-long-password"}`},
		{"PUT /api/users/{id}", "PUT", "/api/users/demo", `{"role":"viewer"}`},
		{"DELETE /api/users/{id}", "DELETE", "/api/users/demo", ""},
		{"POST /api/me/password", "POST", "/api/me/password", `{"current":"x","password":"a-long-password"}`},
		{"PUT /api/instances/{id}/files/content", "PUT", p + "/files/content", `{"path":"vars.xml","content":"<include/>","baseSha256":""}`},
		{"DELETE /api/instances/{id}/files/content", "DELETE", p + "/files/content?path=vars.xml&baseSha256=", ""},
		{"POST /api/instances/{id}/files/restore", "POST", p + "/files/restore", `{"path":"vars.xml","id":"x","baseSha256":""}`},
	}
	if len(table) != len(demoDisabled) {
		t.Fatalf("%d routes tested, %d disabled", len(table), len(demoDisabled))
	}
	for _, rt := range table {
		if !demoDisabled[rt.pattern] {
			t.Errorf("%s is not disabled", rt.pattern)
		}
		if _, ok := h.srv.routeCaps[rt.pattern]; !ok {
			t.Errorf("%s is no route", rt.pattern)
		}
		expect(t, h.do(rt.method, rt.path, "", rt.body), 403, `{"error":"disabled in the demo"}`)
	}
	if list := h.st.List(); len(list) != 3 || list[0].Name != "pbx-east-1" {
		t.Fatalf("store changed: %+v", list)
	}

	// Reading the files works.
	var rows struct{ Rows []struct{ Name string } }
	h.do("GET", p+"/files?path=", "", "").json(t, &rows)
	if len(rows.Rows) != 6 {
		t.Fatalf("files %+v", rows)
	}
	expect(t, h.do("GET", p+"/files/content?path=vars.xml", "", ""), 200, "")
	var found struct{ Rows []struct{ Path string } }
	h.do("GET", p+"/files/search?q=carrier-backup&case=1", "", "").json(t, &found)
	if len(found.Rows) == 0 || found.Rows[0].Path != "autoload_configs/acl.conf.xml" {
		t.Fatalf("search %+v", found)
	}
	expect(t, h.do("GET", p+"/files/history?path=vars.xml", "", ""), 200, `{"rows":[]}`)
}

// TestDemoSessionBound opens limit+N sessions without a cookie: the N oldest
// end, and the visitor holding one of them gets a new session.
func TestDemoSessionBound(t *testing.T) {
	const n = 3
	h := newDemoHarness(t)
	open := func() string {
		r := h.do("GET", "/api/me", "", "")
		if r.code != 200 || sessionOf(r) == "" {
			t.Fatalf("without a cookie: %d %s", r.code, r.body)
		}
		return sessionOf(r)
	}
	tokens := []string{open()}
	for len(tokens) < demoMaxSessions+n-2 {
		w := httptest.NewRecorder()
		if _, ok := h.srv.demoSession(w, httptest.NewRequest("GET", "/api/me", nil)); !ok {
			t.Fatal("refused")
		}
		tokens = append(tokens, w.Result().Cookies()[0].Value)
	}
	tokens = append(tokens, open(), open())
	h.srv.sessMu.Lock()
	count := len(h.srv.sessions)
	var missing, kept int
	for i, tok := range tokens {
		_, ok := h.srv.sessions[tok]
		switch {
		case i >= n && !ok:
			missing++
		case i < n && ok:
			kept++
		}
	}
	h.srv.sessMu.Unlock()
	if count != demoMaxSessions || missing != 0 || kept != 0 {
		t.Fatalf("%d sessions, %d of the newest missing, %d of the oldest kept", count, missing, kept)
	}
	for _, tok := range []string{tokens[n], tokens[len(tokens)-1]} {
		if r := h.do("GET", "/api/me", tok, ""); r.code != 200 || sessionOf(r) != "" {
			t.Fatalf("a kept session: %d, new cookie %q", r.code, sessionOf(r))
		}
	}
	r := h.do("GET", "/api/me", tokens[0], "")
	if r.code != 200 || sessionOf(r) == "" || sessionOf(r) == tokens[0] {
		t.Fatalf("the oldest session: %d %s", r.code, r.body)
	}
	r = h.do("GET", "/api/me", sessionOf(r), "")
	h.srv.sessMu.Lock()
	count = len(h.srv.sessions)
	h.srv.sessMu.Unlock()
	if r.code != 200 || count != demoMaxSessions {
		t.Fatalf("its new session: %d, %d sessions", r.code, count)
	}
}

// TestDemoHangups runs ten simulated minutes of pbx-east-1 through the
// server: every hang-up event becomes one record with its cause, SIP status
// and the log lines of its channel.
func TestDemoHangups(t *testing.T) {
	h := newDemoHarness(t)
	sw := h.sw(demoEast)
	events, _, cancel := sw.Subscribe(1 << 16)
	defer cancel()
	ended := map[string]map[string]string{}
	for i := 0; i < 600; i++ {
		sw.Advance(time.Second)
		for len(events) > 0 {
			if m := <-events; m.Headers["Event-Name"] == "CHANNEL_HANGUP_COMPLETE" {
				ended[m.Headers["Unique-ID"]] = m.Headers
			}
		}
		if i%50 == 0 {
			time.Sleep(time.Millisecond) // let the collector keep up
		}
	}
	if len(ended) < 50 {
		t.Fatalf("%d hang-ups in 10 minutes", len(ended))
	}
	p := "/api/instances/" + h.ids["pbx-east-1"]
	var list struct{ Rows []hangupRow }
	waitFor(t, "every record with its lines", func() bool {
		h.do("GET", p+"/hangups?limit=1000", "", "").json(t, &list)
		if len(list.Rows) != len(ended) {
			return false
		}
		for _, row := range list.Rows {
			if row.LogLines < 3 {
				return false
			}
		}
		return true
	})
	wantStatus := map[string]int{"USER_BUSY": 486, "CALL_REJECTED": 603, "NO_ROUTE_DESTINATION": 404, "RECOVERY_ON_TIMER_EXPIRE": 408, "INCOMPATIBLE_DESTINATION": 488, "ORIGINATOR_CANCEL": 487}
	for _, row := range list.Rows {
		ev := ended[row.UUID]
		if ev == nil || row.Cause != ev["Hangup-Cause"] || row.Cause == "" || row.Name == "" || row.Context == "" || row.Profile == "" {
			t.Fatalf("record %+v", row)
		}
		if want, ok := wantStatus[row.Cause]; ok && row.SIPStatus != want {
			t.Errorf("%s with SIP status %d", row.Cause, row.SIPStatus)
		}
		if row.Answered && (row.Codec == "" || row.MOS < 1.9 || row.SIPStatus != 200) {
			t.Errorf("answered record %+v", row)
		}
	}
	var detail struct {
		Row  hangupRow
		Vars map[string]string
		Log  []logEvent
	}
	h.do("GET", p+"/hangups/"+list.Rows[0].UUID, "", "").json(t, &detail)
	if len(detail.Log) < 3 || detail.Vars["sofia_profile_name"] == "" {
		t.Fatalf("detail %+v", detail)
	}
	for _, l := range detail.Log {
		if l.UUID != detail.Row.UUID || !strings.HasPrefix(l.Text, l.UUID+" ") {
			t.Fatalf("line of another channel: %+v", l)
		}
	}
}

// TestDemoConsoleAndActions drives the simulation through the console and
// the actions, as a visitor does.
func TestDemoConsoleAndActions(t *testing.T) {
	h := newDemoHarness(t)
	p := "/api/instances/" + h.ids["pbx-east-1"]
	console := func(cmd string) string {
		t.Helper()
		var out struct{ Output string }
		r := h.do("POST", p+"/console", "", `{"command":"`+cmd+`"}`)
		if r.code != 200 {
			t.Fatalf("%s: %d %s", cmd, r.code, r.body)
		}
		r.json(t, &out)
		return out.Output
	}
	out := console("originate user/1004 &park()")
	id, ok := strings.CutPrefix(strings.TrimSpace(out), "+OK ")
	if !ok {
		t.Fatalf("originate: %q", out)
	}
	if r := h.do("GET", p+"/channels", "", ""); !strings.Contains(r.body, id) {
		t.Fatal("originated channel not listed")
	}
	if out := console("uuid_kill " + id); out != "+OK\n" {
		t.Fatalf("uuid_kill: %q", out)
	}
	waitFor(t, "the hang-up record", func() bool {
		return h.do("GET", p+"/hangups/"+id, "", "").code == 200
	})
	if out := console("bogus"); out != "-ERR bogus Command not found!\n" {
		t.Fatalf("bogus: %q", out)
	}
	expect(t, h.do("DELETE", p+"/channels/"+id, "", ""), 502, `{"error":"-ERR No such channel!"}`)

	expect(t, h.do("POST", p+"/actions", "", `{"action":"sofia_profile","op":"stop","profile":"internal"}`), 200, `{"output":"stopping: internal"}`)
	if r := h.do("GET", p+"/sofia", "", ""); strings.Contains(r.body, `"name":"internal"`) {
		t.Fatalf("internal still running: %s", r.body)
	}
	expect(t, h.do("POST", p+"/actions", "", `{"action":"setvar","name":"visitor_note","value":"hi"}`), 200, "")
	h.sw(demoEast).Advance(2*time.Minute + time.Second)
	if r := h.do("GET", p+"/sofia", "", ""); !strings.Contains(r.body, `"name":"internal"`) {
		t.Fatalf("internal not back: %s", r.body)
	}
	if r := h.do("GET", p+"/vars", "", ""); strings.Contains(r.body, "visitor_note") {
		t.Fatal("visitor variable not undone")
	}
}

var _ pastLoad = (*demo.Switch)(nil)

// TestDemoHistory reads the load history of the simulated switches right
// after the start: two hours of it, but none for pbx-lab, and none for a
// simulated switch outside demo mode.
func TestDemoHistory(t *testing.T) {
	h := newDemoHarness(t)
	start := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC).Unix()
	for _, name := range []string{"pbx-east-1", "pbx-west-2"} {
		var got historyReply
		h.do("GET", "/api/instances/"+h.ids[name]+"/history", "", "").json(t, &got)
		if len(got.Samples) != historySize {
			t.Fatalf("%s: %d samples", name, len(got.Samples))
		}
		for i, s := range got.Samples {
			if i > 0 && s.T != got.Samples[i-1].T+20 || s.Idle < 97 {
				t.Fatalf("%s: sample %d %+v", name, i, s)
			}
		}
		if end := got.Samples[historySize-1].T; end > start || end <= start-20 {
			t.Errorf("%s: ends at %d, started at %d", name, end, start)
		}
	}
	expect(t, h.do("GET", "/api/instances/"+h.ids["pbx-lab"]+"/history", "", ""), 200, `{"samples":[]}`)

	n := newHarness(t, func(c *Config) {
		c.Dial = func(string, string) Switch { return demo.New(demoEast, 1, time.Now()) }
	})
	expect(t, n.do("GET", "/api/instances/"+n.id+"/history", n.login("viewer", viewerPW), ""), 200, `{"samples":[]}`)
}

func TestDemoLab(t *testing.T) {
	h := newDemoHarness(t)
	p := "/api/instances/" + h.ids["pbx-lab"]
	var st struct{ State esl.State }
	h.do("GET", p+"/status", "", "").json(t, &st)
	if st.State.Connected || !strings.Contains(st.State.LastError, "connection refused") {
		t.Fatalf("lab state %+v", st.State)
	}
	expect(t, h.do("GET", p+"/sofia", "", ""), 503, `{"error":"instance not connected"}`)
	var list []instanceView
	h.do("GET", "/api/instances", "", "").json(t, &list)
	if len(list) != 3 || !list[0].State.Connected || list[2].State.Connected || list[0].ConfigMode != store.ConfigLocal {
		body, _ := json.Marshal(list)
		t.Fatalf("instances %s", body)
	}
}
