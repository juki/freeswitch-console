package main

// Integration tests against a real FreeSWITCH. They are skipped unless
// FSCONSOLE_TEST_ESL holds host:port of its Event Socket
// (FSCONSOLE_TEST_ESL_PASSWORD, default ClueCon):
//
//	FSCONSOLE_TEST_ESL=127.0.0.1:8021 go test -run Real -v ./cmd/fsconsole/
//
// The switch must have the stock dialplan (9196 is the echo extension) and
// the stock internal SIP profile. The tests place loopback calls and one SIP
// call to 192.0.2.1 (TEST-NET, never answered), and hang up what they created
// by its uuid; they change no configuration. Other calls on the switch do not
// disturb them: they assert only things about their own channels.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juki/freeswitch-console/internal/esl"
	"github.com/juki/freeswitch-console/internal/fsapi"
	"github.com/juki/freeswitch-console/internal/server"
)

// originateCmd places a loopback call to the echo extension whose a leg has
// the uuid id. The uuid is chosen before the call, so the test can hang the
// channel up even when the originate never answers.
func originateCmd(id string) string {
	return "originate {origination_uuid=" + id + "}loopback/9196/default &park()"
}

// newUUID returns a random uuid.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// realSwitch returns the Event Socket of the real switch, or skips the test.
func realSwitch(t *testing.T) (addr, password string) {
	t.Helper()
	addr = os.Getenv("FSCONSOLE_TEST_ESL")
	if addr == "" {
		t.Skip("FSCONSOLE_TEST_ESL is not set (host:port of a real FreeSWITCH Event Socket)")
	}
	password = os.Getenv("FSCONSOLE_TEST_ESL_PASSWORD")
	if password == "" {
		password = "ClueCon"
	}
	return addr, password
}

// realStack is the whole console, signed in as admin, with one instance that
// dials dialAddr.
type realStack struct {
	t        *testing.T
	c        *testClient
	id, p    string
	addr     string // the real switch, for cleanup
	password string
	codes    *statusLog
}

// startReal starts the console with main's defaults for the backlog and the
// recent calls, adds an instance for dialAddr (the real switch or a forwarder
// to it) and waits until it is connected.
func startReal(t *testing.T, dialAddr string) *realStack {
	t.Helper()
	addr, password := realSwitch(t)
	sk := startConsole(t, func(cfg *server.Config) {
		cfg.Dial = dialer(500)
		cfg.LogBacklog, cfg.LogBacklogLevel, cfg.RecentCalls = 2000, 6, 500
	})
	c := sk.client(t, "admin", "admin-pw")
	codes := &statusLog{base: http.DefaultTransport.(*http.Transport).Clone()}
	c.http.Transport = codes

	host, portText, err := net.SplitHostPort(dialAddr)
	if err != nil {
		t.Fatalf("FSCONSOLE_TEST_ESL %q: %v", dialAddr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("FSCONSOLE_TEST_ESL %q: port: %v", dialAddr, err)
	}
	code, body := c.do("POST", "/api/instances", map[string]any{"name": "real", "host": host, "port": port, "password": password})
	var inst struct{ ID string }
	if code != http.StatusCreated || json.Unmarshal(body, &inst) != nil || inst.ID == "" {
		t.Fatalf("create instance: status %d: %s", code, clip(body))
	}
	r := &realStack{t: t, c: c, id: inst.ID, p: "/api/instances/" + inst.ID, addr: addr, password: password, codes: codes}
	if _, ok := r.waitState(true, 10*time.Second); !ok {
		t.Fatalf("instance never connected to %s: %+v", dialAddr, r.state())
	}
	return r
}

// state is the instance's connection state from the instance list, which the
// server answers without asking the switch.
func (r *realStack) state() esl.State {
	r.t.Helper()
	var list []struct {
		ID    string
		State esl.State
	}
	r.c.get("/api/instances", http.StatusOK, &list)
	for _, in := range list {
		if in.ID == r.id {
			return in.State
		}
	}
	r.t.Fatalf("instance %s is not listed", r.id)
	return esl.State{}
}

// waitState polls until the instance is connected (or not) and returns how
// long that took.
func (r *realStack) waitState(connected bool, within time.Duration) (time.Duration, bool) {
	r.t.Helper()
	start := time.Now()
	for {
		if r.state().Connected == connected {
			return time.Since(start), true
		}
		if time.Since(start) > within {
			return time.Since(start), false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// console runs one command through the console route and returns its output.
func (r *realStack) console(command string) string {
	r.t.Helper()
	code, body := r.c.do("POST", r.p+"/console", map[string]string{"command": command})
	var out struct{ Output string }
	if code != http.StatusOK || json.Unmarshal(body, &out) != nil {
		r.t.Fatalf("console %q: status %d: %s", command, code, clip(body))
	}
	return out.Output
}

// channelSet returns the uuids listed by the channels view.
func (r *realStack) channelSet() map[string]bool {
	r.t.Helper()
	var rows struct{ Rows []map[string]any }
	r.c.get(r.p+"/channels", http.StatusOK, &rows)
	set := map[string]bool{}
	for _, row := range rows.Rows {
		if id, _ := row["uuid"].(string); id != "" {
			set[id] = true
		}
	}
	return set
}

type hangupRecord struct {
	UUID, Cause, OtherUUID string
	LogLines               int
}

func (r *realStack) hangups() []hangupRecord {
	r.t.Helper()
	var h struct{ Rows []hangupRecord }
	r.c.get(r.p+"/hangups?limit=500", http.StatusOK, &h)
	return h.Rows
}

// owned are the channels a test created; they are hung up on the switch
// itself when the test ends, whatever state the console is in.
type owned struct {
	mu  sync.Mutex
	ids []string
}

func (o *owned) add(id string) {
	o.mu.Lock()
	o.ids = append(o.ids, id)
	o.mu.Unlock()
}

func (o *owned) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ids...)
}

// track registers the cleanup that runs uuid_kill for every owned channel
// over a connection of its own.
func (r *realStack) track() *owned {
	o := &owned{}
	t := r.t
	t.Cleanup(func() {
		ids := o.list()
		if len(ids) == 0 {
			return
		}
		cl := esl.NewClient(r.addr, r.password)
		cl.Start()
		defer cl.Close()
		for deadline := time.Now().Add(10 * time.Second); !cl.State().Connected; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Errorf("cleanup: cannot reach %s to hang up %v: %s", r.addr, ids, cl.State().LastError)
				return
			}
		}
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			out, err := cl.API(ctx, "uuid_kill "+id)
			cancel()
			if err != nil {
				t.Errorf("cleanup: uuid_kill %s: %v", id, err)
			} else if !strings.HasPrefix(out, "+OK") && !strings.Contains(out, "No such channel") {
				t.Errorf("cleanup: uuid_kill %s: %q", id, out)
			}
		}
	})
	return o
}

// statusLog records the responses with a 5xx status.
type statusLog struct {
	base http.RoundTripper
	mu   sync.Mutex
	bad  []string
}

func (s *statusLog) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.base.RoundTrip(req)
	if err == nil && resp.StatusCode >= 500 {
		s.mu.Lock()
		s.bad = append(s.bad, fmt.Sprintf("%s %s: %d", req.Method, req.URL.Path, resp.StatusCode))
		s.mu.Unlock()
	}
	return resp, err
}

func (s *statusLog) fives() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bad...)
}

// try is do without failing the test, for other goroutines.
func (c *testClient) try(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

type sseEvent struct{ name, data string }

// openStream opens the event stream at path; it is closed when the test ends.
func (c *testClient) openStream(path string) <-chan sseEvent {
	c.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		cancel()
		c.t.Fatal(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		c.t.Fatalf("GET %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		c.t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, clip(body))
	}
	got := make(chan sseEvent, 4096)
	done := make(chan struct{})
	c.t.Cleanup(func() {
		cancel()
		resp.Body.Close()
		<-done
	})
	go func() {
		defer close(done)
		defer close(got)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				ev.name = name
			} else if data, ok := strings.CutPrefix(line, "data: "); ok {
				ev.data = data
			} else if line == "" && ev.name != "" {
				select {
				case got <- ev:
				case <-ctx.Done():
					return
				}
				ev = sseEvent{}
			}
		}
	}()
	return got
}

// waitEvent returns the first event named name within d, reading past others.
func waitEvent(t *testing.T, events <-chan sseEvent, name string, d time.Duration) (sseEvent, int) {
	t.Helper()
	timeout := time.After(d)
	skipped := 0
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("stream ended while waiting for a %s event", name)
			}
			if ev.name == name {
				return ev, skipped
			}
			skipped++
		case <-timeout:
			t.Fatalf("no %s event within %v (%d other events)", name, d, skipped)
		}
	}
}

func clip(b []byte) string {
	const n = 600
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

var passwordParam = regexp.MustCompile(`(?i)pass(word|wd|phrase)|secret`)

// wellFormed parses doc strictly and returns the name and value attributes
// of its <param> elements.
func wellFormed(t *testing.T, what, doc string) [][2]string {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(doc))
	var params [][2]string
	elems := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("%s is not well-formed XML: %v:\n%s", what, err, clip([]byte(doc)))
		}
		if se, ok := tok.(xml.StartElement); ok {
			elems++
			if se.Name.Local == "param" {
				var name, value string
				for _, a := range se.Attr {
					switch a.Name.Local {
					case "name":
						name = a.Value
					case "value":
						value = a.Value
					}
				}
				params = append(params, [2]string{name, value})
			}
		}
	}
	if elems == 0 {
		t.Fatalf("%s holds no element: %q", what, clip([]byte(doc)))
	}
	return params
}

// TestRealReadViews reads every view the console offers from the real switch.
func TestRealReadViews(t *testing.T) {
	addr, _ := realSwitch(t)
	r := startReal(t, addr)
	c, p := r.c, r.p

	var status struct {
		State    esl.State
		Hostname string
		Status   struct {
			Version string
			Uptime  map[string]float64
		}
	}
	body := c.get(p+"/status", http.StatusOK, &status)
	if !status.State.Connected || status.Status.Version == "" || len(status.Status.Uptime) == 0 {
		t.Fatalf("status lacks the version or the uptime: %s", clip(body))
	}
	t.Logf("switch %s, version %s", status.Hostname, status.Status.Version)

	var sofia struct{ Rows []map[string]string }
	body = c.get(p+"/sofia", http.StatusOK, &sofia)
	profiles := 0
	for _, row := range sofia.Rows {
		if row["type"] == "profile" {
			profiles++
		}
	}
	if profiles == 0 {
		t.Fatalf("no sofia profile: %s", clip(body))
	}

	for _, kind := range []string{"gateways", "registrations", "channels", "calls"} {
		var v struct{ Rows json.RawMessage }
		body := c.get(p+"/"+kind, http.StatusOK, &v)
		if !bytes.HasPrefix(bytes.TrimSpace(v.Rows), []byte("[")) {
			t.Fatalf("%s: no rows array: %s", kind, clip(body))
		}
	}

	var modules struct{ Rows []map[string]any }
	body = c.get(p+"/modules", http.StatusOK, &modules)
	if !anyRowHas(modules.Rows, "mod_sofia") {
		t.Fatalf("modules lack mod_sofia: %s", clip(body))
	}

	var vars struct {
		Rows []struct{ Name, Value string }
	}
	body = c.get(p+"/vars", http.StatusOK, &vars)
	found := false
	for _, v := range vars.Rows {
		found = found || v.Name == "domain"
	}
	if !found {
		t.Fatalf("vars lack domain: %s", clip(body))
	}

	var configs struct{ Rows []struct{ Name string } }
	body = c.get(p+"/config", http.StatusOK, &configs)
	if len(configs.Rows) == 0 {
		t.Fatalf("config list is empty: %s", clip(body))
	}
	var doc struct{ Name, XML string }
	c.get(p+"/config/sofia.conf", http.StatusOK, &doc)
	secret := 0
	for _, pv := range wellFormed(t, "config/sofia.conf", doc.XML) {
		if passwordParam.MatchString(pv[0]) {
			secret++
			if pv[1] != fsapi.Mask {
				t.Errorf("config/sofia.conf: param %q shows the value %q, want %q", pv[0], pv[1], fsapi.Mask)
			}
		}
	}
	if secret > 0 && !strings.Contains(doc.XML, fsapi.Mask) {
		t.Errorf("config/sofia.conf has %d password params but no mask", secret)
	}
	t.Logf("config: %d entries; sofia.conf: %d bytes, %d password-like params", len(configs.Rows), len(doc.XML), secret)

	var contexts struct{ Rows []struct{ Name string } }
	body = c.get(p+"/dialplan", http.StatusOK, &contexts)
	found = false
	for _, ctx := range contexts.Rows {
		found = found || ctx.Name == "default"
	}
	if !found {
		t.Fatalf("dialplan contexts lack default: %s", clip(body))
	}
	c.get(p+"/dialplan/default", http.StatusOK, &doc)
	wellFormed(t, "dialplan/default", doc.XML)

	var domains struct{ Rows []struct{ Name string } }
	body = c.get(p+"/directory", http.StatusOK, &domains)
	if len(domains.Rows) == 0 {
		t.Fatalf("no directory domain: %s", clip(body))
	}
}

func anyRowHas(rows []map[string]any, value string) bool {
	for _, row := range rows {
		for _, v := range row {
			if v == value {
				return true
			}
		}
	}
	return false
}

// TestRealConsole runs a command, a completion and the command list.
func TestRealConsole(t *testing.T) {
	addr, _ := realSwitch(t)
	r := startReal(t, addr)
	if out := r.console("status"); !strings.Contains(out, "UP") {
		t.Fatalf("console status: %q", out)
	}

	code, body := r.c.do("POST", r.p+"/console/complete", map[string]string{"line": "sof"})
	var comp fsapi.Completion
	if code != http.StatusOK || json.Unmarshal(body, &comp) != nil {
		t.Fatalf("complete sof: status %d: %s", code, clip(body))
	}
	found := false
	for _, m := range comp.Matches {
		found = found || m == "sofia"
	}
	if !found {
		t.Fatalf("complete sof lacks sofia: %s", clip(body))
	}

	var commands struct{ Rows []struct{ Name string } }
	body = r.c.get(r.p+"/console/commands", http.StatusOK, &commands)
	found = false
	for _, cmd := range commands.Rows {
		found = found || cmd.Name == "originate"
	}
	if !found {
		t.Fatalf("console commands lack originate (%d commands): %s", len(commands.Rows), clip(body))
	}
}

// originated returns the uuid of an originate reply "+OK <uuid>".
func originated(out string) (string, bool) {
	id, ok := strings.CutPrefix(strings.TrimSpace(out), "+OK ")
	return id, ok && fsapi.ValidUUID(id)
}

// TestRealCallLifecycle places a call, sees it, hangs it up and finds its
// hang-up record with variables and log lines.
func TestRealCallLifecycle(t *testing.T) {
	addr, _ := realSwitch(t)
	r := startReal(t, addr)
	mine := r.track()

	id := newUUID()
	mine.add(id)
	cmd := originateCmd(id)
	if out := r.console(cmd); !strings.Contains(out, "+OK "+id) {
		t.Fatalf("%s answered %q", cmd, out)
	}

	// The channel list is read from the core database, which FreeSWITCH
	// updates a moment after the originate has answered.
	listed := time.Now()
	for deadline := listed.Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		set := r.channelSet()
		if set[id] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("channel %s is not in channels %q 10s after the originate", id, slices.Sorted(maps.Keys(set)))
		}
	}
	t.Logf("channel %s listed %v after the originate", id, time.Since(listed).Round(time.Millisecond))
	code, body := r.c.do("DELETE", r.p+"/channels/"+id, nil)
	if code != http.StatusOK || !bytes.Contains(body, []byte("+OK")) {
		t.Fatalf("hang up %s: status %d: %s", id, code, clip(body))
	}
	for deadline := time.Now().Add(10 * time.Second); r.channelSet()[id]; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("channel %s still listed 10s after the hang-up", id)
		}
	}

	var rec *hangupRecord
	deadline := time.Now().Add(10 * time.Second)
	for rec == nil {
		for _, h := range r.hangups() {
			if h.UUID == id {
				rec = &h
				break
			}
		}
		if rec == nil {
			if time.Now().After(deadline) {
				t.Fatalf("no hang-up record for %s within 10s", id)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if rec.Cause == "" {
		t.Fatalf("hang-up record of %s has no cause: %+v", id, *rec)
	}
	// Late log lines are attached for a few seconds after the hang-up.
	var detail struct {
		Vars map[string]string
		Log  []struct{ Text string }
	}
	for {
		body = r.c.get(r.p+"/hangups/"+id, http.StatusOK, &detail)
		if len(detail.Vars) > 0 && len(detail.Log) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hang-up detail of %s: %d vars, %d log lines: %s", id, len(detail.Vars), len(detail.Log), clip(body))
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("hang-up %s: cause %s, %d vars, %d log lines", id, rec.Cause, len(detail.Vars), len(detail.Log))
}

// TestRealLogStream opens a debug log stream and sees the lines of reloadxml,
// then a heartbeat.
func TestRealLogStream(t *testing.T) {
	addr, _ := realSwitch(t)
	r := startReal(t, addr)
	opened := time.Now()
	events := r.c.openStream(r.p + "/stream?loglevel=7")
	if ev, _ := waitEvent(t, events, "state", 5*time.Second); !strings.Contains(ev.data, `"connected":true`) {
		t.Fatalf("first state event %s", ev.data)
	}
	_, replayed := waitEvent(t, events, "backlog", 5*time.Second)

	code, body := r.c.do("POST", r.p+"/actions", map[string]string{"action": "reloadxml"})
	if code != http.StatusOK || !bytes.Contains(body, []byte("+OK")) {
		t.Fatalf("reloadxml: status %d: %s", code, clip(body))
	}
	ev, _ := waitEvent(t, events, "log", 10*time.Second)
	var line struct {
		Level int
		Text  string
	}
	if err := json.Unmarshal([]byte(ev.data), &line); err != nil {
		t.Fatalf("log event %s: %v", ev.data, err)
	}
	t.Logf("%d lines replayed; first live line: [%d] %s", replayed, line.Level, line.Text)

	if testing.Short() {
		t.Log("heartbeat half skipped under -short")
		return
	}
	waitEvent(t, events, "heartbeat", 30*time.Second-time.Since(opened))
	t.Logf("heartbeat %v after the stream opened", time.Since(opened).Round(time.Millisecond))
}

// forwarder copies TCP both ways between the console and the switch. It can
// close every connection, or turn the current ones into a black hole that
// keeps them open and drops every byte; new connections always forward.
type forwarder struct {
	ln       net.Listener
	target   string
	accepted atomic.Int32
	mu       sync.Mutex
	pairs    map[*fwPair]struct{}
}

type fwPair struct {
	a, b net.Conn
	hole atomic.Bool
	once sync.Once
}

func newForwarder(t *testing.T, target string) *forwarder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &forwarder{ln: ln, target: target, pairs: map[*fwPair]struct{}{}}
	t.Cleanup(func() {
		ln.Close()
		f.closeAll()
	})
	go func() {
		for {
			a, err := ln.Accept()
			if err != nil {
				return
			}
			b, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				a.Close()
				continue
			}
			f.accepted.Add(1)
			p := &fwPair{a: a, b: b}
			f.mu.Lock()
			f.pairs[p] = struct{}{}
			f.mu.Unlock()
			go f.pipe(p, a, b)
			go f.pipe(p, b, a)
		}
	}()
	return f
}

func (f *forwarder) addr() string { return f.ln.Addr().String() }

func (f *forwarder) pipe(p *fwPair, dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !p.hole.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	f.drop(p)
}

func (f *forwarder) drop(p *fwPair) {
	p.once.Do(func() {
		p.a.Close()
		p.b.Close()
		f.mu.Lock()
		delete(f.pairs, p)
		f.mu.Unlock()
	})
}

func (f *forwarder) current() []*fwPair {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ps []*fwPair
	for p := range f.pairs {
		ps = append(ps, p)
	}
	return ps
}

func (f *forwarder) closeAll() int {
	ps := f.current()
	for _, p := range ps {
		f.drop(p)
	}
	return len(ps)
}

func (f *forwarder) blackHole() int {
	ps := f.current()
	for _, p := range ps {
		p.hole.Store(true)
	}
	return len(ps)
}

// TestRealReconnect cuts the connection to the switch, then makes it a black
// hole while a call is in flight; the instance must notice and recover.
func TestRealReconnect(t *testing.T) {
	addr, _ := realSwitch(t)
	fw := newForwarder(t, addr)
	r := startReal(t, fw.addr())
	statusOK := func(what string) {
		t.Helper()
		var st struct {
			State  esl.State
			Status json.RawMessage
		}
		body := r.c.get(r.p+"/status", http.StatusOK, &st)
		if !st.State.Connected || len(st.Status) == 0 || string(st.Status) == "null" {
			t.Fatalf("%s: status %s", what, clip(body))
		}
	}
	statusOK("before")

	// (a) Every forwarded connection closed.
	if n := fw.closeAll(); n == 0 {
		t.Fatal("the forwarder carries no connection")
	}
	down, ok := r.waitState(false, 10*time.Second)
	if !ok {
		t.Fatalf("instance still connected 10s after its connection was closed: %+v", r.state())
	}
	lastErr := r.state().LastError
	up, ok := r.waitState(true, 10*time.Second)
	if !ok {
		t.Fatalf("instance not reconnected within 10s: %+v", r.state())
	}
	statusOK("after the reconnect")
	t.Logf("(a) closed: disconnected after %v (%s), connected again %v later", down.Round(time.Millisecond), lastErr, up.Round(time.Millisecond))

	// (b) The connection stays open but carries nothing while a status call
	// is in flight.
	before := fw.accepted.Load()
	if n := fw.blackHole(); n == 0 {
		t.Fatal("the forwarder carries no connection")
	}
	type answer struct {
		code int
		body []byte
		err  error
	}
	done := make(chan answer, 1)
	start := time.Now()
	go func() {
		code, body, err := r.c.try("GET", r.p+"/status", nil)
		done <- answer{code, body, err}
	}()
	// The status call has a 15s deadline on the ESL side.
	const limit = 15*time.Second + 5*time.Second
	var a answer
	select {
	case a = <-done:
	case <-time.After(limit):
		t.Fatalf("status call through a black hole still pending after %v", limit)
	}
	failed := time.Since(start)
	if a.err != nil {
		t.Fatalf("status call through a black hole: %v", a.err)
	}
	if a.code == http.StatusOK {
		t.Fatalf("status call through a black hole answered 200: %s", clip(a.body))
	}
	up, ok = r.waitState(true, 10*time.Second)
	if !ok {
		t.Fatalf("instance not reconnected within 10s after the black hole: %+v", r.state())
	}
	if fw.accepted.Load() == before {
		t.Fatal("the instance is connected but never opened a fresh connection")
	}
	statusOK("after the black hole")
	t.Logf("(b) black hole: status call failed after %v with %d %s; connected again %v later", failed.Round(time.Millisecond), a.code, strings.TrimSpace(string(a.body)), up.Round(time.Millisecond))
}

// TestRealManyCalls places 30 calls, 10 at once each second, while a debug
// log stream is read, then hangs them all up. FreeSWITCH accepts 30 new
// sessions per second by default, and a loopback call has two.
func TestRealManyCalls(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short")
	}
	const calls, perSecond = 30, 10
	addr, _ := realSwitch(t)
	r := startReal(t, addr)
	mine := r.track()
	connectedSince := r.state().Since

	events := r.c.openStream(r.p + "/stream?loglevel=7")
	waitEvent(t, events, "state", 5*time.Second)
	var logs, drops atomic.Int64
	var streamEnded atomic.Bool
	go func() {
		for ev := range events {
			switch {
			case ev.name == "log":
				logs.Add(1)
			case ev.name == "state" && !strings.Contains(ev.data, `"connected":true`):
				drops.Add(1)
			}
		}
		streamEnded.Store(true)
	}()

	start := time.Now()
	var wg sync.WaitGroup
	replies := make([]string, calls)
	want := make([]string, calls)
	for i := range calls {
		want[i] = newUUID()
		mine.add(want[i])
		time.Sleep(time.Until(start.Add(time.Duration(i/perSecond) * time.Second)))
		wg.Go(func() {
			code, body, err := r.c.try("POST", r.p+"/console", map[string]string{"command": originateCmd(want[i])})
			var out struct{ Output string }
			switch {
			case err != nil:
				replies[i] = "error: " + err.Error()
			case code != http.StatusOK || json.Unmarshal(body, &out) != nil:
				replies[i] = fmt.Sprintf("status %d: %s", code, clip(body))
			default:
				replies[i] = out.Output
			}
		})
	}
	wg.Wait()
	placed := time.Since(start)
	var bad []string
	ids := map[string]bool{}
	for i, rep := range replies {
		if id, ok := originated(rep); ok && id == want[i] {
			ids[id] = true
		} else {
			bad = append(bad, fmt.Sprintf("#%d: %q", i, rep))
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d of %d originates did not answer +OK: %s", len(bad), calls, strings.Join(bad, "; "))
	}
	if len(ids) == 0 {
		t.FailNow()
	}

	var missing []string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		listed := r.channelSet()
		missing = missing[:0]
		for id := range ids {
			if !listed[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d channels not in channels: %v", len(missing), len(ids), missing)
	}

	killStart := time.Now()
	var kills []string
	var killMu sync.Mutex
	for id := range ids {
		wg.Go(func() {
			code, body, err := r.c.try("DELETE", r.p+"/channels/"+id, nil)
			if err != nil || code != http.StatusOK || !bytes.Contains(body, []byte("+OK")) {
				killMu.Lock()
				kills = append(kills, fmt.Sprintf("%s: %d %v %s", id, code, err, clip(body)))
				killMu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(kills) > 0 {
		t.Errorf("%d hang-ups failed: %s", len(kills), strings.Join(kills, "; "))
	}

	// Each loopback call has two legs; the b leg names the a leg as its other.
	var legs int
	var unrecorded []string
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		seen := map[string]bool{}
		legs = 0
		for _, h := range r.hangups() {
			if ids[h.UUID] || ids[h.OtherUUID] {
				legs++
			}
			if ids[h.UUID] {
				seen[h.UUID] = true
			}
		}
		unrecorded = unrecorded[:0]
		for id := range ids {
			if !seen[id] {
				unrecorded = append(unrecorded, id)
			}
		}
		if (len(unrecorded) == 0 && legs >= 2*len(ids)) || time.Now().After(deadline) {
			break
		}
	}
	if len(unrecorded) > 0 {
		t.Errorf("%d of %d calls have no hang-up record 20s after the hang-up (%d leg records): %v", len(unrecorded), len(ids), legs, unrecorded)
	}
	recorded := time.Since(killStart)

	st := r.state()
	if !st.Connected || !st.Since.Equal(connectedSince) || drops.Load() > 0 || streamEnded.Load() {
		t.Errorf("instance did not stay connected: %+v (connected since %v), %d disconnected state events, stream ended %v", st, connectedSince, drops.Load(), streamEnded.Load())
	}
	if f := r.codes.fives(); len(f) > 0 {
		t.Errorf("%d API calls answered 5xx: %v", len(f), f)
	}
	t.Logf("%d calls placed in %v (%d per second); %d channels listed; hung up and %d leg records in %v; wall time %v; %d log events on the stream",
		len(ids), placed.Round(time.Millisecond), perSecond, len(ids)-len(missing), legs, recorded.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), logs.Load())
}

// TestRealSlowCommand runs a console originate that rings for 20s: the
// command runs in the background on the switch, so the status view keeps
// answering at once and the instance stays connected meanwhile.
func TestRealSlowCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short")
	}
	addr, _ := realSwitch(t)
	r := startReal(t, addr)
	mine := r.track()
	connectedSince := r.state().Since

	id := newUUID()
	mine.add(id)
	// 192.0.2.1 is TEST-NET: nobody answers, so the call ends with NO_ANSWER
	// after originate_timeout.
	cmd := "originate {originate_timeout=20,origination_uuid=" + id + "}sofia/internal/1000@192.0.2.1 &echo()"
	type answer struct {
		code int
		body []byte
		err  error
		took time.Duration
	}
	done := make(chan answer, 1)
	start := time.Now()
	go func() {
		code, body, err := r.c.try("POST", r.p+"/console", map[string]string{"command": cmd})
		done <- answer{code, body, err, time.Since(start)}
	}()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	timeout := time.After(70 * time.Second) // the console route gives up after 60s
	var a answer
	var slowest time.Duration
	polls := 0
	for running := true; running; {
		select {
		case a = <-done:
			running = false
		case <-timeout:
			t.Fatalf("console %q still pending after 70s", cmd)
		case <-tick.C:
			polls++
			var st struct {
				State  esl.State
				Status json.RawMessage
			}
			sent := time.Now()
			code, body, err := r.c.try("GET", r.p+"/status", nil)
			took := time.Since(sent)
			slowest = max(slowest, took)
			switch {
			case err != nil:
				t.Errorf("status #%d: %v", polls, err)
			case code != http.StatusOK || json.Unmarshal(body, &st) != nil:
				t.Errorf("status #%d after %v: status %d: %s", polls, time.Since(start).Round(time.Millisecond), code, clip(body))
			case !st.State.Connected || !st.State.Since.Equal(connectedSince) || string(st.Status) == "null":
				t.Errorf("status #%d after %v: instance reported disconnected or reconnected: %s", polls, time.Since(start).Round(time.Millisecond), clip(body))
			case took >= 2*time.Second:
				t.Errorf("status #%d after %v took %v", polls, time.Since(start).Round(time.Millisecond), took)
			}
		}
	}

	var out struct{ Output string }
	if a.err != nil || a.code != http.StatusOK || json.Unmarshal(a.body, &out) != nil || !strings.Contains(out.Output, "NO_ANSWER") {
		t.Fatalf("console %q after %v: status %d, %v: %s", cmd, a.took.Round(time.Millisecond), a.code, a.err, clip(a.body))
	}
	if a.took < 15*time.Second || polls < 10 {
		t.Errorf("the originate took %v with %d status calls meanwhile; want about 20s", a.took.Round(time.Millisecond), polls)
	}
	if st := r.state(); !st.Connected || !st.Since.Equal(connectedSince) {
		t.Errorf("instance did not stay connected: %+v (connected since %v)", st, connectedSince)
	}
	if f := r.codes.fives(); len(f) > 0 {
		t.Errorf("%d API calls answered 5xx: %v", len(f), f)
	}
	t.Logf("console originate answered %d after %v: %q; %d status calls meanwhile, slowest %v",
		a.code, a.took.Round(time.Millisecond), strings.TrimSpace(out.Output), polls, slowest.Round(time.Millisecond))
}
