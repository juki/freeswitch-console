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

	"fsdeck/internal/audit"
	"fsdeck/internal/server"
	"fsdeck/internal/store"
	"fsdeck/web"
)

const eslPassword = "s3cret-esl"

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
	"status":            "UP 0 years, 1 day\n",
}

// fakeFreeSWITCH speaks enough inbound ESL for the whole stack to run:
// auth, api with canned bodies, HEARTBEAT events and log lines.
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
	for {
		var cmd string
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
		case strings.HasPrefix(cmd, "log "):
			logging <- true
			reply("+OK log level " + strings.TrimPrefix(cmd, "log "))
		case cmd == "nolog":
			logging <- false
			reply("+OK no longer logging")
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

// stack is the whole fsdeck wiring against one fake FreeSWITCH.
type stack struct {
	host string
	port int
	dir  string
	ts   *httptest.Server
}

// startStack wires the real ESL client and the real HTTP server to a fake
// FreeSWITCH, as main does; edit adjusts the configuration.
func startStack(t *testing.T, edit func(*server.Config)) stack {
	t.Helper()
	host, port := fakeFreeSWITCH(t)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "fsdeck.json"), store.WithSecretKey("an-e2e-secret-key-value"))
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.Open(filepath.Join(dir, "fsdeck-audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	// The fake logs at level 7 only; keep those lines in the backlog.
	cfg := server.Config{
		AdminPassword:   "admin-pw",
		ViewerPassword:  "viewer-pw",
		Store:           st,
		Dial:            dial,
		Static:          web.Static(),
		Version:         "1.2.3-test",
		LogBacklog:      100,
		LogBacklogLevel: 7,
		Audit:           auditLog,
		BackupDir:       filepath.Join(dir, "backups"),
	}
	if edit != nil {
		edit(&cfg)
	}
	app, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		app.Close()
		ts.Close()
		auditLog.Close()
	})
	return stack{host: host, port: port, dir: dir, ts: ts}
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
	sk := startStack(t, nil)
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
	if data, _ := os.ReadFile(filepath.Join(dir, "fsdeck.json")); bytes.Contains(data, []byte(eslPassword)) || !bytes.Contains(data, []byte(`"password": "enc:v1:`)) {
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
	auditData, _ := os.ReadFile(filepath.Join(dir, "fsdeck-audit.log"))
	for _, want := range []string{
		`"user":"admin","ip":"127.0.0.1","action":"console.run","target":"test","detail":"status","result":"ok"`,
		`"action":"action.fsctl","target":"test","detail":"op=loglevel value=6","result":"ok"`,
	} {
		if !bytes.Contains(auditData, []byte(want)) {
			t.Fatalf("audit log lacks %s:\n%s", want, auditData)
		}
	}
	if body := c.get("/api/license", http.StatusOK, nil); strings.TrimSpace(string(body)) != `{"edition":"community","status":"none","licensee":"","plan":"","features":[],"expires":null}` {
		t.Fatalf("licence: %s", body)
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
