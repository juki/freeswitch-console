package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juki/freeswitch-console/internal/store"
)

// hookServer records the requests it gets and answers with answer.
type hookServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []hookRequest
}

type hookRequest struct {
	path, contentType, agent, body string
}

func newHookServer(t *testing.T, answer func(w http.ResponseWriter, r *http.Request, n int)) *hookServer {
	t.Helper()
	hs := &hookServer{}
	hs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hs.mu.Lock()
		hs.requests = append(hs.requests, hookRequest{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("User-Agent"), string(b)})
		n := len(hs.requests)
		hs.mu.Unlock()
		answer(w, r, n)
	}))
	t.Cleanup(hs.Close)
	return hs
}

func (hs *hookServer) got() []hookRequest {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]hookRequest(nil), hs.requests...)
}

// logged captures the log output of the test.
func logged(t *testing.T) func() string {
	var mu sync.Mutex
	var buf bytes.Buffer
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

var gatewayFiring = alertEntry{
	alertView: alertView{ID: "7", Rule: ruleGatewayDown, InstanceID: "abc", Instance: "fs<01>&", Subject: "carrier-a",
		Since: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).UnixMilli(), FiredAt: time.Date(2026, 3, 1, 10, 1, 0, 0, time.UTC).UnixMilli(),
		State: "FAIL_WAIT", Ping: "DOWN"},
	Seq: 7, T: time.Date(2026, 3, 1, 10, 1, 0, 0, time.UTC).UnixMilli(), Event: "firing",
}

func localTime(ms int64) string { return time.UnixMilli(ms).Format("2006-01-02 15:04:05 MST") }

func TestHookBodies(t *testing.T) {
	h := alertHarness(t)
	hs := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { io.WriteString(w, "ok") })
	e := gatewayFiring
	since := localTime(e.Since)
	en := "[FreeSWITCH Console] DOWN: gateway carrier-a on fs<01>& is FAIL_WAIT, ping DOWN · since " + since
	zh := "[FreeSWITCH Console] 故障：fs<01>& 的网关 carrier-a 状态为 FAIL_WAIT，探测失败 · 开始于 " + since
	slack := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(en)
	for _, c := range []struct{ format, language, want string }{
		{"slack", "en", `{"text":"` + slack + `"}`},
		{"dingtalk", "zh-CN", `{"msgtype":"text","text":{"content":"` + zh + `"}}`},
		{"wecom", "en", `{"msgtype":"text","text":{"content":"` + en + `"}}`},
		{"feishu", "zh-CN", `{"msg_type":"text","content":{"text":"` + zh + `"}}`},
		{"json", "en", `{"source":"fsconsole","version":"dev","event":"firing","rule":"gatewayDown","instance":{"id":"abc","name":"fs<01>&"},"subject":"carrier-a","summary":"` + en +
			`","state":"FAIL_WAIT","ping":"DOWN","reason":"","value":0,"threshold":0,"sessions":0,"limit":0,"since":"` + time.UnixMilli(e.Since).Format(time.RFC3339) +
			`","at":"` + time.UnixMilli(e.T).Format(time.RFC3339) + `","durationSeconds":0}`},
	} {
		res := h.srv.post(context.Background(), store.AlertTarget{Name: c.format, Format: c.format, Language: c.language, URL: hs.URL + "/" + c.format}, e)
		if !res.ok || res.status != 200 || res.detail != "ok" || res.why != "" {
			t.Fatalf("%s: %+v", c.format, res)
		}
		got := hs.got()
		r := got[len(got)-1]
		if r.path != "/"+c.format || r.contentType != "application/json" || r.agent != "fsconsole/dev" || r.body != c.want {
			t.Errorf("%s:\n got %+v\nwant %s", c.format, r, c.want)
		}
		if !json.Valid([]byte(r.body)) {
			t.Errorf("%s: not JSON", c.format)
		}
	}
}

func TestAlertSummaries(t *testing.T) {
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
	ts := localTime(at)
	down := alertView{Rule: ruleInstanceDown, Instance: "fs01", Since: at, Reason: "dial tcp: connection refused"}
	gw := alertView{Rule: ruleGatewayDown, Instance: "fs01", Subject: "gw1", Since: at, State: "TIMEOUT", Ping: "UP"}
	gwOK := gw
	gwOK.State = "REGED"
	idle := alertView{Rule: ruleIdleCPULow, Instance: "fs01", Since: at, Value: 7.5, Threshold: 10}
	idleOK := idle
	idleOK.Value = 42
	sess := alertView{Rule: ruleSessionsHigh, Instance: "fs01", Since: at, Value: 95, Threshold: 90, Sessions: 950, Limit: 1000}
	sessOK := sess
	sessOK.Value, sessOK.Sessions = 10, 100
	long := strings.Repeat("x", 250)
	for _, c := range []struct {
		v      alertView
		event  string
		dur    time.Duration
		en, zh string
	}{
		{down, "firing", 0, "DOWN: fs01 cannot be reached: dial tcp: connection refused · since " + ts, "故障：无法连接 fs01（dial tcp: connection refused） · 开始于 " + ts},
		{alertView{Rule: ruleInstanceDown, Instance: long, Since: at, Reason: long}, "firing", 0,
			"DOWN: " + long[:200] + " cannot be reached: " + long[:200] + " · since " + ts, "故障：无法连接 " + long[:200] + "（" + long[:200] + "） · 开始于 " + ts},
		{alertView{Rule: ruleInstanceDown, Instance: "fs01", Since: at}, "firing", 0, "DOWN: fs01 cannot be reached · since " + ts, "故障：无法连接 fs01 · 开始于 " + ts},
		{down, "resolved", 5*time.Minute + 12*time.Second, "OK: fs01 is connected again · was down for 5m 12s", "恢复：fs01 已重新连接 · 持续 5分12秒"},
		{gw, "firing", 0, "DOWN: gateway gw1 on fs01 is TIMEOUT · since " + ts, "故障：fs01 的网关 gw1 状态为 TIMEOUT · 开始于 " + ts},
		{gwOK, "resolved", 2*time.Hour + 3*time.Minute + 4*time.Second, "OK: gateway gw1 on fs01 is REGED again · was down for 2h 3m", "恢复：fs01 的网关 gw1 已恢复（REGED） · 持续 2小时3分"},
		{idle, "firing", 0, "WARNING: fs01 idle CPU is 7.5% (threshold 10%) · since " + ts, "警告：fs01 空闲 CPU 为 7.5%（阈值 10%） · 开始于 " + ts},
		{idleOK, "resolved", 40 * time.Second, "OK: fs01 idle CPU is back to 42% · lasted 40s", "恢复：fs01 空闲 CPU 回到 42% · 持续 40秒"},
		{sess, "firing", 0, "WARNING: fs01 has 950 of 1000 sessions (95%, threshold 90%) · since " + ts, "警告：fs01 会话数 950 / 1000（95%，阈值 90%） · 开始于 " + ts},
		{sessOK, "resolved", time.Minute, "OK: fs01 is back to 100 of 1000 sessions (10%) · lasted 1m 0s", "恢复：fs01 会话数回到 100 / 1000（10%） · 持续 1分0秒"},
		{alertView{}, "test", 0, "Test notification. If you can read this, the webhook works.", "测试通知。能看到这条消息，说明 Webhook 配置正确。"},
	} {
		e := alertEntry{alertView: c.v, Event: c.event, DurationMs: c.dur.Milliseconds()}
		if got := alertSummary("en", e); got != "[FreeSWITCH Console] "+c.en {
			t.Errorf("en:\n got %s\nwant %s", got, c.en)
		}
		if got := alertSummary("zh-CN", e); got != "[FreeSWITCH Console] "+c.zh {
			t.Errorf("zh-CN:\n got %s\nwant %s", got, c.zh)
		}
	}
}

func TestHookOutcomes(t *testing.T) {
	h := alertHarness(t)
	h.srv.hookRetry = []time.Duration{time.Millisecond, 5 * time.Millisecond}
	var flaky, down int
	var mu sync.Mutex
	hs := newHookServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/ok":
			io.WriteString(w, "ok")
		case "/dingtalk-error":
			io.WriteString(w, `{"errcode":310000,"errmsg":"keywords not in content"}`)
		case "/feishu-error":
			io.WriteString(w, `{"code":19021,"msg":"sign match fail or timestamp is not within one hour from current time"}`)
		case "/feishu-ok":
			io.WriteString(w, `{"StatusCode":0,"StatusMessage":"success","code":0,"data":{},"msg":"success"}`)
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/missing":
			http.Error(w, "no such hook", http.StatusNotFound)
		case "/flaky":
			flaky++
			switch flaky {
			case 1:
				http.Error(w, "busy", http.StatusServiceUnavailable)
			case 2:
				http.Error(w, "slow down", http.StatusTooManyRequests)
			default:
				io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
			}
		case "/down/SECRETPATHtoken":
			down++
			http.Error(w, "upstream failed for "+r.URL.String(), http.StatusBadGateway)
		case "/big":
			io.WriteString(w, strings.Repeat("y", 10000))
		}
	})
	sendURL := func(url string) (*alertEntry, hookResult) {
		cfg := store.AlertTarget{Name: "t", Format: "json", Language: "en", Enabled: true, URL: url}
		e := gatewayFiring
		e.Deliveries = []alertDelivery{}
		h.srv.deliver(&target{ctx: context.Background(), cfg: cfg}, notice{entry: &e, data: e})
		return &e, h.srv.post(context.Background(), cfg, e)
	}
	send := func(path string) (*alertEntry, hookResult) { return sendURL(hs.URL + path) }
	count := func(path string) int {
		n := 0
		for _, r := range hs.got() {
			if r.path == path {
				n++
			}
		}
		return n
	}
	for _, c := range []struct {
		path     string
		ok       bool
		status   int
		why      string
		attempts int
	}{
		{"/ok", true, 200, "", 1},
		{"/feishu-ok", true, 200, "", 1},
		{"/dingtalk-error", false, 200, `errcode 310000: {"errcode":310000,"errmsg":"keywords not in content"}`, 1},
		{"/feishu-error", false, 200, `code 19021: {"code":19021,`, 1},
		{"/redirect", false, 302, "HTTP 302", 1},
		{"/missing", false, 404, "HTTP 404: no such hook", 1},
	} {
		e, res := send(c.path)
		d := e.Deliveries
		if len(d) != 1 || d[0].OK != c.ok || d[0].Status != c.status || !strings.HasPrefix(d[0].Error, c.why) || d[0].Target != "t" {
			t.Errorf("%s: deliveries %+v", c.path, d)
		}
		if res.ok != c.ok || res.retry {
			t.Errorf("%s: %+v", c.path, res)
		}
		// deliver and the single post.
		if n := count(c.path); n != 2*c.attempts {
			t.Errorf("%s: %d requests", c.path, n)
		}
	}
	if count("/ok") != 2 {
		t.Error("the redirect was followed")
	}

	// 503 and 429 are tried again.
	e, _ := send("/flaky")
	if d := e.Deliveries; len(d) != 1 || !d[0].OK || count("/flaky") != 4 {
		t.Errorf("flaky: %+v after %d requests", d, count("/flaky"))
	}
	// Three attempts, then the last error. The answer quotes the URL; the
	// result does not.
	e, res := send("/down/SECRETPATHtoken")
	if d := e.Deliveries; len(d) != 1 || d[0].OK || d[0].Status != 502 || down != 4 || !strings.HasPrefix(d[0].Error, "HTTP 502: upstream failed for /down/…") {
		t.Errorf("down: %+v after %d requests", d, down)
	}
	if !res.retry || strings.Contains(res.detail, "SECRET") {
		t.Errorf("down: %+v", res)
	}
	// At most 300 characters of an answer.
	if _, res := send("/big"); !res.ok || res.detail != strings.Repeat("y", 300) {
		t.Errorf("big: %d characters", len(res.detail))
	}

	// No answer: the error names neither the path nor the query.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	e, res = sendURL(gone.URL + "/hook/SECRETPATHtoken?key=SECRETQUERYtoken")
	if d := e.Deliveries; len(d) != 1 || d[0].OK || d[0].Status != 0 || strings.Contains(d[0].Error, "SECRET") || !res.retry {
		t.Errorf("refused: %+v %+v", d, res)
	}
}

// hookTargets saves the rule instanceDown with forSeconds 0 and the given
// targets.
func (h *harness) hookTargets(targets ...store.AlertTarget) {
	h.t.Helper()
	a := defaultAlertSettings()
	onlyRule(ruleInstanceDown, 0)(&a.Rules)
	a.Targets = targets
	if _, err := h.st.SetAlerts(a); err != nil {
		h.t.Fatal(err)
	}
}

// deliveries waits until entry seq has n deliveries and returns them by
// target.
func (h *harness) deliveries(tok string, seq uint64, n int) map[string]alertDelivery {
	h.t.Helper()
	var out map[string]alertDelivery
	waitFor(h.t, fmt.Sprintf("%d deliveries of entry %d", n, seq), func() bool {
		out = map[string]alertDelivery{}
		for _, e := range h.alerts(tok).History {
			if e.Seq == seq {
				for _, d := range e.Deliveries {
					out[d.Target] = d
				}
				return len(e.Deliveries) == n
			}
		}
		return false
	})
	return out
}

func TestHookHangingEndpoint(t *testing.T) {
	logs := logged(t)
	h := alertHarness(t)
	tok := h.login("admin", adminPW)
	h.srv.hookTimeout = 300 * time.Millisecond
	h.srv.hookRetry = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	release := make(chan struct{})
	slow := newHookServer(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	fast := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { io.WriteString(w, "ok") })
	h.hookTargets(
		store.AlertTarget{Name: "slow", Format: "slack", Language: "en", Enabled: true, URL: slow.URL + "/services/SECRETPATHtoken"},
		store.AlertTarget{Name: "fast", Format: "json", Language: "zh-CN", Enabled: true, URL: fast.URL + "/hook"},
		store.AlertTarget{Name: "off", Format: "json", Language: "en", Enabled: false, URL: fast.URL + "/off"},
	)
	f := h.fake(0)
	setConn(f, false, time.Now())
	start := time.Now()
	h.srv.evaluateAlerts(time.Now())
	setConn(f, true, time.Now())
	h.srv.evaluateAlerts(time.Now())
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("two evaluations took %v", d)
	}
	// The fast target has both messages while the slow one still hangs.
	got := h.deliveries(tok, 2, 1)
	if d := got["fast"]; !d.OK || d.Status != 200 {
		t.Fatalf("fast %+v", got)
	}
	if r := fast.got(); len(r) != 2 || r[0].path != "/hook" || !strings.Contains(r[0].body, `"event":"firing"`) || !strings.Contains(r[1].body, `"event":"resolved"`) {
		t.Fatalf("fast got %+v", r)
	}
	if len(slow.got()) == 0 {
		t.Fatal("the slow target got nothing yet")
	}
	// Each message to the slow one fails after three attempts.
	got = h.deliveries(tok, 1, 2)
	if d := got["slow"]; d.OK || d.Status != 0 || d.Error != "no answer within 300ms" {
		t.Fatalf("slow %+v", got)
	}
	h.deliveries(tok, 2, 2)
	if n := len(slow.got()); n != 6 {
		t.Fatalf("slow got %d requests", n)
	}
	out := logs()
	if !strings.Contains(out, `alerts: sending to target "slow" (127.0.0.1:`) || strings.Contains(out, "SECRET") || strings.Contains(out, "/services/") {
		t.Fatalf("log:\n%s", out)
	}
}

func TestHookQueueOverflowAndRemoval(t *testing.T) {
	logs := logged(t)
	h := alertHarness(t)
	tok := h.login("admin", adminPW)
	h.srv.queueSize = 2
	inFlight := make(chan struct{}, 10)
	slow := newHookServer(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
		inFlight <- struct{}{}
		<-r.Context().Done()
	})
	h.hookTargets(store.AlertTarget{Name: "slow", Format: "json", Language: "en", Enabled: true, URL: slow.URL + "/hook/SECRETPATHtoken"})
	f := h.fake(0)
	for i := range 6 {
		setConn(f, i%2 == 1, time.Now())
		h.srv.evaluateAlerts(time.Now())
		if i == 0 {
			<-inFlight
		}
	}
	// 1 in flight; 2, 3 and 4 dropped for 5 and 6.
	for seq := uint64(2); seq <= 4; seq++ {
		if d := h.deliveries(tok, seq, 1)["slow"]; d.OK || d.Error != "not sent: the queue of the target was full" {
			t.Fatalf("entry %d: %+v", seq, d)
		}
	}
	// Removing the target cancels the request in flight and what is queued.
	h.hookTargets()
	h.srv.evaluateAlerts(time.Now())
	for _, seq := range []uint64{1, 5, 6} {
		if d := h.deliveries(tok, seq, 1)["slow"]; d.OK || d.Error != errTargetStopped {
			t.Fatalf("entry %d: %+v", seq, d)
		}
	}
	if n := len(slow.got()); n != 1 {
		t.Fatalf("%d requests", n)
	}
	if out := logs(); strings.Count(out, `the queue of target "slow" (127.0.0.1:`) != 3 || strings.Contains(out, "SECRET") {
		t.Fatalf("log:\n%s", out)
	}
}

func TestHookEndToEndAndTestRoute(t *testing.T) {
	var auditPath string
	h := alertHarness(t, withAudit(t, &auditPath))
	tok := h.login("admin", adminPW)
	hs := newHookServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.HasSuffix(r.URL.Path, "/bad") {
			w.WriteHeader(400)
			io.WriteString(w, `{"errcode":93000,"errmsg":"invalid webhook url"}`)
			return
		}
		io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	})
	r := h.do("PUT", "/api/alerts/settings", tok, `{"targets":[{"name":"wecom","format":"wecom","language":"zh-CN","enabled":false,"url":"`+hs.URL+`/cgi-bin/webhook/send?key=SECRETQUERYtoken"},{"name":"bad","format":"json","language":"en","enabled":true,"url":"`+hs.URL+`/bad"}]}`)
	expect(t, r, 200, "")
	var set struct{ Targets []alertTargetView }
	r.json(t, &set)
	// A disabled target can be tested.
	expect(t, h.do("POST", "/api/alerts/test", tok, `{"id":"`+set.Targets[0].ID+`"}`), 200, `{"ok":true,"status":200,"detail":"{\"errcode\":0,\"errmsg\":\"ok\"}"}`)
	if g := hs.got(); len(g) != 1 || g[0].body != `{"msgtype":"text","text":{"content":"[FreeSWITCH Console] 测试通知。能看到这条消息，说明 Webhook 配置正确。"}}` {
		t.Fatalf("got %+v", g)
	}
	expect(t, h.do("POST", "/api/alerts/test", tok, `{"id":"`+set.Targets[1].ID+`"}`), 200, `{"ok":false,"status":400,"detail":"{\"errcode\":93000,\"errmsg\":\"invalid webhook url\"}"}`)

	// An instance that drops is delivered to the enabled target only.
	setConn(h.fake(0), false, time.Now())
	h.srv.evaluateAlerts(time.Now())
	if a := h.alerts(tok); len(a.Active) != 0 {
		t.Fatalf("fired before 60s: %+v", a.Active)
	}
	h.srv.evaluateAlerts(time.Now().Add(time.Minute))
	got := h.deliveries(tok, 1, 1)
	if d := got["bad"]; d.OK || d.Status != 400 || d.Error != `HTTP 400: {"errcode":93000,"errmsg":"invalid webhook url"}` {
		t.Fatalf("deliveries %+v", got)
	}

	h.srv.cfg.Audit.Close()
	data := string(must(t, auditPath))
	if strings.Contains(data, "SECRET") || strings.Contains(data, "cgi-bin") {
		t.Fatalf("audit:\n%s", data)
	}
	var tests []string
	for _, e := range readAudit(t, auditPath) {
		if e.Action == "alerts.test" {
			tests = append(tests, e.Target+" "+e.Detail+" "+e.Result+" "+e.Error)
		}
	}
	host := strings.TrimPrefix(hs.URL, "http://")
	want := []string{"wecom format=wecom host=" + host + " ok ", "bad format=json host=" + host + ` error HTTP 400: {"errcode":93000,"errmsg":"invalid webhook url"}`}
	if strings.Join(tests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit\n got %q\nwant %q", tests, want)
	}
}
