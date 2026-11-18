package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const metricsToken = "metrics-token-0123456789"

func withMetricsToken(c *Config) { c.MetricsToken = metricsToken }

// scrape gets /metrics with the Authorization header auth ("" for none).
func (h *harness) scrape(auth string) response {
	h.t.Helper()
	req, err := http.NewRequest("GET", h.ts.URL+"/metrics", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{code: resp.StatusCode, body: string(b), header: resp.Header, resp: resp}
}

func TestMetricsAuth(t *testing.T) {
	h := newHarness(t)
	expect(t, h.scrape("Bearer "+metricsToken), 404, `{"error":"not found"}`)

	h = newHarness(t, withMetricsToken)
	for _, auth := range []string{"", "Bearer", "Bearer ", "Bearer metrics-token-012345678", "Bearer " + metricsToken + "x", "bearer " + metricsToken, "Basic " + metricsToken, metricsToken} {
		expect(t, h.scrape(auth), 401, `{"error":"a valid bearer token is required"}`)
	}
	// A session is not enough.
	r := h.do("GET", "/metrics", h.login("admin", adminPW), "")
	expect(t, r, 401, "")
	r = h.scrape("Bearer " + metricsToken)
	expect(t, r, 200, "")
	if ct := r.header.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" || !strings.Contains(r.body, `fsconsole_instance_up{instance="fs01"} 1`) {
		t.Fatalf("%s\n%s", ct, r.body)
	}

	for _, token := range []string{"x", "fifteen-chars-x"} {
		if _, err := New(Config{Store: h.st, Dial: h.dial, Static: h.srv.cfg.Static, MetricsToken: token}); err == nil || !strings.Contains(err.Error(), "at least 16 characters") {
			t.Errorf("token %q: %v", token, err)
		}
	}
}

// TestMetricsText checks the whole text for a small fixture: two instances,
// one connected with a heartbeat, hang-ups and SIP events, one disconnected
// without; and the escaping of label values.
func TestMetricsText(t *testing.T) {
	h := newHarness(t, withMetricsToken, withRecentCalls(10), withSipEvents(10))
	admin := h.login("admin", adminPW)
	body, _ := json.Marshal(map[string]any{"name": "edge \"2\"\\x\ny", "host": "h", "port": 1, "password": "p"})
	expect(t, h.do("POST", "/api/instances", admin, string(body)), 201, "")
	h.fake(1).set(func(f *fakeSwitch) { f.state.Connected = false })
	h.srv.cfg.Version = "1.2 \"rc\"\\x\ny"

	f := h.fake(0)
	f.publish(hangupEvent("c1"))
	f.publish(hangupEvent("c2"))
	f.publish(hangupEvent("c3", "Hangup-Cause", "NORMAL_CLEARING"))
	f.publish(evFailure)
	f.publish(evFailure)
	f.publish(evGatewayState)
	inst := h.instance(h.id)
	waitFor(t, "records", func() bool { return len(inst.hangups(10)) == 3 && len(inst.sipRows(10, nil)) == 3 })
	at := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	inst.setBeat(sample{Sessions: 3, Rate: 2, Idle: 97.25}, 1000, at)

	var b strings.Builder
	h.srv.writeMetrics(&b, at.Add(4500*time.Millisecond))
	edge := `instance="edge \"2\"\\x\ny"`
	var sip strings.Builder
	for _, in := range []struct {
		label string
		n     map[string]int
	}{{edge, nil}, {`instance="fs01"`, map[string]int{"gateway_state": 1, "register_failure": 2}}} {
		for _, kind := range []string{"expire", "gateway_add", "gateway_delete", "gateway_state", "register", "register_failure", "unregister"} {
			fmt.Fprintf(&sip, "fsconsole_sip_events_total{%s,kind=%q} %d\n", in.label, kind, in.n[kind])
		}
	}
	want := `# HELP fsconsole_build_info The version of the console.
# TYPE fsconsole_build_info gauge
fsconsole_build_info{version="1.2 \"rc\"\\x\ny"} 1
# HELP fsconsole_instance_up 1 when the Event Socket of the instance is connected.
# TYPE fsconsole_instance_up gauge
fsconsole_instance_up{` + edge + `} 0
fsconsole_instance_up{instance="fs01"} 1
# HELP fsconsole_sessions Channels in progress at the last heartbeat.
# TYPE fsconsole_sessions gauge
fsconsole_sessions{instance="fs01"} 3
# HELP fsconsole_sessions_per_second Sessions created in the second before the last heartbeat.
# TYPE fsconsole_sessions_per_second gauge
fsconsole_sessions_per_second{instance="fs01"} 2
# HELP fsconsole_idle_cpu_percent Idle CPU at the last heartbeat, percent.
# TYPE fsconsole_idle_cpu_percent gauge
fsconsole_idle_cpu_percent{instance="fs01"} 97.25
# HELP fsconsole_session_limit Max-Sessions at the last heartbeat.
# TYPE fsconsole_session_limit gauge
fsconsole_session_limit{instance="fs01"} 1000
# HELP fsconsole_heartbeat_age_seconds Seconds since the last heartbeat was received.
# TYPE fsconsole_heartbeat_age_seconds gauge
fsconsole_heartbeat_age_seconds{instance="fs01"} 4.500
# HELP fsconsole_hangups_total Hang-ups received since the console started, by cause.
# TYPE fsconsole_hangups_total counter
fsconsole_hangups_total{instance="fs01",cause="NORMAL_CLEARING"} 1
fsconsole_hangups_total{instance="fs01",cause="USER_BUSY"} 2
# HELP fsconsole_sip_events_total SIP events received since the console started, by kind.
# TYPE fsconsole_sip_events_total counter
` + sip.String()
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
	// The same state gives the same text.
	for range 10 {
		var again strings.Builder
		h.srv.writeMetrics(&again, at.Add(4500*time.Millisecond))
		if again.String() != want {
			t.Fatalf("not deterministic:\n%s", again.String())
		}
	}

	for in, want := range map[string]string{
		`a\b`: `"a\\b"`, `say "hi"`: `"say \"hi\""`, "two\nlines": `"two\nlines"`, `\"` + "\n": `"\\\"\n"`, "plain": `"plain"`,
	} {
		if got := label(in); got != want {
			t.Errorf("label(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestMetricsSharedName: instances that share a name carry their id in the
// instance label, so no two series have the same labels.
func TestMetricsSharedName(t *testing.T) {
	h := newHarness(t, withMetricsToken, withRecentCalls(10), withSipEvents(10))
	admin := h.login("admin", adminPW)
	var ids []string
	for range 2 {
		r := h.do("POST", "/api/instances", admin, `{"name":"edge","host":"h","port":1,"password":"p"}`)
		expect(t, r, 201, "")
		var created instanceView
		r.json(t, &created)
		ids = append(ids, created.ID)
	}
	at := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	for _, id := range append([]string{h.id}, ids...) {
		h.instance(id).setBeat(sample{Sessions: 1}, 10, at)
	}

	var b strings.Builder
	h.srv.writeMetrics(&b, at)
	body := b.String()
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if seen[line] {
			t.Errorf("line twice: %s", line)
		}
		seen[line] = true
	}
	for _, line := range []string{`fsconsole_instance_up{instance="fs01"} 1`, `fsconsole_sessions{instance="edge (` + ids[0] + `)"} 1`, `fsconsole_sessions{instance="edge (` + ids[1] + `)"} 1`} {
		if !strings.Contains(body, line+"\n") {
			t.Errorf("no %s in\n%s", line, body)
		}
	}
	if strings.Contains(body, `instance="edge"`) {
		t.Errorf("shared name without id:\n%s", body)
	}
}

// TestMetricsHeartbeat: the heartbeat metrics come from the last HEARTBEAT,
// and are left out before the first.
func TestMetricsHeartbeat(t *testing.T) {
	h := newHarness(t, withMetricsToken)
	if body := h.scrape("Bearer " + metricsToken).body; strings.Contains(body, "fsconsole_sessions{") || strings.Contains(body, "fsconsole_heartbeat_age_seconds{") {
		t.Fatalf("heartbeat metrics before a heartbeat:\n%s", body)
	}
	hb := heartbeatWith("7", "3", "95.5")
	hb.Headers["Max-Sessions"] = "250"
	h.fake(0).publish(hb)
	inst := h.instance(h.id)
	waitFor(t, "heartbeat", func() bool { return len(inst.samples()) == 1 })
	body := h.scrape("Bearer " + metricsToken).body
	for _, line := range []string{`fsconsole_sessions{instance="fs01"} 7`, `fsconsole_sessions_per_second{instance="fs01"} 3`, `fsconsole_idle_cpu_percent{instance="fs01"} 95.5`, `fsconsole_session_limit{instance="fs01"} 250`, `fsconsole_heartbeat_age_seconds{instance="fs01"} 0.`} {
		if !strings.Contains(body, line) {
			t.Errorf("no %s in\n%s", line, body)
		}
	}
	// Without SIP events or recent calls there is no counter of them.
	if strings.Contains(body, "fsconsole_sip_events_total{") || strings.Contains(body, "fsconsole_hangups_total{") {
		t.Errorf("counters while off:\n%s", body)
	}
}

// TestMetricsCounters: the counters keep counting after the records rotate
// out of their rings, and the causes are bounded.
func TestMetricsCounters(t *testing.T) {
	h := newHarness(t, withMetricsToken, withRecentCalls(2), withSipEvents(2))
	f := h.fake(0)
	inst := h.instance(h.id)
	for n := 1; n <= 5; n++ {
		f.publish(hangupEvent(fmt.Sprint("c", n)))
		f.publish(evFailure)
	}
	waitFor(t, "records", func() bool {
		r := inst.sipRows(2, nil)
		return len(r) == 2 && r[0].Seq == 5 && len(inst.hangups(2)) == 2
	})
	waitFor(t, "last hang-up", func() bool { return inst.hangups(1)[0].UUID == "c5" })
	body := h.scrape("Bearer " + metricsToken).body
	for _, line := range []string{`fsconsole_hangups_total{instance="fs01",cause="USER_BUSY"} 5`, `fsconsole_sip_events_total{instance="fs01",kind="register_failure"} 5`} {
		if !strings.Contains(body, line+"\n") {
			t.Errorf("no %s in\n%s", line, body)
		}
	}

	for n := 0; n < maxCauses+5; n++ {
		inst.addHangup(hangupEvent(fmt.Sprint("x", n), "Hangup-Cause", fmt.Sprintf("CAUSE_%03d", n)).Headers)
	}
	body = h.scrape("Bearer " + metricsToken).body
	if n := strings.Count(body, "fsconsole_hangups_total{"); n != maxCauses+1 {
		t.Fatalf("%d cause series", n)
	}
	// USER_BUSY came first, so CAUSE_000 to CAUSE_098 fit and the rest are other.
	for _, line := range []string{`cause="USER_BUSY"} 5`, `cause="CAUSE_098"} 1`, `cause="other"} 6`} {
		if !strings.Contains(body, line+"\n") {
			t.Errorf("no %s in\n%s", line, body)
		}
	}
	if strings.Contains(body, "CAUSE_099") {
		t.Error("CAUSE_099 counted on its own")
	}
	// A known cause still counts on its own once the bound is reached.
	inst.addHangup(hangupEvent("y", "Hangup-Cause", "CAUSE_000").Headers)
	if body = h.scrape("Bearer " + metricsToken).body; !strings.Contains(body, `cause="CAUSE_000"} 2`+"\n") {
		t.Errorf("CAUSE_000 not counted:\n%s", body)
	}
}
