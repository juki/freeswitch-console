package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juki/freeswitch-console/internal/audit"
	"github.com/juki/freeswitch-console/internal/conffs"
	"github.com/juki/freeswitch-console/internal/server"
	"github.com/juki/freeswitch-console/internal/store"
	"github.com/juki/freeswitch-console/web"
)

const (
	eslPassword     = "s3cret-esl"
	e2eMetricsToken = "an-e2e-metrics-token"
)

var cannedAPI = map[string]string{
	`json {"command":"status","data":""}`: `{"command":"status","data":"","status":"success","response":{"systemStatus":"ready","version":"1.10.12 64bit","uptime":{"years":0,"days":1,"hours":2,"minutes":3,"seconds":4,"milliseconds":5,"microseconds":6},"sessions":{"count":{"total":10,"active":1,"peak":2,"peak5Min":1,"limit":1000},"rate":{"current":0,"max":30,"peak":1,"peak5Min":0}},"idleCPU":{"used":0,"allowed":99.5}}}`,
	"hostname":                            "fs-test\n",
	"sofia xmlstatus": "<profiles>\n<profile>\n<name>internal</name>\n<type>profile</type>\n<data>sip:mod_sofia@192.0.2.1:5060</data>\n<state>RUNNING (0)</state>\n</profile>\n" +
		"<gateway>\n<name>gw1</name>\n<type>gateway</type>\n<data>sip:gw1@192.0.2.9</data>\n<state>NOREG</state>\n</gateway>\n</profiles>\n",
	"show channels as json": `{"row_count":1,"rows":[{"uuid":"abc-123","cid_num":"1000","dest":"9196"}]}`,
	"show calls as json":    `{"row_count": 0}` + "\n",
	"global_getvar":         "hostname=fs-test\ndefault_password=1234\n",
	"xml_locate configuration configuration name event_socket.conf": `<configuration name="event_socket.conf" description="Socket Client">
  <settings>
    <param name="listen-port" value="8021"/>
    <param name="password" value="` + eslPassword + `"/>
  </settings>
</configuration>`,
	"xml_locate configuration configuration name missing.conf": "can't find anything\n",
	"uuid_kill abc-123": "+OK\n",
	"fsctl loglevel 6":  "+OK log level: INFO [6]\n",
	// As FreeSWITCH 1.11.3 answers them.
	"console_complete_xml c=7;show chx":         "<?xml version=\"1.0\"?>\n<complete>\n  <match>channels</match>\n  <match>chat</match>\n  <write length=\"2\">cha</write>\n</complete>\n\n",
	"console_complete_xml c=14;sofia profile x": "<?xml version=\"1.0\"?>\n<complete>\n  <match>external</match>\n  <match>internal</match>\n  <match>internal</match>\n  <write length=\"0\"></write>\n</complete>\n",
	"show api as json":                          `{"row_count":2,"rows":[{"name":"status","description":"Show current status","syntax":"","ikey":"mod_commands"},{"name":"sofia","description":"Sofia Controls","syntax":"<cmd> <args>","ikey":"mod_sofia"}]}` + "\n",
	"status":                                    "UP 0 years, 1 day\n",
}

// fakeFreeSWITCH speaks enough inbound ESL for the whole stack to run:
// auth, api with canned bodies, bgapi with the canned body in a
// BACKGROUND_JOB event, HEARTBEAT events and log lines, for a client
// subscribed to CHANNEL_HANGUP_COMPLETE a log line and that event when
// abc-123 is hung up, and for one subscribed to sofia::register_failure one
// such event as the test switch sent it.
func fakeFreeSWITCH(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveESL(conn)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func serveESL(conn net.Conn) {
	defer conn.Close()
	var mu sync.Mutex
	write := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		io.WriteString(conn, s)
	}
	reply := func(text string) { write("Content-Type: command/reply\nReply-Text: " + text + "\n\n") }

	done := make(chan struct{})
	defer close(done)
	logging := make(chan bool, 4)
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		on := false
		for {
			select {
			case <-done:
				return
			case on = <-logging:
			case <-tick.C:
				ev := `{"Event-Name":"HEARTBEAT","Session-Count":"1","Idle-CPU":"99.5"}`
				write(fmt.Sprintf("Content-Length: %d\nContent-Type: text/event-json\n\n%s", len(ev), ev))
				if on {
					line := "sofia.c:123 a debug line\n"
					write(fmt.Sprintf("Content-Type: log/data\nContent-Length: %d\nLog-Level: 7\nText-Channel: 0\nLog-File: sofia.c\nLog-Func: fn\nLog-Line: 123\nUser-Data: \n\n%s", len(line), line))
				}
			}
		}
	}()

	write("Content-Type: auth/request\n\n")
	r := bufio.NewReader(conn)
	hangups, jobs := false, false
	for {
		var cmd, jobUUID string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if cmd == "" {
					continue
				}
				break
			}
			if cmd == "" {
				cmd = line
			} else if v, ok := strings.CutPrefix(line, "Job-UUID: "); ok {
				jobUUID = v
			}
		}
		switch {
		case strings.HasPrefix(cmd, "auth "):
			if cmd == "auth "+eslPassword {
				reply("+OK accepted")
			} else {
				reply("-ERR invalid")
				return
			}
		case strings.HasPrefix(cmd, "api "):
			body, ok := cannedAPI[strings.TrimPrefix(cmd, "api ")]
			if !ok {
				body = "-ERR no such command\n"
			}
			write(fmt.Sprintf("Content-Type: api/response\nContent-Length: %d\n\n%s", len(body), body))
			if cmd == "api uuid_kill abc-123" && hangups {
				line := "Hangup sofia/internal/1000@192.0.2.1 [CS_EXECUTE] [NORMAL_CLEARING]\n"
				write(fmt.Sprintf("Content-Type: log/data\nContent-Length: %d\nLog-Level: 5\nText-Channel: 3\nLog-File: switch_ivr.c\nLog-Func: fn\nLog-Line: 1\nUser-Data: abc-123\n\n%s", len(line), line))
				ev := `{"Event-Name":"CHANNEL_HANGUP_COMPLETE","Unique-ID":"abc-123","Hangup-Cause":"NORMAL_CLEARING","Caller-Destination-Number":"9196","variable_billsec":"7","variable_sip_term_status":"200","variable_sip_auth_password":"` + eslPassword + `"}`
				write(fmt.Sprintf("Content-Length: %d\nContent-Type: text/event-json\n\n%s", len(ev), ev))
			}
		case strings.HasPrefix(cmd, "bgapi "):
			body, ok := cannedAPI[strings.TrimPrefix(cmd, "bgapi ")]
			if !ok {
				body = "-ERR no such command\n"
			}
			write("Content-Type: command/reply\nReply-Text: +OK Job-UUID: " + jobUUID + "\nJob-UUID: " + jobUUID + "\n\n")
			if jobs {
				ev, _ := json.Marshal(map[string]string{"Event-Name": "BACKGROUND_JOB", "Job-UUID": jobUUID, "_body": body})
				write(fmt.Sprintf("Content-Length: %d\nContent-Type: text/event-json\n\n%s", len(ev), ev))
			}
		case strings.HasPrefix(cmd, "log "):
			logging <- true
			reply("+OK log level " + strings.TrimPrefix(cmd, "log "))
		case cmd == "nolog":
			logging <- false
			reply("+OK no longer logging")
		case strings.HasPrefix(cmd, "event json "):
			hangups = strings.Contains(cmd, " CHANNEL_HANGUP_COMPLETE")
			jobs = strings.Contains(cmd, " BACKGROUND_JOB")
			reply("+OK event listener enabled json")
			if _, subclasses, ok := strings.Cut(cmd, " CUSTOM "); ok && strings.Contains(" "+subclasses+" ", " sofia::register_failure ") {
				ev := `{"Event-Name":"CUSTOM","Event-Subclass":"sofia::register_failure","Core-UUID":"6d8a3b0e-0000-4000-8000-000000000000","FreeSWITCH-Hostname":"fs-test","Event-Calling-File":"sofia_reg.c","Event-Sequence":"77",` +
					`"profile-name":"internal","to-user":"1001","to-host":"192.0.2.1","network-ip":"203.0.113.45","network-port":"5070","user-agent":"friendly-scanner","registration-type":"REGISTER"}`
				write(fmt.Sprintf("Content-Length: %d\nContent-Type: text/event-json\n\n%s", len(ev), ev))
			}
		default:
			reply("+OK")
		}
	}
}

type testClient struct {
	t    *testing.T
	base string
	http *http.Client
}

func (c *testClient) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp.StatusCode, out
}

func (c *testClient) get(path string, want int, into any) []byte {
	c.t.Helper()
	code, body := c.do("GET", path, nil)
	if code != want {
		c.t.Fatalf("GET %s: status %d, want %d: %s", path, code, want, body)
	}
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			c.t.Fatalf("GET %s: %v: %s", path, err, body)
		}
	}
	return body
}

// stack is the whole fsconsole wiring against one fake FreeSWITCH.
type stack struct {
	host string
	port int
	dir  string
	ts   *httptest.Server
}

// startStack wires the real ESL client and the real HTTP server to a fake
// FreeSWITCH, as main does; edit adjusts the HTTP server before it starts.
func startStack(t *testing.T, edit ...func(*http.Server)) stack {
	t.Helper()
	host, port := fakeFreeSWITCH(t)
	sk := startConsole(t, nil, edit...)
	sk.host, sk.port = host, port
	return sk
}

// startConsole is startStack without the fake: no instance is known yet.
// configure adjusts the server configuration before the server is built.
func startConsole(t *testing.T, configure func(*server.Config), edit ...func(*http.Server)) stack {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fsconsole.json"), store.WithSecretKey("an-e2e-secret-key-value"))
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.Open(filepath.Join(dir, "fsconsole-audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	// The fake logs at level 7 only; keep those lines in the backlog.
	cfg := server.Config{
		AdminPassword:   "admin-pw",
		ViewerPassword:  "viewer-pw",
		Store:           st,
		Dial:            dialer(100, 100),
		Static:          web.Static(),
		Version:         "1.2.3-test",
		LogBacklog:      100,
		LogBacklogLevel: 7,
		RecentCalls:     100,
		SipEvents:       100,
		MetricsToken:    e2eMetricsToken,
		Audit:           auditLog,
		BackupDir:       filepath.Join(dir, "backups"),
	}
	if configure != nil {
		configure(&cfg)
	}
	app, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = newHTTPServer("", app.Handler(), nil)
	for _, e := range edit {
		e(ts.Config)
	}
	ts.Start()
	t.Cleanup(func() {
		app.Close()
		ts.Close()
		auditLog.Close()
	})
	return stack{dir: dir, ts: ts}
}

// client signs in and returns a client with the session.
func (sk stack) client(t *testing.T, username, password string) *testClient {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &testClient{t: t, base: sk.ts.URL, http: &http.Client{Jar: jar}}
	if code, body := c.do("POST", "/api/login", map[string]string{"username": username, "password": password}); code != http.StatusOK {
		t.Fatalf("login %s: status %d: %s", username, code, body)
	}
	return c
}

// TestEndToEnd runs the real ESL client and the real HTTP server against a
// fake FreeSWITCH, through the same wiring main uses.
func TestEndToEnd(t *testing.T) {
	sk := startStack(t)
	host, port, dir, ts := sk.host, sk.port, sk.dir, sk.ts

	jar, _ := cookiejar.New(nil)
	c := &testClient{t: t, base: ts.URL, http: &http.Client{Jar: jar}}

	if body := c.get("/healthz", http.StatusOK, nil); strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Fatalf("healthz: %s", body)
	}
	c.get("/api/version", http.StatusUnauthorized, nil)
	if code, body := c.do("GET", "/api/instances", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: status %d: %s", code, body)
	}
	if code, body := c.do("POST", "/api/login", map[string]string{"username": "admin", "password": "admin-pw"}); code != http.StatusOK {
		t.Fatalf("login: status %d: %s", code, body)
	}

	if body := c.get("/api/version", http.StatusOK, nil); strings.TrimSpace(string(body)) != `{"version":"1.2.3-test"}` {
		t.Fatalf("version: %s", body)
	}

	code, body := c.do("POST", "/api/instances", map[string]any{"name": "test", "host": host, "port": port, "password": eslPassword})
	if code != http.StatusCreated {
		t.Fatalf("create instance: status %d: %s", code, body)
	}
	if bytes.Contains(body, []byte(eslPassword)) {
		t.Fatalf("create response leaks the ESL password: %s", body)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "fsconsole.json")); bytes.Contains(data, []byte(eslPassword)) || !bytes.Contains(data, []byte(`"password": "enc:v1:`)) {
		t.Fatalf("ESL password not encrypted at rest: %s", data)
	}
	var inst struct{ ID string }
	if err := json.Unmarshal(body, &inst); err != nil || inst.ID == "" {
		t.Fatalf("create instance: no id in %s", body)
	}
	p := "/api/instances/" + inst.ID

	var status struct {
		State    struct{ Connected bool }
		Hostname string
		Status   struct{ SystemStatus string }
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.get(p+"/status", http.StatusOK, &status)
		if status.State.Connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("instance never connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status.Hostname != "fs-test" || status.Status.SystemStatus != "ready" {
		t.Fatalf("status = %+v", status)
	}

	var rows struct{ Rows []map[string]any }
	c.get(p+"/sofia", http.StatusOK, &rows)
	if len(rows.Rows) != 2 || rows.Rows[0]["name"] != "internal" || rows.Rows[1]["type"] != "gateway" {
		t.Fatalf("sofia rows = %v", rows.Rows)
	}
	c.get(p+"/channels", http.StatusOK, &rows)
	if len(rows.Rows) != 1 || rows.Rows[0]["uuid"] != "abc-123" {
		t.Fatalf("channel rows = %v", rows.Rows)
	}
	if body := c.get(p+"/calls", http.StatusOK, nil); !bytes.Contains(body, []byte(`"rows":[]`)) {
		t.Fatalf("empty calls must be an empty array: %s", body)
	}

	if body := c.get(p+"/vars", http.StatusOK, nil); bytes.Contains(body, []byte("1234")) {
		t.Fatalf("vars not masked: %s", body)
	}
	body = c.get(p+"/config/event_socket.conf", http.StatusOK, nil)
	if bytes.Contains(body, []byte(eslPassword)) || !bytes.Contains(body, []byte("8021")) {
		t.Fatalf("config not masked or incomplete: %s", body)
	}
	c.get(p+"/config/missing.conf", http.StatusNotFound, nil)

	var out struct{ Output string }
	code, body = c.do("POST", p+"/console", map[string]string{"command": "status"})
	if code != http.StatusOK || json.Unmarshal(body, &out) != nil || !strings.HasPrefix(out.Output, "UP ") {
		t.Fatalf("console: status %d: %s", code, body)
	}
	if code, body = c.do("POST", p+"/console", map[string]string{"command": "status\n\napi shutdown"}); code == http.StatusOK {
		t.Fatalf("console accepted a multi-line command: %s", body)
	}
	if code, body = c.do("DELETE", p+"/channels/abc-123", nil); code != http.StatusOK {
		t.Fatalf("hangup: status %d: %s", code, body)
	}
	// The hang-up is recorded with its log line and masked variables.
	var hangups struct {
		Enabled  bool
		Capacity int
		Rows     []struct {
			UUID, Cause string
			SIPStatus   int
			Answered    bool
			LogLines    int
		}
	}
	for deadline := time.Now().Add(5 * time.Second); len(hangups.Rows) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("hang-up never recorded")
		}
		c.get(p+"/hangups", http.StatusOK, &hangups)
	}
	if r := hangups.Rows[0]; !hangups.Enabled || hangups.Capacity != 100 || r.UUID != "abc-123" || r.Cause != "NORMAL_CLEARING" || r.SIPStatus != 200 || !r.Answered || r.LogLines != 1 {
		t.Fatalf("hangups %+v", hangups)
	}
	body = c.get(p+"/hangups/abc-123", http.StatusOK, nil)
	if !bytes.Contains(body, []byte(`"uuid":"abc-123"`)) || !bytes.Contains(body, []byte(`"sip_auth_password":"********"`)) || !bytes.Contains(body, []byte(`"text":"Hangup sofia/internal`)) {
		t.Fatalf("hang-up detail %s", body)
	}

	// The failed registration the fake sends on subscribing is recorded.
	var sipEvents struct {
		Enabled  bool
		Capacity int
		Rows     []struct {
			Seq                                int
			Kind, Profile, User, IP, UserAgent string
			Port                               int
		}
	}
	c.get(p+"/sipevents?kind=register_failure", http.StatusOK, &sipEvents)
	if r := sipEvents.Rows; !sipEvents.Enabled || sipEvents.Capacity != 100 || len(r) == 0 || r[0].Kind != "register_failure" || r[0].User != "1001" || r[0].IP != "203.0.113.45" || r[0].Port != 5070 || r[0].UserAgent != "friendly-scanner" {
		t.Fatalf("SIP events %+v", sipEvents)
	}
	body = c.get(fmt.Sprintf("%s/sipevents/%d", p, sipEvents.Rows[0].Seq), http.StatusOK, nil)
	if !bytes.Contains(body, []byte(`"registration-type":"REGISTER"`)) || bytes.Contains(body, []byte("Core-UUID")) {
		t.Fatalf("SIP event detail %s", body)
	}

	// The metrics, with the token and no session, once a heartbeat (every
	// 50ms) has come.
	var metrics []byte
	for deadline := time.Now().Add(5 * time.Second); !bytes.Contains(metrics, []byte(`fsconsole_sessions{`)); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no heartbeat in the metrics:\n%s", metrics)
		}
		scrape, _ := http.NewRequest("GET", ts.URL+"/metrics", nil)
		scrape.Header.Set("Authorization", "Bearer "+e2eMetricsToken)
		scraped, err := http.DefaultClient.Do(scrape)
		if err != nil {
			t.Fatal(err)
		}
		metrics, _ = io.ReadAll(scraped.Body)
		scraped.Body.Close()
		if scraped.StatusCode != http.StatusOK {
			t.Fatalf("metrics: %d %s", scraped.StatusCode, metrics)
		}
	}
	for _, line := range []string{`fsconsole_build_info{version="1.2.3-test"} 1`, `fsconsole_instance_up{instance="test"} 1`, `fsconsole_sessions{instance="test"} 1`,
		`fsconsole_hangups_total{instance="test",cause="NORMAL_CLEARING"} 1`, `fsconsole_sip_events_total{instance="test",kind="register_failure"} 1`} {
		if !bytes.Contains(metrics, []byte(line+"\n")) {
			t.Fatalf("metrics: no %s in\n%s", line, metrics)
		}
	}

	// The backlog level is applied when the instance is created, so the
	// fake has been logging since it connected: the stream replays those
	// lines, marks the hand-over, then goes live.
	time.Sleep(200 * time.Millisecond)
	resp, err := c.http.Get(ts.URL + p + "/stream?loglevel=7")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream content type = %q", ct)
	}
	type event struct{ name, data string }
	got := make(chan event)
	go func() {
		defer close(got)
		sc := bufio.NewScanner(resp.Body)
		var ev event
		for sc.Scan() {
			line := sc.Text()
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				ev.name = name
			} else if data, ok := strings.CutPrefix(line, "data: "); ok {
				ev.data = data
			} else if line == "" && ev.name != "" {
				got <- ev
				ev = event{}
			}
		}
	}()
	nextEvent := func() event {
		t.Helper()
		select {
		case ev, ok := <-got:
			if !ok {
				t.Fatal("stream ended early")
			}
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("stream timeout")
		}
		return event{}
	}
	var logLine struct {
		Seq  uint64
		Text string
	}
	if ev := nextEvent(); ev.name != "state" {
		t.Fatalf("first event %+v", ev)
	}
	var replayed int
	var lastSeq uint64
	for {
		ev := nextEvent()
		if ev.name == "backlog" {
			if ev.data != fmt.Sprintf(`{"lines":%d}`, replayed) || replayed == 0 {
				t.Fatalf("backlog event %s after %d replayed lines", ev.data, replayed)
			}
			break
		}
		if ev.name != "log" || json.Unmarshal([]byte(ev.data), &logLine) != nil || logLine.Seq != lastSeq+1 && lastSeq != 0 || logLine.Seq == 0 {
			t.Fatalf("replayed %+v after seq %d", ev, lastSeq)
		}
		lastSeq = logLine.Seq
		replayed++
	}
	for seenLive, seenHeartbeat := false, false; !seenLive || !seenHeartbeat; {
		switch ev := nextEvent(); ev.name {
		case "heartbeat":
			seenHeartbeat = true
		case "log":
			json.Unmarshal([]byte(ev.data), &logLine)
			if logLine.Seq != lastSeq+1 || !strings.Contains(logLine.Text, "debug line") {
				t.Fatalf("live line %s after seq %d", ev.data, lastSeq)
			}
			lastSeq = logLine.Seq
			seenLive = true
		}
	}
	resp.Body.Close()

	// A runtime operation, then the audit file has the console command and
	// the operation.
	if code, body := c.do("POST", p+"/actions", map[string]any{"action": "fsctl", "op": "loglevel", "value": 6}); code != http.StatusOK || !strings.Contains(string(body), "log level") {
		t.Fatalf("fsctl: status %d: %s", code, body)
	}
	auditData, _ := os.ReadFile(filepath.Join(dir, "fsconsole-audit.log"))
	for _, want := range []string{
		`"user":"admin","ip":"127.0.0.1","action":"console.run","target":"test","detail":"status","result":"ok"`,
		`"action":"action.fsctl","target":"test","detail":"op=loglevel value=6","result":"ok"`,
	} {
		if !bytes.Contains(auditData, []byte(want)) {
			t.Fatalf("audit log lacks %s:\n%s", want, auditData)
		}
	}
	// Console completion and the command list.
	for line, want := range map[string]string{
		"show ch":        `{"matches":["channels","chat"],"replace":2,"text":"cha"}`,
		"sofia profile ": `{"matches":["external","internal"],"replace":0,"text":""}`,
	} {
		code, body := c.do("POST", p+"/console/complete", map[string]string{"line": line})
		if code != http.StatusOK || strings.TrimSpace(string(body)) != want {
			t.Fatalf("complete %q: %d %s", line, code, body)
		}
	}
	var commands struct {
		Rows []struct{ Name, Description, Syntax string }
	}
	c.get(p+"/console/commands", http.StatusOK, &commands)
	if len(commands.Rows) != 2 || commands.Rows[0].Name != "sofia" || commands.Rows[0].Syntax != "<cmd> <args>" {
		t.Fatalf("commands %+v", commands)
	}

	// The viewer account reads but cannot act.
	vjar, _ := cookiejar.New(nil)
	v := &testClient{t: t, base: ts.URL, http: &http.Client{Jar: vjar}}
	if code, body := v.do("POST", "/api/login", map[string]string{"username": "viewer", "password": "viewer-pw"}); code != http.StatusOK {
		t.Fatalf("viewer login: status %d: %s", code, body)
	}
	v.get(p+"/channels", http.StatusOK, nil)
	if code, _ := v.do("POST", p+"/console", map[string]string{"command": "status"}); code != http.StatusForbidden {
		t.Fatalf("viewer console: status %d, want 403", code)
	}

	if code, body := c.do("DELETE", p, nil); code != http.StatusNoContent {
		t.Fatalf("delete instance: status %d: %s", code, body)
	}
	c.get(p+"/status", http.StatusNotFound, nil)

	// The frontend is embedded and served from the root.
	if code, body := c.do("GET", "/", nil); code != http.StatusOK || !bytes.Contains(body, []byte("<html")) {
		t.Fatalf("GET /: status %d", code)
	}
}

func confTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "freeswitch")
	os.MkdirAll(filepath.Join(root, "sip_profiles"), 0o755)
	os.WriteFile(filepath.Join(root, "vars.xml"), []byte("<include>\n  <X-PRE-PROCESS cmd=\"set\" data=\"domain=$${local_ip_v4}\"/>\n</include>\n"), 0o644)
	return root
}

// TestEndToEndConfigFiles covers through the real wiring config editing in
// local and agent mode, search, the audit query and the operator role.
func TestEndToEndConfigFiles(t *testing.T) {
	sk := startStack(t)
	c := sk.client(t, "admin", "admin-pw")

	root := confTree(t)
	code, body := c.do("POST", "/api/instances", map[string]any{"name": "test", "host": sk.host, "port": sk.port, "password": eslPassword, "configMode": "local", "configPath": root})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var inst struct{ ID, ConfigMode, ConfigPath string }
	json.Unmarshal(body, &inst)
	if inst.ConfigMode != "local" || inst.ConfigPath != root {
		t.Fatalf("instance %s", body)
	}
	p := "/api/instances/" + inst.ID

	// Local mode: read, write, 409 on a stale hash, 422 on broken XML,
	// history, restore.
	var f conffs.File
	c.get(p+"/files/content?path=vars.xml", http.StatusOK, &f)
	write := func(content, base string) (int, []byte) {
		return c.do("PUT", p+"/files/content", map[string]string{"path": "vars.xml", "content": content, "baseSha256": base})
	}
	edited := strings.Replace(f.Content, "local_ip_v4", "local_ip_v6", 1)
	code, body = write(edited, f.SHA256)
	var w conffs.Written
	if code != http.StatusOK || json.Unmarshal(body, &w) != nil || w.Size != int64(len(edited)) {
		t.Fatalf("write: %d %s", code, body)
	}
	if code, body = write("<include/>", f.SHA256); code != http.StatusConflict || !bytes.Contains(body, []byte(`"currentSha256":"`+w.SHA256+`"`)) {
		t.Fatalf("stale write: %d %s", code, body)
	}
	if code, body = write("<include>\n<oops>\n</include>\n", w.SHA256); code != http.StatusUnprocessableEntity || !bytes.Contains(body, []byte(`"line":3`)) {
		t.Fatalf("broken XML: %d %s", code, body)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "vars.xml")); string(data) != edited {
		t.Fatalf("file on disk: %s", data)
	}
	var hist struct{ Rows []conffs.Backup }
	c.get(p+"/files/history?path=vars.xml", http.StatusOK, &hist)
	if len(hist.Rows) != 1 || hist.Rows[0].SHA256 != f.SHA256 || hist.Rows[0].User != "admin" {
		t.Fatalf("history %+v", hist)
	}
	code, body = c.do("POST", p+"/files/restore", map[string]string{"path": "vars.xml", "id": hist.Rows[0].ID, "baseSha256": w.SHA256})
	if code != http.StatusOK {
		t.Fatalf("restore: %d %s", code, body)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "vars.xml")); string(data) != f.Content {
		t.Fatalf("restored file: %s", data)
	}
	var found conffs.SearchResult
	c.get(p+"/files/search?q=LOCAL_IP", http.StatusOK, &found)
	if len(found.Rows) != 1 || found.Rows[0].Path != "vars.xml" || found.Rows[0].Line != 2 || found.Truncated || !strings.HasPrefix(found.Rows[0].Text, "<X-PRE-PROCESS") {
		t.Fatalf("search %+v", found)
	}
	c.get(p+"/files/search?q=x", http.StatusBadRequest, nil)
	if _, err := os.Stat(filepath.Join(sk.dir, "backups", inst.ID)); err != nil {
		t.Fatalf("backups not beside the data file: %v", err)
	}

	// The audit query: the config changes are in it.
	var audit struct {
		Rows []struct{ Action, Target, Result string }
	}
	c.get("/api/audit?q=config.&limit=10", http.StatusOK, &audit)
	if len(audit.Rows) != 4 || audit.Rows[0].Action != "config.restore" || audit.Rows[3].Action != "config.write" || audit.Rows[3].Target != "test" {
		t.Fatalf("audit %+v", audit.Rows)
	}

	// Agent mode: the agent handler on a loopback listener, its own backups.
	agentRoot := confTree(t)
	store, err := conffs.NewLocal(agentRoot, filepath.Join(t.TempDir(), "agent-backups"))
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(conffs.NewAgentHandler(store, "e2e-agent-token-1234"))
	defer agent.Close()
	code, body = c.do("PUT", p, map[string]any{"name": "test", "host": sk.host, "port": sk.port, "password": "", "configMode": "agent", "agentURL": agent.URL, "agentToken": "e2e-agent-token-1234"})
	if code != http.StatusOK || bytes.Contains(body, []byte("e2e-agent-token")) {
		t.Fatalf("switch to agent: %d %s", code, body)
	}
	c.get(p+"/files/content?path=vars.xml", http.StatusOK, &f)
	if code, body = write("<include/>\n", f.SHA256); code != http.StatusOK {
		t.Fatalf("write through the agent: %d %s", code, body)
	}
	if data, _ := os.ReadFile(filepath.Join(agentRoot, "vars.xml")); string(data) != "<include/>\n" {
		t.Fatalf("agent file: %s", data)
	}
	c.get(p+"/files/history?path=vars.xml", http.StatusOK, &hist)
	if len(hist.Rows) != 1 {
		t.Fatalf("agent history %+v", hist)
	}
	c.get(p+"/files/search?q=include&case=1", http.StatusOK, &found)
	if len(found.Rows) != 1 || found.Rows[0].Text != "<include/>" {
		t.Fatalf("search through the agent %+v", found)
	}

	// An operator: hang up and rescan yes; console, admin operations, user
	// management and config files no.
	code, body = c.do("POST", "/api/users", map[string]string{"username": "op1", "role": "operator", "password": "operator-password"})
	if code != http.StatusCreated || bytes.Contains(body, []byte("operator-password")) {
		t.Fatalf("create operator: %d %s", code, body)
	}
	o := sk.client(t, "op1", "operator-password")
	if code, body := o.do("DELETE", p+"/channels/abc-123", nil); code != http.StatusOK {
		t.Fatalf("operator hangup: %d %s", code, body)
	}
	if code, _ := o.do("POST", p+"/actions", map[string]string{"action": "reloadxml"}); code == http.StatusForbidden {
		t.Fatal("operator reloadxml refused")
	}
	for _, rq := range []struct {
		method, path string
		body         any
	}{
		{"POST", p + "/console", map[string]string{"command": "status"}},
		{"POST", p + "/actions", map[string]any{"action": "fsctl", "op": "loglevel", "value": 6}},
		{"GET", "/api/users", nil},
		{"POST", "/api/users", map[string]string{"username": "op2", "role": "admin", "password": "operator-password"}},
		{"GET", "/api/audit", nil},
		{"GET", p + "/files?path=", nil},
		{"POST", "/api/instances", map[string]any{"name": "x", "host": "h", "port": 1, "password": "p"}},
	} {
		if code, body := o.do(rq.method, rq.path, rq.body); code != http.StatusForbidden {
			t.Fatalf("operator %s %s: %d %s", rq.method, rq.path, code, body)
		}
	}
}

// TestReadTimeout runs the HTTP server as main builds it with a short
// ReadTimeout: a request body that never arrives is cut off, while an open
// event stream outlives the timeout.
func TestReadTimeout(t *testing.T) {
	if s := newHTTPServer("x", nil, nil); s.ReadTimeout != 30*time.Second || s.IdleTimeout != 120*time.Second || s.ReadHeaderTimeout != 10*time.Second || s.WriteTimeout != 0 {
		t.Fatalf("timeouts %v %v %v %v", s.ReadTimeout, s.IdleTimeout, s.ReadHeaderTimeout, s.WriteTimeout)
	}
	const timeout = 300 * time.Millisecond
	sk := startStack(t, func(s *http.Server) { s.ReadTimeout = timeout })
	c := sk.client(t, "admin", "admin-pw")
	code, body := c.do("POST", "/api/instances", map[string]any{"name": "test", "host": sk.host, "port": sk.port, "password": eslPassword})
	var inst struct{ ID string }
	if err := json.Unmarshal(body, &inst); code != http.StatusCreated || err != nil {
		t.Fatalf("create instance: %d %s", code, body)
	}

	resp, err := c.http.Get(sk.ts.URL + "/api/instances/" + inst.ID + "/stream")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %v %v", resp, err)
	}
	opened := time.Now()
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	// The fake sends a heartbeat every 50ms once connected.
	for late := false; !late; {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended after %v", time.Since(opened))
			}
			late = line == "event: heartbeat" && time.Since(opened) > 5*timeout
		case <-time.After(2 * time.Second):
			t.Fatal("no heartbeat")
		}
	}
	resp.Body.Close()
	for range lines {
	}

	conn, err := net.Dial("tcp", sk.ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /api/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, err = io.ReadAll(conn)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("a request whose body never arrives was not cut off after %v", time.Since(start))
	}
}
