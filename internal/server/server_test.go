package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"fsdeck/internal/esl"
	"fsdeck/internal/store"
)

const (
	adminPW  = "admin-pw"
	viewerPW = "viewer-pw"
	eslPW    = "esl-S3cret-pw"
)

// fakeSwitch is a connected FreeSWITCH answering from a reply table.
type fakeSwitch struct {
	addr, password string

	mu        sync.Mutex
	state     esl.State
	replies   map[string]string
	apiErr    error
	sent      []string
	logLevels []int
	subs      map[int]chan esl.Message
	nextSub   int
	closed    bool
}

const statusJSON = `{"command":"status","data":"","status":"success","response":{"systemStatus":"ready","uptime":{"days":1},"version":"1.10.12","sessions":{"count":{"total":5,"active":1,"peak":2,"peak5Min":1,"limit":1000},"rate":{"current":0,"max":30,"peak":1,"peak5Min":0}},"idleCPU":{"used":0,"allowed":99.1},"stackSizeKB":{"current":240,"max":8192}}}`

func newFake(addr, password string) *fakeSwitch {
	return &fakeSwitch{
		addr:     addr,
		password: password,
		state:    esl.State{Connected: true, Since: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		subs:     map[int]chan esl.Message{},
		replies: map[string]string{
			"hostname":                            "fs01\n",
			`json {"command":"status","data":""}`: statusJSON + "\n",
			"sofia xmlstatus": `<?xml version="1.0" encoding="ISO-8859-1"?>
<profiles>
<profile>
<name>internal</name>
<type>profile</type>
<data>sip:mod_sofia@10.0.0.5:5060</data>
<state>RUNNING (0)</state>
</profile>
</profiles>
`,
			"sofia xmlstatus gateway":    "<gateways>\n  <gateway>\n    <name>gw1</name>\n    <state>REGED</state>\n  </gateway>\n</gateways>\n",
			"show registrations as json": `{"row_count":0}`,
			"show channels as json":      `{"row_count":1,"rows":[{"uuid":"abc-123","direction":"inbound"}]}`,
			"show calls as json":         `{"row_count":1,"rows":[{"uuid":"abc-123","b_uuid":"def-456"}]}`,
			"show modules as json":       `{"row_count":1,"rows":[{"type":"api","name":"status","ikey":"mod_commands"}]}`,
			"global_getvar":              "hostname=fs01\ndefault_password=1234\n",
			"xml_locate configuration":   `<section name="configuration"><configuration name="sofia.conf" description="sofia Endpoint"/></section>`,
			"xml_locate configuration configuration name sofia.conf": `<configuration name="sofia.conf"><param name="password" value="gwsecret"/></configuration>`,
			"xml_locate configuration configuration name nope.conf":  "can't find anything\n",
			"uuid_kill abc-123":             "+OK\n",
			"reloadxml":                     "+OK [Success]\n",
			"reloadacl":                     "+OK acl reloaded\n",
			"sofia profile internal rescan": "+OK scan complete\n",
			"show calls count":              "\n0 total.\n\n",
			"bogus":                         "-ERR bogus Command not found!\n",

			"xml_locate dialplan":                       `<section name="dialplan"><context name="public"><extension name="p"/></context><context name="default"><extension name="a"/><extension name="b"/></context></section>`,
			"xml_locate dialplan context name default":  `<context name="default"><extension name="a"><condition field="destination_number" expression="^1000$"><action application="bridge" data="user/1000"/></condition></extension></context>`,
			"xml_locate dialplan context name nope":     "can't find anything\n",
			"xml_locate directory":                      `<section name="directory"><domain name="10.0.0.1"><groups><group name="default"><users><user id="1000"/><user id="1001"/></users></group><group name="sales"><users><user id="1000" type="pointer"/></users></group></groups></domain></section>`,
			"xml_locate directory domain name 10.0.0.1": `<domain name="10.0.0.1"><groups><group name="default"><users><user id="1000"><params><param name="password" value="sip-S3cret"/><param name="vm-password" value="vm-S3cret"/><param name="a1-hash" value="a1-S3cret"/></params><variables><variable name="user_context" value="default"/></variables></user></users></group></groups></domain>`,
			"xml_locate directory domain name nope":     "can't find anything\n",
		},
	}
}

func (f *fakeSwitch) State() esl.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeSwitch) API(_ context.Context, cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, cmd)
	switch {
	case !f.state.Connected:
		return "", esl.ErrNotConnected
	case f.apiErr != nil:
		return "", f.apiErr
	}
	if r, ok := f.replies[cmd]; ok {
		return r, nil
	}
	return "+OK\n", nil
}

func (f *fakeSwitch) SetLogLevel(level int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logLevels = append(f.logLevels, level)
}

func (f *fakeSwitch) Subscribe(buffer int) (<-chan esl.Message, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan esl.Message, buffer)
	if f.closed {
		close(ch)
		return ch, func() {}
	}
	id := f.nextSub
	f.nextSub++
	f.subs[id] = ch
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if c, ok := f.subs[id]; ok {
			close(c)
			delete(f.subs, id)
		}
	}
}

func (f *fakeSwitch) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.state.Connected = false
	for id, c := range f.subs {
		close(c)
		delete(f.subs, id)
	}
}

func (f *fakeSwitch) publish(m esl.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.subs {
		select {
		case c <- m:
		default:
		}
	}
}

func (f *fakeSwitch) set(fn func(f *fakeSwitch)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSwitch) snapshot() (sent []string, levels []int, subs int, closed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...), append([]int(nil), f.logLevels...), len(f.subs), f.closed
}

type harness struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	st  *store.Store
	id  string // preloaded instance

	mu     sync.Mutex
	dialed []*fakeSwitch
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "instances.json"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := st.Add(store.Instance{Name: "fs01", Host: "10.0.0.5", Port: 8021, Password: eslPW})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, st: st, id: in.ID}
	h.srv, err = New(Config{
		AdminPassword:  adminPW,
		ViewerPassword: viewerPW,
		Store:          st,
		Dial:           h.dial,
		Static:         fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>fsdeck</title>")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.loginDelay = 0
	h.ts = httptest.NewServer(h.srv.Handler())
	t.Cleanup(func() {
		h.srv.Close()
		h.ts.Close()
	})
	return h
}

func (h *harness) dial(addr, password string) Switch {
	f := newFake(addr, password)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dialed = append(h.dialed, f)
	return f
}

func (h *harness) fake(i int) *fakeSwitch {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dialed[i]
}

type response struct {
	code   int
	body   string
	header http.Header
	resp   *http.Response
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), v); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
}

// do sends body (a JSON string) with an application/json content type when
// body is non-empty.
func (h *harness) do(method, path, token, body string) response {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), eslPW) || strings.Contains(string(b), adminPW) {
		h.t.Fatalf("%s %s: response leaks a password: %s", method, path, b)
	}
	return response{code: resp.StatusCode, body: string(b), header: resp.Header, resp: resp}
}

func (h *harness) login(username, password string) string {
	h.t.Helper()
	r := h.do("POST", "/api/login", "", `{"username":"`+username+`","password":"`+password+`"}`)
	if r.code != 200 {
		h.t.Fatalf("login %s: %d %s", username, r.code, r.body)
	}
	for _, c := range r.resp.Cookies() {
		if c.Name == cookieName {
			return c.Value
		}
	}
	h.t.Fatal("no session cookie")
	return ""
}

func expect(t *testing.T, r response, code int, want string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("status %d, want %d (body %s)", r.code, code, r.body)
	}
	if want != "" && strings.TrimSpace(r.body) != want {
		t.Fatalf("body\n got %s\nwant %s", strings.TrimSpace(r.body), want)
	}
}

func TestLoginLogoutMe(t *testing.T) {
	h := newHarness(t)
	expect(t, h.do("GET", "/api/me", "", ""), 401, `{"error":"not logged in"}`)
	expect(t, h.do("POST", "/api/login", "", `{"username":"admin","password":"wrong"}`), 401, `{"error":"invalid username or password"}`)
	expect(t, h.do("POST", "/api/login", "", `{"username":"root","password":"`+adminPW+`"}`), 401, "")
	expect(t, h.do("POST", "/api/login", "", `{"username":"viewer","password":"`+adminPW+`"}`), 401, "")
	expect(t, h.do("POST", "/api/login", "", `{"username":"admin"`), 400, "")
	expect(t, h.do("POST", "/api/login", "", `{"username":"admin","password":"x","extra":1}`), 400, "")
	expect(t, h.do("POST", "/api/login", "", `{"username":"admin","password":"x"} {}`), 400, "")

	r := h.do("POST", "/api/login", "", `{"username":"admin","password":"`+adminPW+`"}`)
	expect(t, r, 200, `{"username":"admin","role":"admin"}`)
	cookies := r.resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies %v", cookies)
	}
	c := cookies[0]
	if c.Name != "fsdeck_session" || len(c.Value) != 64 || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure || c.MaxAge != 12*3600 {
		t.Fatalf("cookie %+v", c)
	}
	expect(t, h.do("GET", "/api/me", c.Value, ""), 200, `{"username":"admin","role":"admin"}`)

	r = h.do("POST", "/api/logout", c.Value, "")
	expect(t, r, 204, "")
	if cs := r.resp.Cookies(); len(cs) != 1 || cs[0].MaxAge >= 0 || cs[0].Value != "" {
		t.Fatalf("logout cookie %+v", cs)
	}
	expect(t, h.do("GET", "/api/me", c.Value, ""), 401, "")

	v := h.login("viewer", viewerPW)
	expect(t, h.do("GET", "/api/me", v, ""), 200, `{"username":"viewer","role":"viewer"}`)
}

func TestESLContextIgnoresRequestCancel(t *testing.T) {
	reqCtx, abort := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/", nil).WithContext(reqCtx)
	ctx, cancel := eslContext(r, time.Minute)
	defer cancel()
	abort()
	if err := ctx.Err(); err != nil {
		t.Fatalf("ESL context follows the aborted request: %v", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("ESL context has no deadline")
	}
}

func TestSessionCookieSecure(t *testing.T) {
	plain := httptest.NewRequest("GET", "/", nil)
	if sessionCookie(plain, "v", 1).Secure {
		t.Fatal("cookie is Secure on a plain HTTP request")
	}
	proxied := httptest.NewRequest("GET", "/", nil)
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if !sessionCookie(proxied, "v", 1).Secure {
		t.Fatal("cookie is not Secure behind a TLS-terminating proxy")
	}
}

func TestLoginWrongContentType(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("POST", h.ts.URL+"/api/login", strings.NewReader(`{"username":"admin","password":"`+adminPW+`"}`))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 415 || len(resp.Cookies()) != 0 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestLoginDelay(t *testing.T) {
	h := newHarness(t)
	h.srv.loginDelay = 100 * time.Millisecond
	start := time.Now()
	expect(t, h.do("POST", "/api/login", "", `{"username":"admin","password":"nope"}`), 401, "")
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("failed login answered after %v", d)
	}
}

func TestViewerDisabled(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "i.json"))
	srv, err := New(Config{AdminPassword: "a", Store: st, Dial: func(string, string) Switch { return nil }, Static: fstest.MapFS{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.checkLogin("viewer", ""); ok {
		t.Fatal("viewer with empty password accepted")
	}
	if _, err := New(Config{Store: st, Dial: srv.cfg.Dial, Static: fstest.MapFS{}}); err == nil {
		t.Fatal("missing admin password accepted")
	}
}

func TestSessionExpiry(t *testing.T) {
	h := newHarness(t)
	tok := h.login("admin", adminPW)
	h.srv.sessMu.Lock()
	s := h.srv.sessions[tok]
	s.expires = time.Now().Add(-time.Second)
	h.srv.sessions[tok] = s
	h.srv.sessMu.Unlock()
	expect(t, h.do("GET", "/api/me", tok, ""), 401, "")
	h.srv.sessMu.Lock()
	_, ok := h.srv.sessions[tok]
	h.srv.sessMu.Unlock()
	if ok {
		t.Fatal("expired session kept")
	}
}

type route struct {
	method, path, body string
	admin              bool
}

func routes(id string) []route {
	p := "/api/instances/" + id
	return []route{
		{"GET", "/api/me", "", false},
		{"GET", "/api/instances", "", false},
		{"POST", "/api/instances", `{"name":"x","host":"h","port":1,"password":"p"}`, true},
		{"PUT", p, `{"name":"x","host":"h","port":1,"password":""}`, true},
		{"DELETE", p, "", true},
		{"GET", p + "/status", "", false},
		{"GET", p + "/sofia", "", false},
		{"GET", p + "/gateways", "", false},
		{"GET", p + "/registrations", "", false},
		{"GET", p + "/channels", "", false},
		{"GET", p + "/calls", "", false},
		{"DELETE", p + "/channels/abc-123", "", true},
		{"GET", p + "/modules", "", false},
		{"GET", p + "/vars", "", false},
		{"GET", p + "/config", "", false},
		{"GET", p + "/config/sofia.conf", "", false},
		{"GET", p + "/dialplan", "", false},
		{"GET", p + "/dialplan/default", "", false},
		{"GET", p + "/directory", "", false},
		{"GET", p + "/directory/10.0.0.1", "", false},
		{"GET", p + "/history", "", false},
		{"POST", p + "/actions", `{"action":"reloadxml"}`, true},
		{"POST", p + "/console", `{"command":"status"}`, true},
		{"GET", "/api/nope", "", false},
		{"POST", "/api/logout", "", false},
	}
}

func TestAuthMatrix(t *testing.T) {
	h := newHarness(t)
	for _, rt := range append(routes(h.id), route{"GET", "/api/instances/" + h.id + "/stream", "", false}) {
		if r := h.do(rt.method, rt.path, "", rt.body); r.code != 401 {
			t.Errorf("%s %s without session: %d", rt.method, rt.path, r.code)
		}
		if r := h.do(rt.method, rt.path, "deadbeef", rt.body); r.code != 401 {
			t.Errorf("%s %s with bogus session: %d", rt.method, rt.path, r.code)
		}
	}

	viewer := h.login("viewer", viewerPW)
	for _, rt := range routes(h.id) {
		if rt.path == "/api/logout" {
			continue
		}
		r := h.do(rt.method, rt.path, viewer, rt.body)
		switch {
		case rt.admin && r.code != 403:
			t.Errorf("viewer %s %s: %d, want 403", rt.method, rt.path, r.code)
		case !rt.admin && (r.code == 401 || r.code == 403):
			t.Errorf("viewer %s %s: %d", rt.method, rt.path, r.code)
		}
	}
	for _, cmd := range func() []string { s, _, _, _ := h.fake(0).snapshot(); return s }() {
		switch cmd {
		case "uuid_kill abc-123", "reloadxml", "status":
			t.Errorf("viewer caused %q", cmd)
		}
	}
	if list := h.st.List(); len(list) != 1 || list[0].Name != "fs01" {
		t.Errorf("viewer changed the store: %+v", list)
	}

	admin := h.login("admin", adminPW)
	for _, rt := range routes(h.id) {
		if rt.method == "DELETE" && rt.path == "/api/instances/"+h.id || rt.path == "/api/logout" {
			continue
		}
		if r := h.do(rt.method, rt.path, admin, rt.body); r.code == 401 || r.code == 403 {
			t.Errorf("admin %s %s: %d", rt.method, rt.path, r.code)
		}
	}
}

func TestInstanceCRUD(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	if f := h.fake(0); f.addr != "10.0.0.5:8021" || f.password != eslPW {
		t.Fatalf("preloaded dial %s %s", f.addr, f.password)
	}

	r := h.do("POST", "/api/instances", admin, `{"name":"v6","host":"::1","port":8022,"password":"`+eslPW+`"}`)
	expect(t, r, 201, "")
	var created instanceView
	r.json(t, &created)
	if created.Name != "v6" || created.Host != "::1" || created.Port != 8022 || len(created.ID) != 16 || !created.State.Connected {
		t.Fatalf("created %+v", created)
	}
	if strings.Contains(r.body, "password") {
		t.Fatalf("view has a password field: %s", r.body)
	}
	if f := h.fake(1); f.addr != "[::1]:8022" || f.password != eslPW {
		t.Fatalf("dial %s %s", f.addr, f.password)
	}

	for _, bad := range []string{
		`{"name":"","host":"h","port":1}`,
		`{"name":"a","host":"h h","port":1}`,
		`{"name":"a","host":"h","port":70000}`,
		`{"name":"a","host":"h","port":"1"}`,
		`{"id":"x","name":"a","host":"h","port":1}`,
		`not json`,
	} {
		expect(t, h.do("POST", "/api/instances", admin, bad), 400, "")
	}

	r = h.do("GET", "/api/instances", admin, "")
	var list []instanceView
	r.json(t, &list)
	if len(list) != 2 || list[0].ID != h.id || list[1].ID != created.ID || !list[0].State.Connected {
		t.Fatalf("list %+v", list)
	}

	// Update with an empty password keeps the stored one and reconnects.
	r = h.do("PUT", "/api/instances/"+created.ID, admin, `{"name":"v6b","host":"fs.example.com","port":9000,"password":""}`)
	expect(t, r, 200, "")
	var updated instanceView
	r.json(t, &updated)
	if updated.ID != created.ID || updated.Name != "v6b" || updated.Host != "fs.example.com" || updated.Port != 9000 {
		t.Fatalf("updated %+v", updated)
	}
	if _, _, _, closed := h.fake(1).snapshot(); !closed {
		t.Fatal("old switch not closed on update")
	}
	if f := h.fake(2); f.addr != "fs.example.com:9000" || f.password != eslPW {
		t.Fatalf("redial %s %s", f.addr, f.password)
	}
	expect(t, h.do("PUT", "/api/instances/"+created.ID, admin, `{"name":"v6b","host":"fs.example.com","port":9000,"password":"new-pw"}`), 200, "")
	if f := h.fake(3); f.password != "new-pw" {
		t.Fatalf("redial password %q", f.password)
	}
	expect(t, h.do("PUT", "/api/instances/0000000000000000", admin, `{"name":"a","host":"h","port":1}`), 404, "")
	expect(t, h.do("PUT", "/api/instances/"+created.ID, admin, `{"name":"a","host":"","port":1}`), 400, "")

	expect(t, h.do("DELETE", "/api/instances/"+created.ID, admin, ""), 204, "")
	if _, _, _, closed := h.fake(3).snapshot(); !closed {
		t.Fatal("switch not closed on delete")
	}
	expect(t, h.do("GET", "/api/instances/"+created.ID+"/status", admin, ""), 404, `{"error":"instance not found"}`)
	expect(t, h.do("DELETE", "/api/instances/"+created.ID, admin, ""), 404, "")
	expect(t, h.do("DELETE", "/api/instances/"+h.id, admin, ""), 204, "")
	expect(t, h.do("GET", "/api/instances", admin, ""), 200, `[]`)
}

func TestDataRoutes(t *testing.T) {
	h := newHarness(t)
	p := "/api/instances/" + h.id
	for _, who := range []string{"admin", "viewer"} {
		pw := map[string]string{"admin": adminPW, "viewer": viewerPW}[who]
		tok := h.login(who, pw)

		r := h.do("GET", p+"/status", tok, "")
		expect(t, r, 200, "")
		var st struct {
			State    esl.State `json:"state"`
			Hostname string    `json:"hostname"`
			Status   struct {
				SystemStatus string `json:"systemStatus"`
			} `json:"status"`
		}
		r.json(t, &st)
		if !st.State.Connected || st.Hostname != "fs01" || st.Status.SystemStatus != "ready" {
			t.Fatalf("status %s", r.body)
		}

		expect(t, h.do("GET", p+"/sofia", tok, ""), 200, `{"rows":[{"data":"sip:mod_sofia@10.0.0.5:5060","name":"internal","state":"RUNNING (0)","type":"profile"}]}`)
		expect(t, h.do("GET", p+"/gateways", tok, ""), 200, `{"rows":[{"name":"gw1","state":"REGED"}]}`)
		expect(t, h.do("GET", p+"/registrations", tok, ""), 200, `{"rows":[]}`)
		expect(t, h.do("GET", p+"/channels", tok, ""), 200, `{"rows":[{"direction":"inbound","uuid":"abc-123"}]}`)
		expect(t, h.do("GET", p+"/calls", tok, ""), 200, `{"rows":[{"b_uuid":"def-456","uuid":"abc-123"}]}`)
		expect(t, h.do("GET", p+"/modules", tok, ""), 200, `{"rows":[{"ikey":"mod_commands","name":"status","type":"api"}]}`)
		expect(t, h.do("GET", p+"/vars", tok, ""), 200, `{"rows":[{"name":"default_password","value":"********"},{"name":"hostname","value":"fs01"}]}`)
		expect(t, h.do("GET", p+"/config", tok, ""), 200, `{"rows":[{"name":"sofia.conf","description":"sofia Endpoint"}]}`)
		r = h.do("GET", p+"/config/sofia.conf", tok, "")
		expect(t, r, 200, "")
		var cfg struct{ Name, XML string }
		r.json(t, &cfg)
		if cfg.Name != "sofia.conf" || strings.Contains(cfg.XML, "gwsecret") || !strings.Contains(cfg.XML, `value="********"`) {
			t.Fatalf("config %s", r.body)
		}
		expect(t, h.do("GET", p+"/config/nope.conf", tok, ""), 404, "")
	}

	admin := h.login("admin", adminPW)
	f := h.fake(0)
	f.set(func(f *fakeSwitch) { f.sent = nil })
	expect(t, h.do("DELETE", p+"/channels/abc-123", admin, ""), 200, `{"output":"+OK"}`)
	expect(t, h.do("POST", p+"/actions", admin, `{"action":"reloadxml"}`), 200, `{"output":"+OK [Success]"}`)
	expect(t, h.do("POST", p+"/actions", admin, `{"action":"reloadacl"}`), 200, `{"output":"+OK acl reloaded"}`)
	expect(t, h.do("POST", p+"/actions", admin, `{"action":"sofia_rescan","profile":"internal"}`), 200, `{"output":"+OK scan complete"}`)
	expect(t, h.do("POST", p+"/console", admin, `{"command":"show calls count"}`), 200, `{"output":"\n0 total.\n\n"}`)
	sent, _, _, _ := f.snapshot()
	want := []string{"uuid_kill abc-123", "reloadxml", "reloadacl", "sofia profile internal rescan", "show calls count"}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent %q", sent)
	}
}

func TestBadInputSendsNothing(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id
	f := h.fake(0)
	f.set(func(f *fakeSwitch) { f.sent = nil })

	for _, path := range []string{
		p + "/channels/abc%20def",
		p + "/channels/a;b",
		p + "/channels/a.b",
		p + "/channels/a%0Ab",
	} {
		expect(t, h.do("DELETE", path, admin, ""), 400, "")
	}
	for _, path := range []string{
		p + "/config/a%20b", p + "/config/x%3Breloadxml", p + "/config/a%0Ab",
		p + "/dialplan/a%20b", p + "/dialplan/default%20name%20x", p + "/dialplan/x%3Breloadxml",
		p + "/directory/a%0Ab", p + "/directory/x%2Fy", p + "/directory/%C3%A4",
	} {
		expect(t, h.do("GET", path, admin, ""), 400, "")
	}
	for _, body := range []string{
		`{"action":"sofia_rescan"}`,
		`{"action":"sofia_rescan","profile":"internal rescan"}`,
		`{"action":"sofia_rescan","profile":"a\nb"}`,
		`{"action":"shutdown"}`,
		`{"action":"reloadxml","extra":true}`,
		`{}`,
	} {
		expect(t, h.do("POST", p+"/actions", admin, body), 400, "")
	}
	for _, body := range []string{`{"command":""}`, `{"command":"status\nshutdown"}`, `{"command":"a\rb"}`, `{"cmd":"x"}`} {
		expect(t, h.do("POST", p+"/console", admin, body), 400, "")
	}
	expect(t, h.do("GET", p+"/stream?loglevel=8", admin, ""), 400, "")
	expect(t, h.do("GET", p+"/stream?loglevel=x", admin, ""), 400, "")
	if sent, _, _, _ := f.snapshot(); len(sent) != 0 {
		t.Fatalf("sent %q", sent)
	}
}

func TestDisconnected(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	f := h.fake(0)
	f.set(func(f *fakeSwitch) {
		f.state = esl.State{Connected: false, Since: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), LastError: "dial tcp: connection refused"}
		f.sent = nil
	})
	r := h.do("GET", "/api/instances/"+h.id+"/status", admin, "")
	expect(t, r, 200, `{"state":{"connected":false,"since":"2026-01-02T03:04:05Z","lastError":"dial tcp: connection refused"},"hostname":"","status":null}`)

	for _, rt := range routes(h.id) {
		if !strings.HasPrefix(rt.path, "/api/instances/"+h.id+"/") || strings.HasSuffix(rt.path, "/status") || strings.HasSuffix(rt.path, "/history") {
			continue
		}
		expect(t, h.do(rt.method, rt.path, admin, rt.body), 503, `{"error":"instance not connected"}`)
	}
	if sent, _, _, _ := f.snapshot(); len(sent) != 0 {
		t.Fatalf("sent %q", sent)
	}
}

func TestTransportError(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id
	h.fake(0).set(func(f *fakeSwitch) { f.apiErr = errors.New("esl: read tcp: connection reset by peer") })
	want := `{"error":"esl: read tcp: connection reset by peer"}`
	expect(t, h.do("GET", p+"/status", admin, ""), 503, want)
	expect(t, h.do("GET", p+"/channels", admin, ""), 503, want)
	expect(t, h.do("POST", p+"/console", admin, `{"command":"status"}`), 503, want)
	expect(t, h.do("POST", p+"/actions", admin, `{"action":"reloadxml"}`), 503, want)
}

func TestErrReply(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id
	h.fake(0).set(func(f *fakeSwitch) {
		f.replies["sofia xmlstatus"] = "-ERR sofia Command not found!\n"
		f.replies["show channels as json"] = "-ERR SQL error\n"
		f.replies["uuid_kill abc-123"] = "-ERR No such channel!\n"
		f.replies["reloadxml"] = "-ERR [file.xml:3 error]\n"
		f.replies["hostname"] = "-ERR no\n"
		f.replies["xml_locate configuration"] = "<section><configuration"
		f.replies["xml_locate dialplan"] = "-ERR dialplan\n"
		f.replies["xml_locate directory domain name 10.0.0.1"] = "-ERR directory\n"
	})
	expect(t, h.do("GET", p+"/dialplan", admin, ""), 502, `{"error":"-ERR dialplan"}`)
	expect(t, h.do("GET", p+"/directory/10.0.0.1", admin, ""), 502, `{"error":"-ERR directory"}`)
	expect(t, h.do("GET", p+"/sofia", admin, ""), 502, `{"error":"-ERR sofia Command not found!"}`)
	expect(t, h.do("GET", p+"/channels", admin, ""), 502, `{"error":"-ERR SQL error"}`)
	expect(t, h.do("DELETE", p+"/channels/abc-123", admin, ""), 502, `{"error":"-ERR No such channel!"}`)
	expect(t, h.do("POST", p+"/actions", admin, `{"action":"reloadxml"}`), 502, `{"error":"-ERR [file.xml:3 error]"}`)
	expect(t, h.do("GET", p+"/status", admin, ""), 502, `{"error":"-ERR no"}`)
	expect(t, h.do("GET", p+"/config", admin, ""), 502, "")
	expect(t, h.do("POST", p+"/console", admin, `{"command":"bogus"}`), 200, `{"output":"-ERR bogus Command not found!\n"}`)
}

func TestHeadersStaticAndUnknownAPI(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	security := map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
	}
	check := func(r response, api bool) {
		t.Helper()
		for k, v := range security {
			if got := r.header.Get(k); got != v {
				t.Errorf("%s = %q", k, got)
			}
		}
		want := "no-cache"
		if api {
			want = "no-store"
		}
		if got := r.header.Get("Cache-Control"); got != want {
			t.Errorf("Cache-Control = %q, want %q", got, want)
		}
	}

	r := h.do("GET", "/", "", "")
	expect(t, r, 200, "<!doctype html><title>fsdeck</title>")
	check(r, false)
	r = h.do("GET", "/missing.js", "", "")
	expect(t, r, 404, "")
	check(r, false)

	r = h.do("GET", "/api/me", admin, "")
	check(r, true)
	r = h.do("GET", "/api/me", "", "")
	check(r, true)
	r = h.do("GET", "/api/nope", admin, "")
	expect(t, r, 404, `{"error":"not found"}`)
	check(r, true)
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	expect(t, h.do("POST", "/api/me", admin, `{}`), 404, `{"error":"not found"}`)
	expect(t, h.do("GET", "/api/instances/"+h.id+"/nope", admin, ""), 404, `{"error":"not found"}`)
	expect(t, h.do("GET", "/api/instances/0000000000000000/channels", admin, ""), 404, `{"error":"instance not found"}`)
}

func TestBodyRules(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	send := func(method, path, ctype, body string) int {
		req, _ := http.NewRequest(method, h.ts.URL+path, strings.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		req.AddCookie(&http.Cookie{Name: cookieName, Value: admin})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	body := `{"name":"a","host":"h","port":1,"password":"p"}`
	if c := send("POST", "/api/instances", "text/plain", body); c != 415 {
		t.Errorf("text/plain: %d", c)
	}
	if c := send("POST", "/api/instances", "", body); c != 415 {
		t.Errorf("no content type: %d", c)
	}
	if c := send("DELETE", "/api/instances/"+h.id, "text/plain", "x"); c != 415 {
		t.Errorf("delete with body: %d", c)
	}
	if c := send("POST", "/api/instances", "application/json; charset=utf-8", body); c != 201 {
		t.Errorf("json with charset: %d", c)
	}
	big := `{"name":"` + strings.Repeat("a", 1<<20) + `","host":"h","port":1}`
	if c := send("POST", "/api/instances", "application/json", big); c != 413 {
		t.Errorf("large body: %d", c)
	}
	if c := send("POST", "/api/instances", "application/json", ""); c != 400 {
		t.Errorf("empty body: %d", c)
	}
}

type sseEvent struct {
	name, data string // name "comment" for ": ..." lines
}

// openStream returns the events of an SSE response; the channel closes when
// the stream ends.
func (h *harness) openStream(token, query string) (<-chan sseEvent, context.CancelFunc) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", h.ts.URL+"/api/instances/"+h.id+"/stream"+query, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-store" {
		h.t.Fatalf("stream: %d %v", resp.StatusCode, resp.Header)
	}
	ch := make(chan sseEvent, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, ": "):
				ch <- sseEvent{name: "comment", data: line[2:]}
			case strings.HasPrefix(line, "event: "):
				ev.name = line[7:]
			case strings.HasPrefix(line, "data: "):
				if ev.data != "" {
					h.t.Errorf("multi-line data")
				}
				ev.data = line[6:]
			case line == "" && ev.name != "":
				ch <- ev
				ev = sseEvent{}
			}
		}
	}()
	return ch, cancel
}

func next(t *testing.T, ch <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("stream ended")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return sseEvent{}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func logMsg(level, text string) esl.Message {
	return esl.Message{Kind: esl.KindLog, Body: text + "\n", Headers: map[string]string{
		"Log-Level": level, "Log-File": "switch_core.c", "Log-Func": "fn", "Log-Line": "123", "Text-Channel": "3",
	}}
}

var heartbeat = esl.Message{Kind: esl.KindEvent, Headers: map[string]string{"Event-Name": "HEARTBEAT", "Session-Count": "0", "Idle-CPU": "98.5"}}

func TestStreamAdmin(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	f := h.fake(0)
	ch, cancel := h.openStream(admin, "?loglevel=4")
	defer cancel()

	ev := next(t, ch)
	if ev.name != "state" || ev.data != `{"connected":true,"since":"2026-01-02T03:04:05Z","lastError":""}` {
		t.Fatalf("first event %+v", ev)
	}
	f.publish(logMsg("7", "debug line"))
	f.publish(logMsg("x", "bad level"))
	f.publish(logMsg("3", "error line\nsecond line"))
	f.publish(esl.Message{Kind: esl.KindEvent, Headers: map[string]string{"Event-Name": "CUSTOM"}})
	f.publish(heartbeat)
	f.set(func(f *fakeSwitch) {
		f.state = esl.State{Connected: false, Since: time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC), LastError: "EOF"}
	})
	f.publish(esl.Message{Kind: esl.KindState, Headers: map[string]string{"connected": "false", "error": "EOF"}})

	if ev := next(t, ch); ev.name != "log" || ev.data != `{"level":3,"text":"error line\nsecond line","file":"switch_core.c","func":"fn","line":123}` {
		t.Fatalf("log event %+v", ev)
	}
	ev = next(t, ch)
	var hb map[string]string
	json.Unmarshal([]byte(ev.data), &hb)
	if ev.name != "heartbeat" || !reflect.DeepEqual(hb, heartbeat.Headers) {
		t.Fatalf("heartbeat %+v", ev)
	}
	if ev := next(t, ch); ev.name != "state" || ev.data != `{"connected":false,"since":"2026-01-02T03:04:06Z","lastError":"EOF"}` {
		t.Fatalf("state %+v", ev)
	}
}

func TestStreamViewerGetsNoLogs(t *testing.T) {
	h := newHarness(t)
	viewer := h.login("viewer", viewerPW)
	f := h.fake(0)
	ch, cancel := h.openStream(viewer, "?loglevel=7")
	defer cancel()
	if ev := next(t, ch); ev.name != "state" {
		t.Fatalf("first %+v", ev)
	}
	f.publish(logMsg("0", "console"))
	f.publish(heartbeat)
	if ev := next(t, ch); ev.name != "heartbeat" {
		t.Fatalf("viewer got %+v", ev)
	}
	if _, levels, _, _ := f.snapshot(); len(levels) != 0 {
		t.Fatalf("viewer changed the log level: %v", levels)
	}

	// Admin without loglevel gets no logs either.
	admin := h.login("admin", adminPW)
	ch2, cancel2 := h.openStream(admin, "")
	defer cancel2()
	next(t, ch2)
	f.publish(logMsg("0", "console"))
	f.publish(heartbeat)
	if ev := next(t, ch2); ev.name != "heartbeat" {
		t.Fatalf("admin without loglevel got %+v", ev)
	}
}

func TestStreamSharedLogLevel(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	f := h.fake(0)
	levels := func() []int { _, l, _, _ := f.snapshot(); return l }
	expectLevels := func(want ...int) {
		t.Helper()
		waitFor(t, "log levels", func() bool { return reflect.DeepEqual(levels(), want) })
	}

	a, cancelA := h.openStream(admin, "?loglevel=5")
	next(t, a)
	expectLevels(5)
	b, cancelB := h.openStream(admin, "?loglevel=7")
	next(t, b)
	expectLevels(5, 7)
	c, cancelC := h.openStream(admin, "?loglevel=5")
	next(t, c)
	expectLevels(5, 7)

	// Each stream filters on its own level.
	f.publish(logMsg("6", "info"))
	f.publish(heartbeat)
	if ev := next(t, a); ev.name != "heartbeat" {
		t.Fatalf("level 5 stream got %+v", ev)
	}
	if ev := next(t, b); ev.name != "log" {
		t.Fatalf("level 7 stream got %+v", ev)
	}

	cancelB()
	expectLevels(5, 7, 5)
	cancelA()
	time.Sleep(50 * time.Millisecond)
	expectLevels(5, 7, 5)
	cancelC()
	expectLevels(5, 7, 5, -1)
	// The history collector keeps its own subscription.
	waitFor(t, "unsubscribe", func() bool { _, _, n, _ := f.snapshot(); return n == 1 })
}

func TestStreamPingAndEnd(t *testing.T) {
	h := newHarness(t)
	h.srv.pingInterval = 20 * time.Millisecond
	admin := h.login("admin", adminPW)
	ch, cancel := h.openStream(admin, "?loglevel=7")
	defer cancel()
	next(t, ch)
	if ev := next(t, ch); ev.name != "comment" || ev.data != "ping" {
		t.Fatalf("got %+v", ev)
	}
	f := h.fake(0)
	expect(t, h.do("DELETE", "/api/instances/"+h.id, admin, ""), 204, "")
	select {
	case <-func() chan struct{} {
		done := make(chan struct{})
		go func() {
			for range ch {
			}
			close(done)
		}()
		return done
	}():
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after delete")
	}
	// The deleted switch is closed, so the level is not reset on it.
	if _, levels, _, closed := f.snapshot(); !closed || !reflect.DeepEqual(levels, []int{7}) {
		t.Fatalf("closed %v levels %v", closed, levels)
	}
}

func TestStreamUnknownInstance(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	expect(t, h.do("GET", "/api/instances/0000000000000000/stream", admin, ""), 404, `{"error":"instance not found"}`)
}

func (h *harness) loginAs(username, password string) response {
	h.t.Helper()
	return h.do("POST", "/api/login", "", `{"username":"`+username+`","password":"`+password+`"}`)
}

// loginFrom logs in from another client address.
func (h *harness) loginFrom(remoteAddr, username, password string) int {
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func expectBlocked(t *testing.T, r response, secs string) {
	t.Helper()
	expect(t, r, 429, `{"error":"too many failed attempts; try again in `+secs+`s"}`)
	if got := r.header.Get("Retry-After"); got != secs {
		t.Fatalf("Retry-After %q, want %q", got, secs)
	}
	if len(r.resp.Cookies()) != 0 {
		t.Fatal("blocked login set a cookie")
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		expect(t, h.loginAs("admin", "wrong"), 401, `{"error":"invalid username or password"}`)
	}
	// Blocked: the password is not checked and the failed-login delay is
	// skipped. Less than a second has passed, so 60s rounds up to 60.
	h.srv.loginDelay = time.Second
	start := time.Now()
	expectBlocked(t, h.loginAs("admin", adminPW), "60")
	expectBlocked(t, h.loginAs("admin", "wrong"), "60")
	if d := time.Since(start); d >= time.Second {
		t.Fatalf("blocked login answered after %v", d)
	}
	h.srv.loginDelay = 0

	// Other usernames and other addresses are separate keys.
	h.login("viewer", viewerPW)
	expect(t, h.loginAs("root", "x"), 401, "")
	if c := h.loginFrom("192.0.2.7:4000", "admin", adminPW); c != 200 {
		t.Fatalf("other IPv4 address: %d", c)
	}
	if c := h.loginFrom("[2001:db8::7]:4000", "admin", adminPW); c != 200 {
		t.Fatalf("other IPv6 address: %d", c)
	}
	// The test client is 127.0.0.1 on another port: same key.
	if c := h.loginFrom("127.0.0.1:1", "admin", adminPW); c != 429 {
		t.Fatalf("same address, other port: %d", c)
	}
}

func TestLoginRateLimitSuccessResets(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 4; i++ {
		expect(t, h.loginAs("admin", "wrong"), 401, "")
	}
	h.login("admin", adminPW)
	for i := 0; i < 4; i++ {
		expect(t, h.loginAs("admin", "wrong"), 401, "")
	}
	h.login("admin", adminPW)
	for i := 0; i < 5; i++ {
		expect(t, h.loginAs("admin", "wrong"), 401, "")
	}
	expectBlocked(t, h.loginAs("admin", adminPW), "60")
}

func TestLoginRateLimitBlockExpires(t *testing.T) {
	h := newHarness(t)
	h.srv.loginBlock = 400 * time.Millisecond
	for i := 0; i < 5; i++ {
		expect(t, h.loginAs("admin", "wrong"), 401, "")
	}
	// A partial second rounds up to 1. Attempts while blocked are not
	// counted and do not extend the block.
	for i := 0; i < 3; i++ {
		expectBlocked(t, h.loginAs("admin", "wrong"), "1")
	}
	time.Sleep(450 * time.Millisecond)
	// The five failures are still within the window, so one more failure
	// blocks again at once.
	expect(t, h.loginAs("admin", "wrong"), 401, "")
	expectBlocked(t, h.loginAs("admin", adminPW), "1")
	time.Sleep(450 * time.Millisecond)
	h.login("admin", adminPW)
	for i := 0; i < 4; i++ {
		expect(t, h.loginAs("admin", "wrong"), 401, "")
	}
}

func TestLoginRateLimitWindow(t *testing.T) {
	h := newHarness(t)
	h.srv.loginWindow = 200 * time.Millisecond
	for i := 0; i < 4; i++ {
		expect(t, h.loginAs("admin", "wrong"), 401, "")
	}
	time.Sleep(250 * time.Millisecond)
	// The earlier failures have left the window.
	for i := 0; i < 4; i++ {
		expect(t, h.loginAs("admin", "wrong"), 401, "")
	}
	h.login("admin", adminPW)
}

func TestLoginRateLimitConcurrent(t *testing.T) {
	h := newHarness(t)
	codes := make(chan int, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- h.loginFrom("198.51.100.1:1", "admin", "wrong")
		}()
	}
	wg.Wait()
	close(codes)
	n := map[int]int{}
	for c := range codes {
		n[c]++
	}
	if n[401] != 5 || n[429] != 15 {
		t.Fatalf("codes %v", n)
	}
}

func TestClientKey(t *testing.T) {
	for addr, ip := range map[string]string{
		"192.0.2.1:5000":          "192.0.2.1",
		"[2001:db8::1]:5000":      "2001:db8::1",
		"[2001:DB8:0::1]:5000":    "2001:db8::1",
		"[::ffff:192.0.2.1]:5000": "192.0.2.1",
		"[fe80::1%en0]:5000":      "fe80::1%en0",
		"@":                       "@", // e.g. a unix socket listener
	} {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = addr
		if k := clientKey(r, "admin"); k != (loginKey{"admin", ip}) {
			t.Errorf("%s: %+v", addr, k)
		}
	}
	r := httptest.NewRequest("POST", "/", nil)
	if k := clientKey(r, strings.Repeat("u", 1000)); len(k.username) != 64 {
		t.Errorf("username length %d", len(k.username))
	}
}

func TestLoginRateLimitCap(t *testing.T) {
	h := newHarness(t)
	s := h.srv
	s.loginMaxKeys = 3
	key := func(n string) loginKey { return loginKey{n, "192.0.2.1"} }
	keys := func() []string {
		s.loginMu.Lock()
		defer s.loginMu.Unlock()
		var out []string
		for k := range s.loginFails {
			out = append(out, k.username)
		}
		sort.Strings(out)
		return out
	}
	t0 := time.Now()
	for i, n := range []string{"a", "b", "c"} {
		s.loginAttempt(key(n), t0.Add(time.Duration(i)*time.Second))
	}
	// Full and nothing expired: the least recently failed key goes.
	s.loginAttempt(key("d"), t0.Add(3*time.Second))
	if got := keys(); !reflect.DeepEqual(got, []string{"b", "c", "d"}) {
		t.Fatalf("keys %v", got)
	}
	// b has expired; it is dropped and c, the oldest live key, stays.
	s.loginAttempt(key("e"), t0.Add(s.loginWindow+1500*time.Millisecond))
	if got := keys(); !reflect.DeepEqual(got, []string{"c", "d", "e"}) {
		t.Fatalf("keys %v", got)
	}
	// A blocked key outlives its window until the block ends.
	s.loginWindow = time.Second
	s.loginBlock = time.Hour
	for i := 0; i < 5; i++ {
		s.loginAttempt(key("c"), t0.Add(6*time.Minute))
	}
	s.loginAttempt(key("f"), t0.Add(10*time.Minute))
	if got := keys(); !reflect.DeepEqual(got, []string{"c", "f"}) {
		t.Fatalf("keys %v", got)
	}
	if wait := s.loginAttempt(key("c"), t0.Add(10*time.Minute)); wait <= 0 {
		t.Fatal("blocked key lost")
	}
}

func heartbeatWith(count, rate, idle string) esl.Message {
	return esl.Message{Kind: esl.KindEvent, Headers: map[string]string{
		"Event-Name": "HEARTBEAT", "Session-Count": count, "Session-Per-Sec-Last": rate, "Idle-CPU": idle,
	}}
}

func (h *harness) instance(id string) *instance {
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	return h.srv.insts[id]
}

type historyReply struct {
	Samples []sample `json:"samples"`
}

func TestHistory(t *testing.T) {
	h := newHarness(t)
	viewer := h.login("viewer", viewerPW)
	p := "/api/instances/" + h.id + "/history"
	expect(t, h.do("GET", p, viewer, ""), 200, `{"samples":[]}`)

	f := h.fake(0)
	inst := h.instance(h.id)
	before := time.Now().Unix()
	f.publish(heartbeatWith("3", "2", "97.25"))
	f.publish(logMsg("3", "log line"))
	f.publish(esl.Message{Kind: esl.KindEvent, Headers: map[string]string{"Event-Name": "CUSTOM", "Session-Count": "9"}})
	f.publish(esl.Message{Kind: esl.KindState, Headers: map[string]string{"connected": "true"}})
	f.publish(heartbeatWith("x", "", "NaN"))
	f.publish(esl.Message{Kind: esl.KindEvent, Headers: map[string]string{"Event-Name": "HEARTBEAT", "Idle-CPU": "Inf"}})
	f.publish(heartbeatWith("1.5", "-1", "1e400"))
	waitFor(t, "samples", func() bool { return len(inst.samples()) == 4 })
	after := time.Now().Unix()

	// Readable while disconnected; nothing is sent.
	f.set(func(f *fakeSwitch) { f.state.Connected = false; f.sent = nil })
	r := h.do("GET", p, viewer, "")
	expect(t, r, 200, "")
	var got historyReply
	r.json(t, &got)
	want := []sample{{Sessions: 3, Rate: 2, Idle: 97.25}, {}, {}, {Rate: -1}}
	for i := range got.Samples {
		if ts := got.Samples[i].T; ts < before || ts > after {
			t.Errorf("sample %d time %d", i, ts)
		}
		got.Samples[i].T = 0
	}
	if !reflect.DeepEqual(got.Samples, want) {
		t.Fatalf("samples %s", r.body)
	}
	if sent, levels, _, _ := f.snapshot(); len(sent) != 0 || len(levels) != 0 {
		t.Fatalf("sent %q, log levels %v", sent, levels)
	}
	expect(t, h.do("GET", "/api/instances/0000000000000000/history", viewer, ""), 404, `{"error":"instance not found"}`)
}

func TestHistoryRing(t *testing.T) {
	inst := newInstance(newFake("a", "b"))
	defer inst.close()
	for i := 0; i < 3; i++ {
		inst.addSample(sample{T: int64(i)})
	}
	if got := inst.samples(); len(got) != 3 || got[0].T != 0 || got[2].T != 2 {
		t.Fatalf("samples %v", got)
	}
	for i := 3; i < historySize+40; i++ {
		inst.addSample(sample{T: int64(i)})
	}
	got := inst.samples()
	if len(got) != historySize {
		t.Fatalf("%d samples", len(got))
	}
	for i, s := range got {
		if s.T != int64(40+i) {
			t.Fatalf("sample %d: %d", i, s.T)
		}
	}
}

func stopped(inst *instance) bool {
	select {
	case <-inst.collected:
		return true
	default:
		return false
	}
}

func TestHistoryCollectorLifetime(t *testing.T) {
	h := newHarness(t)
	admin := h.login("admin", adminPW)
	p := "/api/instances/" + h.id

	// Update: the old collector has returned when the PUT answers, and the
	// new instance starts with an empty history.
	old := h.instance(h.id)
	h.fake(0).publish(heartbeat)
	waitFor(t, "sample", func() bool { return len(old.samples()) == 1 })
	expect(t, h.do("PUT", p, admin, `{"name":"fs01","host":"10.0.0.6","port":8021,"password":""}`), 200, "")
	if !stopped(old) {
		t.Fatal("collector running after update")
	}
	if _, _, subs, _ := h.fake(0).snapshot(); subs != 0 {
		t.Fatalf("old switch has %d subscribers", subs)
	}
	expect(t, h.do("GET", p+"/history", admin, ""), 200, `{"samples":[]}`)
	if _, _, subs, _ := h.fake(1).snapshot(); subs != 1 {
		t.Fatalf("new switch has %d subscribers", subs)
	}

	// Delete.
	cur := h.instance(h.id)
	expect(t, h.do("DELETE", p, admin, ""), 204, "")
	if !stopped(cur) {
		t.Fatal("collector running after delete")
	}

	// The switch closing on its own ends the collector too.
	r := h.do("POST", "/api/instances", admin, `{"name":"b","host":"h","port":1,"password":"p"}`)
	var v instanceView
	r.json(t, &v)
	inst := h.instance(v.ID)
	h.fake(2).Close()
	waitFor(t, "collector end", func() bool { return stopped(inst) })

	// Server.Close.
	r = h.do("POST", "/api/instances", admin, `{"name":"c","host":"h","port":1,"password":"p"}`)
	r.json(t, &v)
	inst = h.instance(v.ID)
	h.srv.Close()
	if !stopped(inst) {
		t.Fatal("collector running after Close")
	}
}

func TestDialplanDirectoryRoutes(t *testing.T) {
	h := newHarness(t)
	p := "/api/instances/" + h.id
	for _, who := range []string{"admin", "viewer"} {
		tok := h.login(who, map[string]string{"admin": adminPW, "viewer": viewerPW}[who])
		expect(t, h.do("GET", p+"/dialplan", tok, ""), 200, `{"rows":[{"name":"default","count":2},{"name":"public","count":1}]}`)
		expect(t, h.do("GET", p+"/directory", tok, ""), 200, `{"rows":[{"name":"10.0.0.1","count":3}]}`)

		var doc struct{ Name, XML string }
		r := h.do("GET", p+"/dialplan/default", tok, "")
		expect(t, r, 200, "")
		r.json(t, &doc)
		if doc.Name != "default" || !strings.Contains(doc.XML, `<action application="bridge" data="user/1000"/>`) {
			t.Fatalf("dialplan %s", r.body)
		}
		r = h.do("GET", p+"/directory/10.0.0.1", tok, "")
		expect(t, r, 200, "")
		r.json(t, &doc)
		for _, secret := range []string{"sip-S3cret", "vm-S3cret", "a1-S3cret"} {
			if strings.Contains(r.body, secret) {
				t.Fatalf("directory leaks %s: %s", secret, r.body)
			}
		}
		if doc.Name != "10.0.0.1" || strings.Count(doc.XML, `value="********"`) != 3 || !strings.Contains(doc.XML, `<variable name="user_context" value="default"/>`) {
			t.Fatalf("directory %s", r.body)
		}
		expect(t, h.do("GET", p+"/dialplan/nope", tok, ""), 404, `{"error":"not found: context nope"}`)
		expect(t, h.do("GET", p+"/directory/nope", tok, ""), 404, `{"error":"not found: domain nope"}`)
	}
	admin := h.login("admin", adminPW)
	h.fake(0).set(func(f *fakeSwitch) {
		f.replies["xml_locate dialplan"] = "can't find anything\n"
		f.replies["xml_locate directory"] = "can't find anything\n"
	})
	expect(t, h.do("GET", p+"/dialplan", admin, ""), 200, `{"rows":[]}`)
	expect(t, h.do("GET", p+"/directory", admin, ""), 200, `{"rows":[]}`)
	expect(t, h.do("GET", p+"/dialplan/a%20b", admin, ""), 400, `{"error":"invalid argument: invalid context name"}`)
	expect(t, h.do("GET", p+"/directory/a%20b", admin, ""), 400, `{"error":"invalid argument: invalid domain name"}`)
}
