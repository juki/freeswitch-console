package demo

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/juki/freeswitch-console/internal/conffs"
	"github.com/juki/freeswitch-console/internal/esl"
	"github.com/juki/freeswitch-console/internal/fsapi"
)

const (
	eastAddr = "192.0.2.11:8021"
	westAddr = "198.51.100.21:8021"
	labAddr  = "203.0.113.30:8021"
)

var start = time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

func api(t *testing.T, s *Switch, cmd string) string {
	t.Helper()
	out, err := s.API(context.Background(), cmd)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return out
}

// recorder drains a subscription of s into slices while the test advances
// the clock in steps.
type recorder struct {
	events, logs <-chan esl.Message
	cancel       func()
	hangups      []map[string]string
	sip          []map[string]string // CUSTOM events
	beats        int
	lines        []esl.Message
}

func record(s *Switch) *recorder {
	ev, lg, cancel := s.Subscribe(1 << 16)
	return &recorder{events: ev, logs: lg, cancel: cancel}
}

func (r *recorder) drain() {
	for {
		select {
		case m := <-r.events:
			switch m.Headers["Event-Name"] {
			case "CHANNEL_HANGUP_COMPLETE":
				r.hangups = append(r.hangups, m.Headers)
			case "HEARTBEAT":
				r.beats++
			case "CUSTOM":
				r.sip = append(r.sip, m.Headers)
			}
		case m := <-r.logs:
			r.lines = append(r.lines, m)
		default:
			return
		}
	}
}

// run advances s by total in steps, draining r after each.
func (r *recorder) run(s *Switch, total, step time.Duration) {
	for d := time.Duration(0); d < total; d += step {
		s.Advance(step)
		r.drain()
	}
}

// TestParsers runs every fsapi parser the console uses against the answers
// of both connected switches.
func TestParsers(t *testing.T) {
	ctx := context.Background()
	for _, addr := range []string{eastAddr, westAddr} {
		s := New(addr, 7, start)
		defer s.Close()
		s.Advance(time.Minute)
		if _, err := fsapi.Status(ctx, s); err != nil {
			t.Errorf("%s status: %v", addr, err)
		}
		if h, err := fsapi.Hostname(ctx, s); err != nil || h != s.sp.name {
			t.Errorf("%s hostname %q %v", addr, h, err)
		}
		for _, kind := range []string{"registrations", "channels", "calls", "modules"} {
			rows, err := fsapi.Rows(ctx, s, kind)
			if err != nil || len(rows) == 0 && kind != "calls" && kind != "channels" {
				t.Errorf("%s %s: %d rows, %v", addr, kind, len(rows), err)
			}
		}
		if rows, err := fsapi.SofiaStatus(ctx, s); err != nil || len(rows) < 4 {
			t.Errorf("%s sofia: %v %v", addr, rows, err)
		}
		gws, err := fsapi.SofiaGateways(ctx, s)
		if err != nil || len(gws) != len(s.sp.gateways) || gws[0]["profile"] != "external" || gws[0]["calls-in"] == "" {
			t.Errorf("%s gateways: %v %v", addr, gws, err)
		}
		vars, err := fsapi.GlobalVars(ctx, s)
		if err != nil || len(vars) < 20 {
			t.Errorf("%s vars: %d %v", addr, len(vars), err)
		}
		for _, v := range vars {
			if v.Name == "default_password" && v.Value != fsapi.Mask {
				t.Errorf("%s: default_password not masked: %q", addr, v.Value)
			}
		}
		configs, err := fsapi.ConfigList(ctx, s)
		if err != nil || len(configs) != 5 {
			t.Errorf("%s configs: %v %v", addr, configs, err)
		}
		for _, c := range configs {
			if doc, err := fsapi.Config(ctx, s, c.Name); err != nil || !strings.Contains(doc, `name="`+c.Name+`"`) {
				t.Errorf("%s config %s: %v", addr, c.Name, err)
			}
		}
		contexts, err := fsapi.DialplanContexts(ctx, s)
		if err != nil || len(contexts) != 2 || contexts[0].Name != "default" || contexts[0].Count != 4 {
			t.Errorf("%s contexts: %v %v", addr, contexts, err)
		}
		for _, c := range contexts {
			if _, err := fsapi.DialplanContext(ctx, s, c.Name); err != nil {
				t.Errorf("%s context %s: %v", addr, c.Name, err)
			}
		}
		domains, err := fsapi.DirectoryDomains(ctx, s)
		if err != nil || len(domains) != 1 || domains[0].Name != s.sp.ip || domains[0].Count != 13 {
			t.Errorf("%s domains: %v %v", addr, domains, err)
		}
		if doc, err := fsapi.DirectoryDomain(ctx, s, s.sp.ip); err != nil || strings.Contains(doc, "demo-only-1234") || !strings.Contains(doc, `<user id="1010">`) {
			t.Errorf("%s domain: %v\n%s", addr, err, doc)
		}
		if _, err := fsapi.DirectoryDomain(ctx, s, "nope"); err == nil {
			t.Errorf("%s: unknown domain found", addr)
		}
		cmds, err := fsapi.APICommands(ctx, s)
		if err != nil || len(cmds) != len(commandTable) {
			t.Errorf("%s commands: %d %v", addr, len(cmds), err)
		}
		for line, want := range map[string]string{
			"":                        "console_complete_xml",
			"sh":                      "show",
			"show ch":                 "channels",
			"sofia profile ":          "internal",
			"sofia profile internal ": "siptrace",
			"sofia status gateway ":   "carrier-a",
			"load mod_s":              "mod_spandsp",
		} {
			c, err := fsapi.Complete(ctx, s, line)
			if err != nil || !contains(c.Matches, want) {
				t.Errorf("%s complete %q: %+v %v", addr, line, c, err)
			}
		}
		if c, _ := fsapi.Complete(ctx, s, "show cha"); c.Replace != 3 || c.Text != "channels " {
			t.Errorf("%s: unique completion %+v", addr, c)
		}
		if c, _ := fsapi.Complete(ctx, s, "show c"); c.Replace != 1 || c.Text != "c" || len(c.Matches) != 2 {
			t.Errorf("%s: shared prefix %+v", addr, c)
		}
		for _, cmd := range []string{"reloadxml", "reloadacl", "sofia profile internal rescan", "fsctl loglevel 6", "status", "version", "uptime", "sofia status", "show channels", "show calls", "show registrations", "sofia status profile internal", "sofia status gateway carrier-a"} {
			if out, err := fsapi.Run(ctx, s, cmd); err != nil || out == "" {
				t.Errorf("%s %s: %q %v", addr, cmd, out, err)
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestEastShape(t *testing.T) {
	s := New(eastAddr, 3, start)
	defer s.Close()
	if n := len(s.channels); n < 16 || n > 40 {
		t.Errorf("east starts with %d channels", n)
	}
	if !strings.Contains(api(t, s, "status"), "UP 0 years, 12 days,") {
		t.Errorf("status %s", api(t, s, "status"))
	}
	if !strings.Contains(api(t, s, "version"), "1.10.12") || !strings.Contains(api(t, New(westAddr, 1, start), "version"), "1.11.3") {
		t.Error("versions")
	}
	gws := api(t, s, "sofia xmlstatus gateway")
	for _, want := range []string{"<name>carrier-a</name>", "<state>REGED</state>", "<state>NOREG</state>", "<state>FAIL_WAIT</state>", "<status>DOWN</status>", "<retry>"} {
		if !strings.Contains(gws, want) {
			t.Errorf("gateways lack %s:\n%s", want, gws)
		}
	}
	st := api(t, s, "sofia xmlstatus")
	for _, want := range []string{":5060</data>", ":7443;transport=wss</data>", "(WSS)", ":5080</data>", "<type>alias</type>"} {
		if !strings.Contains(st, want) {
			t.Errorf("sofia lacks %s:\n%s", want, st)
		}
	}
	if regs := api(t, s, "show registrations count"); regs != "\n14 total.\n\n" {
		t.Errorf("registrations %q", regs)
	}
	minN, maxN := 1000, 0
	for i := 0; i < 360; i++ { // two hours
		s.Advance(20 * time.Second)
		minN, maxN = min(minN, len(s.channels)), max(maxN, len(s.channels))
	}
	if minN < 4 || maxN > 40 {
		t.Errorf("east ran with %d to %d channels, want about 8 to 40", minN, maxN)
	}
	w := New(westAddr, 3, start)
	defer w.Close()
	for i := 0; i < 360; i++ {
		w.Advance(20 * time.Second)
		if len(w.channels) > 6 {
			t.Fatalf("west has %d channels", len(w.channels))
		}
	}
	if regs := api(t, w, "show registrations count"); regs != "\n6 total.\n\n" {
		t.Errorf("west registrations %q", regs)
	}
}

// TestCallsAndHangups checks that every channel ends with one hang-up event
// and log lines of its own, and that the calls still running are listed.
func TestCallsAndHangups(t *testing.T) {
	s := New(eastAddr, 11, start)
	defer s.Close()
	s.SetLogLevel(7)
	r := record(s)
	r.run(s, 15*time.Minute, time.Second)

	seen := map[string]bool{}
	for _, h := range r.hangups {
		id := h["Unique-ID"]
		if seen[id] {
			t.Fatalf("two hang-ups for %s", id)
		}
		seen[id] = true
		if h["Hangup-Cause"] == "" || h["variable_hangup_cause_q850"] == "" || h["Caller-Destination-Number"] == "" || h["variable_duration"] == "" {
			t.Errorf("hang-up lacks fields: %v", h)
		}
		if _, open := s.channels[id]; open {
			t.Errorf("%s hung up but still listed", id)
		}
	}
	if len(r.hangups) < 100 {
		t.Fatalf("%d hang-ups in 15 minutes", len(r.hangups))
	}
	lines := map[string]int{}
	created := map[string]bool{}
	for _, m := range r.lines {
		lines[m.Headers["User-Data"]]++
		if strings.Contains(m.Body, "] New Channel") || strings.Contains(m.Body, " New Channel ") {
			created[m.Headers["User-Data"]] = true
		}
	}
	for id := range seen {
		if lines[id] < 3 {
			t.Errorf("%s has %d log lines", id, lines[id])
		}
	}
	for id := range created {
		if _, open := s.channels[id]; !open && !seen[id] {
			t.Errorf("%s was created and is gone without a hang-up", id)
		}
	}
	levels := map[string]bool{}
	for _, m := range r.lines {
		levels[m.Headers["Log-Level"]] = true
	}
	for _, l := range []string{"3", "4", "5", "6", "7"} {
		if !levels[l] {
			t.Errorf("no log line at level %s", l)
		}
	}
	if r.beats != 45 {
		t.Errorf("%d heartbeats in 15 minutes", r.beats)
	}
	calls, err := fsapi.Rows(context.Background(), s, "calls")
	if err != nil || len(calls) == 0 {
		t.Fatalf("calls %v %v", calls, err)
	}
}

// TestCauseMix runs a few thousand calls: every cause of the mix occurs,
// in roughly its share, with its SIP status.
func TestCauseMix(t *testing.T) {
	s := New(eastAddr, 5, start)
	defer s.Close()
	r := record(s)
	r.run(s, 10*time.Hour, time.Minute)
	causes := map[string]int{}
	calls := 0
	status := map[string]string{}
	for _, h := range r.hangups {
		if h["Unique-ID"] != h["Channel-Call-UUID"] {
			continue // count calls by their a-leg
		}
		calls++
		cause := h["Hangup-Cause"]
		if f := h["variable_originate_failed_cause"]; f != "" {
			cause += "/" + f
		}
		causes[cause]++
		status[cause] = h["variable_sip_term_status"]
		if h["variable_billsec"] != "0" {
			mos, _ := strconv.ParseFloat(h["variable_rtp_audio_in_mos"], 64)
			codec := h["variable_read_codec"]
			if codec != "PCMU" && codec != "PCMA" && codec != "OPUS" || mos < 1.9 || mos > 4.4 {
				t.Errorf("answered call with codec %q and MOS %v", codec, mos)
			}
		}
	}
	if calls < 3000 {
		t.Fatalf("%d calls in 10 hours", calls)
	}
	want := map[string]string{
		"NORMAL_CLEARING": "200", "ORIGINATOR_CANCEL": "487", "NO_ANSWER": "480", "USER_BUSY": "486", "CALL_REJECTED": "603",
		"NO_ROUTE_DESTINATION": "404", "NORMAL_CLEARING/USER_NOT_REGISTERED": "200", "RECOVERY_ON_TIMER_EXPIRE": "408",
		"MEDIA_TIMEOUT": "200", "INCOMPATIBLE_DESTINATION": "488",
	}
	for cause, st := range want {
		if causes[cause] == 0 || status[cause] != st {
			t.Errorf("%s: %d calls, SIP status %q, want %s", cause, causes[cause], status[cause], st)
		}
	}
	if share := float64(causes["NORMAL_CLEARING"]) / float64(calls); share < 0.72 || share > 0.84 {
		t.Errorf("NORMAL_CLEARING share %.3f of %d calls: %v", share, calls, causes)
	}
	t.Logf("%d calls: %v", calls, causes)
}

func TestDeterministic(t *testing.T) {
	ids := func() []string {
		s := New(eastAddr, 42, start)
		defer s.Close()
		r := record(s)
		r.run(s, 10*time.Minute, 5*time.Second)
		var out []string
		for _, h := range r.hangups {
			out = append(out, h["Unique-ID"]+h["Hangup-Cause"])
		}
		return out
	}
	a, b := ids(), ids()
	if strings.Join(a, ",") != strings.Join(b, ",") || len(a) == 0 {
		t.Fatalf("two runs differ: %d and %d hang-ups", len(a), len(b))
	}
}

func TestSetLogLevel(t *testing.T) {
	s := New(eastAddr, 9, start)
	defer s.Close()
	r := record(s)
	r.run(s, 5*time.Minute, time.Second)
	if len(r.lines) != 0 {
		t.Fatalf("%d log lines before SetLogLevel", len(r.lines))
	}
	s.SetLogLevel(5)
	r.run(s, 5*time.Minute, time.Second)
	if len(r.lines) == 0 {
		t.Fatal("no log lines at level 5")
	}
	for _, m := range r.lines {
		if l, _ := strconv.Atoi(m.Headers["Log-Level"]); l > 5 {
			t.Fatalf("level %d line at log level 5: %s", l, m.Body)
		}
	}
	// fsctl loglevel lowers what the core logs at all.
	r.lines = nil
	s.SetLogLevel(7)
	api(t, s, "fsctl loglevel 4")
	r.run(s, time.Minute, time.Second) // less than the two minutes the change lasts
	for _, m := range r.lines {
		if l, _ := strconv.Atoi(m.Headers["Log-Level"]); l > 4 {
			t.Fatalf("level %d line at core level 4: %s", l, m.Body)
		}
	}
	s.SetLogLevel(-1)
	r.lines = nil
	r.run(s, time.Minute, time.Second)
	if len(r.lines) != 0 {
		t.Fatal("log lines after SetLogLevel(-1)")
	}
}

// TestVisitorChangesHeal makes the changes the actions offer and checks that
// they take effect and are undone after two minutes.
func TestVisitorChangesHeal(t *testing.T) {
	s := New(eastAddr, 13, start)
	defer s.Close()
	ctx := context.Background()
	profiles := func() string { return api(t, s, "sofia xmlstatus") }
	modules := func() string { return api(t, s, "show modules") }

	if out := api(t, s, "sofia profile internal stop"); !strings.HasPrefix(out, "stopping") {
		t.Fatalf("stop: %q", out)
	}
	if strings.Contains(profiles(), "<name>internal</name>") || api(t, s, "show registrations count") != "\n0 total.\n\n" {
		t.Fatal("internal still listed after stop")
	}
	for _, ch := range s.channels {
		if ch.profile == "internal" {
			t.Fatalf("channel %s left on the stopped profile", ch.name)
		}
	}
	api(t, s, "unload mod_conference")
	if strings.Contains(modules(), "mod_conference") {
		t.Fatal("mod_conference still listed")
	}
	if out := api(t, s, "unload mod_sofia"); !strings.HasPrefix(out, "-ERR") {
		t.Fatalf("unload mod_sofia: %q", out)
	}
	api(t, s, "global_setvar demo_visitor=hello")
	api(t, s, "global_setvar outbound_caller_name=Visitor")
	if v := api(t, s, "global_getvar demo_visitor"); v != "hello" {
		t.Fatalf("var %q", v)
	}
	api(t, s, "sofia profile external killgw carrier-b")
	api(t, s, "sofia profile external unregister carrier-a")
	api(t, s, "fsctl max_sessions 5")
	api(t, s, "fsctl pause")
	gws, _ := fsapi.SofiaGateways(ctx, s)
	if len(gws) != 2 || gws[0]["state"] != "UNREGED" {
		t.Fatalf("gateways after kill and unregister: %v", gws)
	}
	if st := api(t, s, "status"); !strings.Contains(st, "is not ready") || !strings.Contains(st, "5 session(s) max") {
		t.Fatalf("status %s", st)
	}

	s.Advance(healAfter - time.Second)
	if strings.Contains(profiles(), "<name>internal</name>") {
		t.Fatal("healed early")
	}
	s.Advance(2 * time.Second)
	if !strings.Contains(profiles(), "<name>internal</name>") || api(t, s, "show registrations count") != "\n14 total.\n\n" {
		t.Fatal("internal not back after 2 minutes")
	}
	if !strings.Contains(modules(), "mod_conference") {
		t.Fatal("mod_conference not back")
	}
	if v := api(t, s, "global_getvar demo_visitor"); v != "" || strings.Contains(api(t, s, "global_getvar"), "demo_visitor") {
		t.Fatalf("visitor variable still there: %q", v)
	}
	if v := api(t, s, "global_getvar outbound_caller_name"); v != "Demo PBX" {
		t.Fatalf("outbound_caller_name %q", v)
	}
	gws, _ = fsapi.SofiaGateways(ctx, s)
	if len(gws) != 3 || gws[0]["state"] != "REGED" {
		t.Fatalf("gateways not healed: %v", gws)
	}
	if st := api(t, s, "status"); !strings.Contains(st, "is ready") || !strings.Contains(st, "1000 session(s) max") {
		t.Fatalf("status not healed: %s", st)
	}
	if len(s.changes) != 0 {
		t.Fatalf("changes left: %d", len(s.changes))
	}

	// A second change of the same thing moves the healing on and still
	// restores the first state.
	api(t, s, "sofia profile internal stop")
	s.Advance(time.Minute)
	api(t, s, "sofia profile internal start")
	api(t, s, "sofia profile internal stop")
	s.Advance(time.Minute + 30*time.Second)
	if strings.Contains(profiles(), "<name>internal</name>") {
		t.Fatal("healed from the first change")
	}
	s.Advance(time.Minute)
	if !strings.Contains(profiles(), "<name>internal</name>") {
		t.Fatal("not healed")
	}
}

func TestSipTrace(t *testing.T) {
	s := New(westAddr, 2, start)
	defer s.Close()
	s.SetLogLevel(0)
	r := record(s)
	if out := api(t, s, "sofia profile internal siptrace on"); out != "Enabled sip debugging on internal\n" {
		t.Fatalf("siptrace: %q", out)
	}
	r.drain()
	if len(r.lines) == 0 || !strings.Contains(r.lines[0].Body, "OPTIONS sip:") || r.lines[0].Headers["Log-Level"] != "0" {
		t.Fatalf("no SIP message at once: %v", r.lines)
	}
	r.run(s, healAfter+time.Second, time.Second)
	n := len(r.lines)
	r.run(s, time.Minute, time.Second)
	if len(r.lines) != n {
		t.Fatalf("SIP messages after the trace healed: %d more", len(r.lines)-n)
	}
}

func TestChannelCap(t *testing.T) {
	s := New(eastAddr, 4, start)
	defer s.Close()
	ok, refused := 0, 0
	for i := 0; i < 100; i++ {
		out := api(t, s, "originate {origination_caller_id_number=1001}user/1002 &park()")
		switch {
		case strings.HasPrefix(out, "+OK "):
			ok++
		case out == "-ERR SWITCH_CONGESTION\n":
			refused++
		default:
			t.Fatalf("originate: %q", out)
		}
		if n := len(s.channels); n > maxChannels {
			t.Fatalf("%d channels", n)
		}
	}
	if len(s.channels) != maxChannels || refused == 0 {
		t.Fatalf("%d channels, %d refused", len(s.channels), refused)
	}
	for i := 0; i < 600; i++ {
		s.Advance(time.Second)
		if n := len(s.channels); n > maxChannels {
			t.Fatalf("%d channels", n)
		}
	}
}

func TestOriginateAndKill(t *testing.T) {
	s := New(westAddr, 8, start)
	defer s.Close()
	r := record(s)
	out := api(t, s, "originate sofia/gateway/carrier-a/12125550142 &echo")
	id, ok := strings.CutPrefix(strings.TrimSpace(out), "+OK ")
	if !ok || !fsapi.ValidUUID(id) {
		t.Fatalf("originate: %q", out)
	}
	if !strings.Contains(api(t, s, "show channels as json"), id) {
		t.Fatal("originated channel not listed")
	}
	s.Advance(10 * time.Second)
	if out := api(t, s, "uuid_kill "+id); out != "+OK\n" {
		t.Fatalf("uuid_kill: %q", out)
	}
	if out := api(t, s, "uuid_kill "+id); out != "-ERR No such channel!\n" {
		t.Fatalf("second uuid_kill: %q", out)
	}
	r.drain()
	var h map[string]string
	for _, x := range r.hangups {
		if x["Unique-ID"] == id {
			h = x
		}
	}
	if h == nil || h["Hangup-Cause"] != "NORMAL_CLEARING" || h["variable_sip_gateway_name"] != "carrier-a" || h["variable_billsec"] == "0" {
		t.Fatalf("hang-up %v", h)
	}
	for cmd, want := range map[string]string{
		"originate user/1009 &park()":                  "-ERR USER_NOT_REGISTERED\n",
		"originate user/4242 &park()":                  "-ERR SUBSCRIBER_ABSENT\n",
		"originate sofia/gateway/nope/1 &park()":       "-ERR INVALID_GATEWAY\n",
		"originate verto.rtc/1001 &park()":             "-ERR CHAN_NOT_IMPLEMENTED\n",
		"originate user/1001":                          "-USAGE: ",
		"uuid_kill":                                    "-USAGE: ",
		"originate loopback/9196 &park()":              "+OK ",
		"sofia profile external killgw carrier-backup": "-ERR Invalid gateway!\n",
	} {
		if out := api(t, s, cmd); !strings.HasPrefix(out, want) {
			t.Errorf("%s: %q, want %q", cmd, out, want)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	s := New(eastAddr, 1, start)
	defer s.Close()
	for cmd, want := range map[string]string{
		"bogus":            "-ERR bogus Command not found!\n",
		"conference list":  "-ERR conference Command not found!\n",
		"show nothing":     "-USAGE: ",
		"sofia frobnicate": "-USAGE: sofia",
		"xml_locate configuration configuration name nope.conf": "can't find anything\n",
	} {
		if out := api(t, s, cmd); !strings.HasPrefix(out, want) {
			t.Errorf("%s: %q, want %q", cmd, out, want)
		}
	}
	if _, err := s.API(context.Background(), "status\nbogus"); err != esl.ErrInvalidCommand {
		t.Errorf("line break: %v", err)
	}
}

type beat struct {
	at       time.Time
	sessions int
	rate     int
	idle     float64
}

func history(s *Switch) []beat {
	var out []beat
	s.History(func(at time.Time, sessions, rate int, idle float64) {
		out = append(out, beat{at: at, sessions: sessions, rate: rate, idle: idle})
	})
	return out
}

// TestHistory checks the two hours before the start: one beat per heartbeat
// up to the first live one, within the switch's range, and joining it.
func TestHistory(t *testing.T) {
	for _, tc := range []struct {
		addr          string
		lo, hi, swing int // the range, and how far the day must swing in it
		tolerance     int // channels between the last beat and the first live one
	}{
		{eastAddr, 8, 40, 24, 8},
		{westAddr, 0, 6, 4, 2},
	} {
		s := New(tc.addr, 1, start)
		h := history(s)
		if len(h) != historyBeats {
			t.Fatalf("%s: %d beats", tc.addr, len(h))
		}
		last := h[len(h)-1]
		if last.at.After(start) || !last.at.After(start.Add(-heartbeatEvery)) {
			t.Errorf("%s: the last beat at %v", tc.addr, last.at)
		}
		lo, hi := 1000, -1
		for i, b := range h {
			if i > 0 && !b.at.Equal(h[i-1].at.Add(heartbeatEvery)) {
				t.Fatalf("%s: beat %d at %v after %v", tc.addr, i, b.at, h[i-1].at)
			}
			if b.sessions < tc.lo || b.sessions > tc.hi || b.rate < 0 || b.rate > 2 || b.idle < 97 || b.idle > 99.4 {
				t.Fatalf("%s: beat %d %+v", tc.addr, i, b)
			}
			lo, hi = min(lo, b.sessions), max(hi, b.sessions)
		}
		if hi-lo < tc.swing {
			t.Errorf("%s: %d to %d sessions, no day in it", tc.addr, lo, hi)
		}
		if again := history(New(tc.addr, 1, start)); !reflect.DeepEqual(again, h) {
			t.Errorf("%s: not deterministic", tc.addr)
		}
		if other := history(New(tc.addr, 2, start)); reflect.DeepEqual(other, h) {
			t.Errorf("%s: the same for another seed", tc.addr)
		}

		events, _, cancel := s.Subscribe(16)
		var first map[string]string
		for first == nil {
			s.Advance(time.Second)
			for len(events) > 0 {
				if m := <-events; m.Headers["Event-Name"] == "HEARTBEAT" {
					first = m.Headers
				}
			}
		}
		cancel()
		n, _ := strconv.Atoi(first["Session-Count"])
		idle, _ := strconv.ParseFloat(first["Idle-CPU"], 64)
		at, _ := strconv.ParseInt(first["Event-Date-Timestamp"], 10, 64)
		if d := n - last.sessions; d < -tc.tolerance || d > tc.tolerance || math.Abs(idle-last.idle) > 1 {
			t.Errorf("%s: last beat %+v, first live heartbeat %d sessions, idle %.2f", tc.addr, last, n, idle)
		}
		if !time.UnixMicro(at).Equal(last.at.Add(heartbeatEvery)) {
			t.Errorf("%s: first live heartbeat at %v, last beat at %v", tc.addr, time.UnixMicro(at), last.at)
		}
		s.Close()
		if h := history(s); len(h) != 0 {
			t.Errorf("%s: %d beats once closed", tc.addr, len(h))
		}
	}
	if h := history(New(labAddr, 1, start)); len(h) != 0 {
		t.Errorf("lab: %d beats", len(h))
	}
}

func TestDown(t *testing.T) {
	for _, addr := range []string{labAddr, "192.0.2.99:8021"} {
		s := Dial(addr, "x")
		st := s.State()
		if st.Connected || !strings.HasSuffix(st.LastError, "connect: connection refused") || !strings.Contains(st.LastError, addr) {
			t.Errorf("%s state %+v", addr, st)
		}
		if _, err := s.API(context.Background(), "status"); err != esl.ErrNotConnected {
			t.Errorf("%s API: %v", addr, err)
		}
		s.Close()
	}
}

// TestCloseStopsGoroutines closes live switches: nothing keeps running and
// the subscribers' channels are closed.
func TestCloseStopsGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	var subs []<-chan esl.Message
	for range 5 {
		s := Dial(eastAddr, "x")
		s.SetLogLevel(7)
		ev, lg, _ := s.Subscribe(16)
		subs = append(subs, ev, lg)
		time.Sleep(10 * time.Millisecond)
		s.Close()
		s.Close()
		if _, err := s.API(context.Background(), "status"); err != esl.ErrNotConnected {
			t.Fatalf("API after Close: %v", err)
		}
		if st := s.State(); st.Connected || st.LastError != "esl: client closed" {
			t.Fatalf("state after Close %+v", st)
		}
	}
	for _, ch := range subs {
		for range ch {
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("%d goroutines before, %d after Close", before, n)
	}
}

// TestBounded runs a day: the event queue and the state stay small.
func TestBounded(t *testing.T) {
	s := New(eastAddr, 21, start)
	defer s.Close()
	maxQueue := 0
	for i := 0; i < 24*60; i++ {
		s.Advance(time.Minute)
		if i%30 == 0 {
			api(t, s, "global_setvar v"+strconv.Itoa(i)+"=x")
			api(t, s, "sofia profile internal siptrace on")
		}
		maxQueue = max(maxQueue, len(s.queue))
	}
	if maxQueue > 200 || len(s.changes) > 2 || len(s.vars) > len(s.conf.vars)+1 {
		t.Fatalf("queue up to %d, %d changes, %d vars", maxQueue, len(s.changes), len(s.vars))
	}
	for i := 0; i < 100; i++ {
		api(t, s, "global_setvar many"+strconv.Itoa(i)+"=x")
	}
	if len(s.vars) > len(s.conf.vars)+maxOwnVars {
		t.Fatalf("%d variables", len(s.vars))
	}
	// Calls and traces a visitor ends at once leave nothing in the queue.
	before := len(s.queue)
	for i := 0; i < 2000; i++ {
		out := api(t, s, "originate user/1003 &park()")
		if id, ok := strings.CutPrefix(strings.TrimSpace(out), "+OK "); ok {
			api(t, s, "uuid_kill "+id)
		}
		api(t, s, "sofia profile internal siptrace on")
		api(t, s, "sofia profile internal siptrace off")
	}
	if len(s.queue) > before+1 {
		t.Fatalf("queue grew from %d to %d", before, len(s.queue))
	}
}

func TestFiles(t *testing.T) {
	ctx := context.Background()
	var f Files
	root, err := f.List(ctx, "")
	if err != nil || len(root) != 6 || !root[0].Dir || root[len(root)-1].Name != "vars.xml" {
		t.Fatalf("root %v %v", root, err)
	}
	if _, err := f.List(ctx, "nope"); !errors.Is(err, conffs.ErrNotFound) {
		t.Errorf("missing dir: %v", err)
	}
	if _, err := f.List(ctx, "../x"); !errors.Is(err, conffs.ErrInvalid) {
		t.Errorf("escaping dir: %v", err)
	}
	file, err := f.Read(ctx, "sip_profiles/external/carrier-a.xml")
	if err != nil || !strings.Contains(file.Content, `<gateway name="carrier-a">`) || len(file.SHA256) != 64 || file.Size != int64(len(file.Content)) {
		t.Fatalf("read %+v %v", file, err)
	}
	if _, err := f.Read(ctx, "nope.xml"); !errors.Is(err, conffs.ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
	res, err := f.Search(ctx, `<user id="1004"`, conffs.SearchOptions{CaseSensitive: true})
	if err != nil || len(res.Rows) != 2 || res.Rows[0].Path != "directory/default.xml" || res.Rows[1].Path != "directory/default/1004.xml" || res.Rows[1].Line != 2 {
		t.Fatalf("search %+v %v", res, err)
	}
	if res, _ := f.Search(ctx, "PARAM", conffs.SearchOptions{Limit: 3}); len(res.Rows) != 3 || !res.Truncated {
		t.Fatalf("limited search %+v", res)
	}
	if h, err := f.History(ctx, "vars.xml"); err != nil || h == nil || len(h) != 0 {
		t.Fatalf("history %v %v", h, err)
	}
	_, werr := f.Write(ctx, "vars.xml", "<include/>", file.SHA256, "demo")
	derr := f.Delete(ctx, "vars.xml", file.SHA256, "demo")
	_, rerr := f.Restore(ctx, "vars.xml", "x", file.SHA256, "demo")
	for _, err := range []error{werr, derr, rerr} {
		w := httptest.NewRecorder()
		conffs.WriteError(w, err)
		if w.Code != 403 || strings.TrimSpace(w.Body.String()) != `{"error":"disabled in the demo"}` {
			t.Errorf("%v answered %d %s", err, w.Code, w.Body)
		}
	}
}

// TestSampleConfiguration checks the sample files: well-formed, and
// consistent with what the simulation reports.
func TestSampleConfiguration(t *testing.T) {
	ctx := context.Background()
	var f Files
	var walk func(dir string)
	count := 0
	walk = func(dir string) {
		entries, err := f.List(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			p := strings.TrimPrefix(dir+"/"+e.Name, "/")
			if e.Dir {
				walk(p)
				continue
			}
			file, err := f.Read(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if err := conffs.CheckXML(file.Content); err != nil {
				t.Errorf("%s: %v", p, err)
			}
			count++
		}
	}
	walk("")
	if count != 25 {
		t.Errorf("%d sample files", count)
	}

	s := New(eastAddr, 1, start)
	defer s.Close()
	dir := api(t, s, "xml_locate directory")
	for _, u := range users {
		if !strings.Contains(dir, `<user id="`+u.id+`">`) || !strings.Contains(dir, `value="`+u.name+`"`) {
			t.Errorf("user %s %q not in the directory", u.id, u.name)
		}
	}
	conf := api(t, s, "xml_locate configuration")
	for _, g := range gatewaySpecs {
		if !strings.Contains(conf, `<gateway name="`+g.name+`">`) || !strings.Contains(conf, `value="`+g.proxy+`"`) || !strings.Contains(conf, `value="`+g.username+`"`) {
			t.Errorf("gateway %s not in sofia.conf as simulated", g.name)
		}
	}
	for _, m := range s.modules {
		if strings.Contains(conf, `<load module="`+m.name+`"/>`) != m.loaded {
			t.Errorf("%s: loaded %v, but modules.conf says otherwise", m.name, m.loaded)
		}
	}
	plan := api(t, s, "xml_locate dialplan")
	for _, ext := range append([][2]string{extLocal, extOutbound, extInbound}, extOthers...) {
		if !strings.Contains(plan, `<extension name="`+ext[0]+`">`) || !strings.Contains(plan, `expression="`+ext[1]+`"`) {
			t.Errorf("extension %s %s not in the dialplan", ext[0], ext[1])
		}
	}
	for _, u := range users {
		if d := did(u.id); !strings.HasPrefix(d, "+131255501") || len(d) != 12 {
			t.Errorf("did(%s) = %s", u.id, d)
		}
	}
	for _, want := range []string{`name="sip-port" value="5060"`, `value=":7443"`, `name="sip-port" value="5080"`, `name="max-sessions" value="1000"`, `name="sessions-per-second" value="30"`} {
		if !strings.Contains(conf, want) {
			t.Errorf("configuration lacks %s", want)
		}
	}
}

// docNets are the documentation ranges every address of the demo is in,
// besides the office phones' private addresses.
var docNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"} {
		_, n, _ := net.ParseCIDR(cidr)
		out = append(out, n)
	}
	return out
}()

func documentation(ip string) bool {
	a := net.ParseIP(ip)
	for _, n := range docNets {
		if a != nil && n.Contains(a) {
			return true
		}
	}
	return false
}

// checkSipAddresses fails unless every address of the events h of the
// switch at addr is a documentation address, or for a registration of one
// of its phones that phone's address.
func checkSipAddresses(t *testing.T, addr string, events []map[string]string) {
	t.Helper()
	phones := map[string]bool{}
	for _, r := range specAt(addr).regs {
		phones[r.ip] = true
	}
	for _, h := range events {
		for _, k := range []string{"network-ip", "Register-Network-IP"} {
			ip, ok := h[k]
			if !ok {
				continue
			}
			sub := h["Event-Subclass"]
			if !documentation(ip) && (sub == "sofia::register_failure" || sub == "sofia::gateway_state" || !phones[ip]) {
				t.Fatalf("%s from %s: %v", sub, ip, h)
			}
		}
	}
}

// TestSipEvents runs east for three hours and a visitor's killgw: every kind
// of SIP event appears, in the shapes mod_sofia gives them.
func TestSipEvents(t *testing.T) {
	s := New(eastAddr, 5, start)
	defer s.Close()
	r := record(s)
	defer r.cancel()
	r.run(s, 3*time.Hour, 10*time.Second)
	api(t, s, "sofia profile external killgw carrier-b")
	r.run(s, healAfter+time.Second, time.Second)

	kinds := map[string]int{}
	sources := map[string][]int64{} // failure times by source, microseconds
	for _, h := range r.sip {
		sub := h["Event-Subclass"]
		kind, ok := strings.CutPrefix(sub, "sofia::")
		if !ok || h["Core-UUID"] == "" || h["Event-Date-Timestamp"] == "" || h["FreeSWITCH-Hostname"] != "pbx-east-1" {
			t.Fatalf("event %v", h)
		}
		kinds[kind]++
		switch kind {
		case "register", "unregister", "expire":
			if !directoryUser(h["username"]) || h["profile-name"] != "internal" || h["call-id"] == "" || h["user-agent"] == "" || h["network-port"] == "" {
				t.Fatalf("%s %v", kind, h)
			}
			if kind == "unregister" && (h["expires"] != "0" || !strings.Contains(h["user-agent"], "SoftDial")) {
				t.Fatalf("unregister %v", h)
			}
		case "register_failure":
			if h["registration-type"] != "REGISTER" || h["to-host"] != "192.0.2.11" || h["to-user"] == "" || h["user-agent"] == "" {
				t.Fatalf("failure %v", h)
			}
			ts, _ := strconv.ParseInt(h["Event-Date-Timestamp"], 10, 64)
			sources[h["network-ip"]] = append(sources[h["network-ip"]], ts)
		case "gateway_state":
			switch h["Gateway"] {
			case "carrier-backup":
				if st := h["State"]; st != "FAIL_WAIT" && st != "DOWN" || h["Ping-Status"] != "DOWN" || h["Register-Network-IP"] != "" {
					t.Fatalf("carrier-backup %v", h)
				}
			case "carrier-a":
				if h["Ping-Status"] != "UP" || h["Register-Network-IP"] != "198.51.100.40" || (h["State"] == "REGISTER") != (h["Status"] == "200" && h["Phrase"] == "OK") {
					t.Fatalf("carrier-a %v", h)
				}
			default:
				t.Fatalf("gateway %v", h)
			}
		case "gateway_add", "gateway_delete":
			if h["Gateway"] != "carrier-b" || h["profile-name"] != "external" {
				t.Fatalf("%s %v", kind, h)
			}
		default:
			t.Fatalf("kind %s", kind)
		}
	}
	for _, k := range []string{"register", "unregister", "expire", "register_failure", "gateway_state", "gateway_add", "gateway_delete"} {
		if kinds[k] == 0 {
			t.Errorf("no %s in %v", k, kinds)
		}
	}
	if kinds["gateway_add"] != 1 || kinds["gateway_delete"] != 1 {
		t.Errorf("kill and heal: %v", kinds)
	}
	// Two or three sources, each in bursts: many attempts within a minute.
	if len(sources) < 2 || len(sources) > 3 {
		t.Errorf("%d failure sources", len(sources))
	}
	for ip, times := range sources {
		burst := 0
		for i := range times {
			n := 0
			for j := i; j < len(times) && times[j]-times[i] < 60e6; j++ {
				n++
			}
			burst = max(burst, n)
		}
		if burst < 6 {
			t.Errorf("%s: at most %d attempts in a minute", ip, burst)
		}
	}
	checkSipAddresses(t, eastAddr, r.sip)
}

type pastEvent struct {
	at time.Time
	h  map[string]string
}

func sipPrefill(s *Switch) []pastEvent {
	var out []pastEvent
	s.SipEvents(func(at time.Time, h map[string]string) { out = append(out, pastEvent{at, h}) })
	return out
}

// TestSipEventsPrefill checks the hour of SIP events before the start:
// deterministic, oldest first, inside the hour, every kind the simulation
// produces without a visitor, in the documentation ranges.
func TestSipEventsPrefill(t *testing.T) {
	for _, addr := range []string{eastAddr, westAddr} {
		s := New(addr, 1, start)
		past := sipPrefill(s)
		if len(past) < 40 {
			t.Fatalf("%s: %d events", addr, len(past))
		}
		kinds := map[string]int{}
		var events []map[string]string
		lastBackup := ""
		for i, p := range past {
			if !p.at.After(start.Add(-sipPast)) || !p.at.Before(start) || i > 0 && p.at.Before(past[i-1].at) {
				t.Fatalf("%s: event %d at %v", addr, i, p.at)
			}
			if ts := strconv.FormatInt(p.at.UnixMicro(), 10); p.h["Event-Date-Timestamp"] != ts || p.h["Event-Name"] != "CUSTOM" {
				t.Fatalf("%s: event %d %v", addr, i, p.h)
			}
			kinds[strings.TrimPrefix(p.h["Event-Subclass"], "sofia::")]++
			events = append(events, p.h)
			if p.h["Gateway"] == "carrier-backup" {
				lastBackup = p.h["State"]
			}
		}
		for _, k := range []string{"register", "register_failure", "gateway_state"} {
			if kinds[k] == 0 {
				t.Errorf("%s: no %s in %v", addr, k, kinds)
			}
		}
		if kinds["expire"]+kinds["unregister"] == 0 {
			t.Errorf("%s: no phone left: %v", addr, kinds)
		}
		checkSipAddresses(t, addr, events)
		// carrier-backup flapped into the state sofia shows now.
		if addr == eastAddr && (lastBackup == "" || lastBackup != s.gateway("carrier-backup").state) {
			t.Errorf("carrier-backup ends %q, is %s", lastBackup, s.gateway("carrier-backup").state)
		}

		if again := sipPrefill(New(addr, 1, start)); !reflect.DeepEqual(again, past) {
			t.Errorf("%s: not deterministic", addr)
		}
		if other := sipPrefill(New(addr, 2, start)); reflect.DeepEqual(other, past) {
			t.Errorf("%s: the same for another seed", addr)
		}
		s.Close()
		if closed := sipPrefill(s); len(closed) != 0 {
			t.Errorf("%s: %d events once closed", addr, len(closed))
		}
	}
	if lab := sipPrefill(New(labAddr, 1, start)); len(lab) != 0 {
		t.Fatalf("lab: %d events", len(lab))
	}
}
