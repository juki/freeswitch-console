package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juki/freeswitch-console/internal/esl"
	"github.com/juki/freeswitch-console/internal/store"
)

// alertHarness evaluates by hand: no evaluation loop runs.
func alertHarness(t *testing.T, opts ...func(*Config)) *harness {
	t.Helper()
	return newHarness(t, append([]func(*Config){func(c *Config) { c.AlertInterval = -1 }}, opts...)...)
}

type alertsReply struct {
	Active  []alertView
	History []alertEntry
}

func (h *harness) alerts(token string) alertsReply {
	h.t.Helper()
	var a alertsReply
	r := h.do("GET", "/api/alerts", token, "")
	expect(h.t, r, 200, "")
	r.json(h.t, &a)
	return a
}

// setRules saves the default settings changed by edit, without targets.
func (h *harness) setRules(edit func(r *store.AlertRules)) {
	h.t.Helper()
	a := defaultAlertSettings()
	edit(&a.Rules)
	if _, err := h.st.SetAlerts(a); err != nil {
		h.t.Fatal(err)
	}
}

// onlyRule turns every rule but key off and sets its forSeconds.
func onlyRule(key string, forSeconds int) func(r *store.AlertRules) {
	return func(r *store.AlertRules) {
		for k, rule := range map[string]*store.AlertRule{ruleInstanceDown: &r.InstanceDown, ruleGatewayDown: &r.GatewayDown, ruleIdleCPULow: &r.IdleCPULow.AlertRule, ruleSessionsHigh: &r.SessionsHigh.AlertRule} {
			rule.Enabled, rule.ForSeconds = k == key, forSeconds
		}
	}
}

func setConn(f *fakeSwitch, up bool, since time.Time) {
	f.set(func(f *fakeSwitch) {
		f.state = esl.State{Connected: up, Since: since}
		if !up {
			f.state.LastError = "dial tcp 10.0.0.5:8021: connect: connection refused"
		}
	})
}

func (h *harness) instanceAlerts(token string) int {
	h.t.Helper()
	var list []struct{ State struct{ Alerts int } }
	h.do("GET", "/api/instances", token, "").json(h.t, &list)
	return list[0].State.Alerts
}

func TestAlertDebounceInstanceDown(t *testing.T) {
	h := alertHarness(t)
	tok := h.login("admin", adminPW)
	h.setRules(onlyRule(ruleInstanceDown, 60))
	f := h.fake(0)
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	ev := func(d time.Duration) { h.srv.evaluateAlerts(t0.Add(d)) }

	setConn(f, false, t0)
	for _, d := range []time.Duration{0, 30 * time.Second, 59 * time.Second} {
		if ev(d); len(h.alerts(tok).Active) != 0 {
			t.Fatalf("fired after %v", d)
		}
	}
	ev(60 * time.Second)
	a := h.alerts(tok)
	if len(a.Active) != 1 || len(a.History) != 1 {
		t.Fatalf("after 60s: %+v", a)
	}
	al := a.Active[0]
	if al.Rule != ruleInstanceDown || al.InstanceID != h.id || al.Instance != "fs01" || al.Since != t0.UnixMilli() ||
		al.FiredAt != t0.Add(time.Minute).UnixMilli() || !strings.Contains(al.Reason, "connection refused") || al.ID != "1" {
		t.Fatalf("alert %+v", al)
	}
	if e := a.History[0]; e.Event != "firing" || e.Seq != 1 || e.alertView != al || e.Deliveries == nil || len(e.Deliveries) != 0 {
		t.Fatalf("entry %+v", e)
	}
	if n := h.instanceAlerts(tok); n != 1 {
		t.Fatalf("instance alerts %d", n)
	}

	// Back for 40s, down again: the resolution starts over.
	t1 := t0.Add(100 * time.Second)
	setConn(f, true, t1)
	ev(130 * time.Second)
	setConn(f, false, t1.Add(40*time.Second))
	ev(145 * time.Second)
	t2 := t1.Add(50 * time.Second)
	setConn(f, true, t2)
	ev(150 * time.Second)
	ev(150*time.Second + 59*time.Second)
	if a := h.alerts(tok); len(a.Active) != 1 || len(a.History) != 1 {
		t.Fatalf("resolved early: %+v", a)
	}
	ev(150*time.Second + 60*time.Second)
	a = h.alerts(tok)
	if len(a.Active) != 0 || len(a.History) != 2 {
		t.Fatalf("not resolved: %+v", a)
	}
	if e := a.History[0]; e.Event != "resolved" || e.Seq != 2 || e.ID != "1" || e.Since != t0.UnixMilli() || e.DurationMs != t2.Sub(t0).Milliseconds() || e.Reason != "" {
		t.Fatalf("resolved %+v", e)
	}
	if n := h.instanceAlerts(tok); n != 0 {
		t.Fatalf("instance alerts %d", n)
	}
}

// heartbeatOf publishes a heartbeat and waits until it is the instance's
// last.
func (h *harness) heartbeatOf(idle string, sessions, limit int) {
	h.t.Helper()
	f := h.fake(0)
	f.publish(esl.Message{Kind: esl.KindEvent, Headers: map[string]string{"Event-Name": "HEARTBEAT", "Idle-CPU": idle,
		"Session-Count": fmt.Sprint(sessions), "Max-Sessions": fmt.Sprint(limit)}})
	inst := h.instance(h.id)
	waitFor(h.t, "heartbeat", func() bool {
		inst.histMu.Lock()
		defer inst.histMu.Unlock()
		return fmt.Sprint(inst.beat.Idle) == idle && inst.beat.Sessions == sessions
	})
}

func TestAlertHeartbeatRules(t *testing.T) {
	h := alertHarness(t)
	tok := h.login("admin", adminPW)
	h.setRules(func(r *store.AlertRules) { r.InstanceDown.Enabled, r.GatewayDown.Enabled = false, false })

	h.heartbeatOf("5.25", 950, 1000)
	base := time.Now()
	h.srv.evaluateAlerts(base)
	// An interruption: the next heartbeat is fine, then low again.
	h.heartbeatOf("50", 10, 1000)
	h.srv.evaluateAlerts(base.Add(30 * time.Second))
	h.heartbeatOf("5.25", 950, 1000)
	base = time.Now()
	h.srv.evaluateAlerts(base)
	if h.srv.evaluateAlerts(base.Add(59 * time.Second)); len(h.alerts(tok).Active) != 0 {
		t.Fatal("fired before 60s")
	}
	h.srv.evaluateAlerts(base.Add(60 * time.Second))
	a := h.alerts(tok)
	if len(a.Active) != 2 {
		t.Fatalf("active %+v", a.Active)
	}
	by := map[string]alertView{}
	for _, al := range a.Active {
		by[al.Rule] = al
	}
	if v := by[ruleIdleCPULow]; v.Value != 5.3 || v.Threshold != 10 || v.Sessions != 0 {
		t.Fatalf("idle %+v", v)
	}
	if v := by[ruleSessionsHigh]; v.Value != 95 || v.Threshold != 90 || v.Sessions != 950 || v.Limit != 1000 {
		t.Fatalf("sessions %+v", v)
	}

	h.heartbeatOf("50", 100, 1000)
	base = time.Now()
	h.srv.evaluateAlerts(base)
	h.srv.evaluateAlerts(base.Add(60 * time.Second))
	a = h.alerts(tok)
	if len(a.Active) != 0 || len(a.History) != 4 {
		t.Fatalf("not resolved %+v", a)
	}
	for _, e := range a.History[:2] {
		if e.Event != "resolved" || (e.Rule == ruleIdleCPULow && e.Value != 50) || (e.Rule == ruleSessionsHigh && (e.Value != 10 || e.Sessions != 100)) {
			t.Fatalf("resolved %+v", e)
		}
	}

	// A limit of 0 is no condition.
	h.heartbeatOf("50", 5000, 0)
	h.srv.evaluateAlerts(time.Now().Add(time.Hour))
	if a := h.alerts(tok); len(a.Active) != 0 {
		t.Fatalf("limit 0 fired %+v", a.Active)
	}
}

func TestClassifyGateway(t *testing.T) {
	for _, c := range []struct {
		state, ping string
		bad, known  bool
	}{
		{"REGED", "UP", false, true}, {"NOREG", "UP", false, true}, {"REGED", "", false, true},
		{"REGED", "DOWN", true, true}, {"NOREG", "DOWN", true, true},
		{"FAIL_WAIT", "UP", true, true}, {"FAILED", "DOWN", true, true}, {"EXPIRED", "UP", true, true}, {"TIMEOUT", "UP", true, true}, {"DOWN", "UP", true, true},
		{"TRYING", "UP", false, false}, {"REGISTER", "UP", false, false}, {"UNREGED", "UP", false, false}, {"UNREGISTER", "UP", false, false},
		{"TRYING", "DOWN", true, true},
	} {
		if bad, known := classifyGateway(c.state, c.ping); bad != c.bad || known != c.known {
			t.Errorf("%s/%s: %v %v", c.state, c.ping, bad, known)
		}
	}
}

func gwEvent(subclass, name, state, ping string) esl.Message {
	h := map[string]string{"Event-Name": "CUSTOM", "Event-Subclass": subclass, "Gateway": name}
	if state != "" {
		h["State"], h["Ping-Status"] = state, ping
	}
	return esl.Message{Kind: esl.KindEvent, Headers: h}
}

// gateway returns the state, ping and classification of a gateway.
func (h *harness) gateway(name string) (state, ping string, bad, ok bool) {
	inst := h.instance(h.id)
	inst.gwMu.Lock()
	defer inst.gwMu.Unlock()
	g := inst.gateways[name]
	if g == nil || g.gone {
		return "", "", false, false
	}
	return g.state, g.ping, g.bad, true
}

func (h *harness) seeded() bool {
	inst := h.instance(h.id)
	st := inst.sw.State()
	inst.gwMu.Lock()
	defer inst.gwMu.Unlock()
	return st.Connected && inst.gwSeeded.Equal(st.Since)
}

func TestAlertGateways(t *testing.T) {
	h := alertHarness(t, withSipEvents(100))
	tok := h.login("admin", adminPW)
	h.setRules(onlyRule(ruleGatewayDown, 60))
	f := h.fake(0)
	waitFor(t, "seed", h.seeded)
	if st, _, bad, ok := h.gateway("gw1"); !ok || st != "REGED" || bad {
		t.Fatalf("seeded gw1 %q %v %v", st, bad, ok)
	}
	step := func(m esl.Message, name, state string, bad bool) {
		t.Helper()
		f.publish(m)
		waitFor(t, name+" "+state, func() bool {
			st, _, b, ok := h.gateway(name)
			return ok == (state != "-") && st == strings.TrimPrefix(state, "-") && b == bad
		})
	}
	step(gwEvent("sofia::gateway_state", "gw1", "FAIL_WAIT", "DOWN"), "gw1", "FAIL_WAIT", true)
	step(gwEvent("sofia::gateway_state", "gw1", "TRYING", "DOWN"), "gw1", "TRYING", true)
	step(gwEvent("sofia::gateway_state", "gw1", "REGED", "UP"), "gw1", "REGED", false)
	step(gwEvent("sofia::gateway_state", "gw1", "UNREGED", "UP"), "gw1", "UNREGED", false)
	step(gwEvent("sofia::gateway_state", "gw1", "TRYING", "UP"), "gw1", "TRYING", false)
	step(gwEvent("sofia::gateway_state", "gw1", "FAIL_WAIT", "DOWN"), "gw1", "FAIL_WAIT", true)
	step(gwEvent("sofia::gateway_state", "gw1", "TRYING", "UP"), "gw1", "TRYING", true)
	step(gwEvent("sofia::gateway_add", "gw2", "", ""), "gw2", "", false)

	now := time.Now()
	h.srv.evaluateAlerts(now)
	h.srv.evaluateAlerts(now.Add(time.Minute))
	a := h.alerts(tok)
	if len(a.Active) != 1 || a.Active[0].Subject != "gw1" || a.Active[0].State != "TRYING" || a.Active[0].Ping != "UP" {
		t.Fatalf("active %+v", a.Active)
	}
	// Deleted: the alert goes without a resolution.
	step(gwEvent("sofia::gateway_delete", "gw1", "", ""), "gw1", "-", false)
	h.srv.evaluateAlerts(now.Add(2 * time.Minute))
	if a := h.alerts(tok); len(a.Active) != 0 || len(a.History) != 1 {
		t.Fatalf("after delete %+v", a)
	}

	// An event that arrives while the seed is in flight wins over its answer.
	hold := make(chan struct{})
	f.set(func(f *fakeSwitch) { f.hold = hold })
	h.instance(h.id).seedGateways()
	waitFor(t, "seed sent", func() bool {
		sent, _, _, _ := f.snapshot()
		return strings.Count(strings.Join(sent, "\n"), "sofia xmlstatus gateway") == 2
	})
	step(gwEvent("sofia::gateway_state", "gw1", "FAIL_WAIT", "DOWN"), "gw1", "FAIL_WAIT", true)
	close(hold)
	h.instance(h.id).seeding.Wait()
	if st, _, bad, _ := h.gateway("gw1"); st != "FAIL_WAIT" || !bad {
		t.Fatalf("the seed overwrote the event: %s %v", st, bad)
	}
	// gw2, added before the seed and not listed by it, is gone.
	if _, _, _, ok := h.gateway("gw2"); ok {
		t.Fatal("gw2 kept")
	}
	// A gateway deleted while the seed is in flight stays deleted.
	hold = make(chan struct{})
	f.set(func(f *fakeSwitch) { f.hold = hold })
	h.instance(h.id).seedGateways()
	waitFor(t, "third seed sent", func() bool {
		sent, _, _, _ := f.snapshot()
		return strings.Count(strings.Join(sent, "\n"), "sofia xmlstatus gateway") == 3
	})
	step(gwEvent("sofia::gateway_delete", "gw1", "", ""), "gw1", "-", false)
	close(hold)
	h.instance(h.id).seeding.Wait()
	if _, _, _, ok := h.gateway("gw1"); ok {
		t.Fatal("the seed brought gw1 back")
	}
}

func TestAlertFrozenWhileDisconnected(t *testing.T) {
	h := alertHarness(t, withSipEvents(100))
	tok := h.login("admin", adminPW)
	h.setRules(func(r *store.AlertRules) { r.InstanceDown.Enabled, r.SessionsHigh.Enabled = false, false })
	f := h.fake(0)
	waitFor(t, "seed", h.seeded)
	f.publish(gwEvent("sofia::gateway_state", "gw1", "FAIL_WAIT", "DOWN"))
	h.heartbeatOf("5", 0, 1000)
	now := time.Now()
	h.srv.evaluateAlerts(now)
	h.srv.evaluateAlerts(now.Add(time.Minute))
	if a := h.alerts(tok); len(a.Active) != 2 {
		t.Fatalf("active %+v", a.Active)
	}
	// Good again, a new gateway bad, and the connection drops before any of
	// it is evaluated: nothing fires or resolves while it is down.
	f.publish(gwEvent("sofia::gateway_state", "gw1", "REGED", "UP"))
	f.publish(gwEvent("sofia::gateway_state", "gw2", "FAILED", "DOWN"))
	h.heartbeatOf("80", 0, 1000)
	setConn(f, false, time.Now())
	h.srv.evaluateAlerts(now.Add(time.Hour))
	a := h.alerts(tok)
	if len(a.Active) != 2 || len(a.History) != 2 || a.Active[0].Subject+a.Active[1].Subject != "gw1" {
		t.Fatalf("while disconnected %+v", a)
	}

	// Connected again: the gateways count from the seed, the CPU from the
	// first heartbeat after the reconnect.
	back := time.Now()
	setConn(f, true, back)
	f.publish(esl.Message{Kind: esl.KindState, Headers: map[string]string{"connected": "true"}})
	waitFor(t, "seed after the reconnect", h.seeded)
	h.srv.evaluateAlerts(back.Add(2 * time.Minute))
	a = h.alerts(tok)
	if len(a.Active) != 1 || a.Active[0].Rule != ruleIdleCPULow || a.History[0].Event != "resolved" || a.History[0].Subject != "gw1" {
		t.Fatalf("after the reconnect %+v", a)
	}
	h.heartbeatOf("81", 0, 1000)
	beat := time.Now()
	h.srv.evaluateAlerts(beat)
	h.srv.evaluateAlerts(beat.Add(time.Minute))
	if a := h.alerts(tok); len(a.Active) != 0 || len(a.History) != 4 {
		t.Fatalf("after a heartbeat %+v", a)
	}
}

func TestAlertsDroppedSilently(t *testing.T) {
	h := alertHarness(t)
	tok := h.login("admin", adminPW)
	h.setRules(onlyRule(ruleInstanceDown, 0))
	setConn(h.fake(0), false, time.Now())
	h.srv.evaluateAlerts(time.Now())
	if a := h.alerts(tok); len(a.Active) != 1 {
		t.Fatalf("active %+v", a)
	}
	// Switched off.
	h.setRules(func(r *store.AlertRules) { r.InstanceDown.Enabled = false })
	h.srv.evaluateAlerts(time.Now())
	if a := h.alerts(tok); len(a.Active) != 0 || len(a.History) != 1 {
		t.Fatalf("rule off %+v", a)
	}
	// Deleted instance.
	h.setRules(onlyRule(ruleInstanceDown, 0))
	h.srv.evaluateAlerts(time.Now())
	if a := h.alerts(tok); len(a.Active) != 1 || len(a.History) != 2 {
		t.Fatalf("again %+v", a)
	}
	expect(t, h.do("DELETE", "/api/instances/"+h.id, tok, ""), 204, "")
	h.srv.evaluateAlerts(time.Now())
	if a := h.alerts(tok); len(a.Active) != 0 || len(a.History) != 2 {
		t.Fatalf("instance deleted %+v", a)
	}
}

func TestAlertHistoryBound(t *testing.T) {
	h := alertHarness(t)
	tok := h.login("admin", adminPW)
	h.setRules(onlyRule(ruleInstanceDown, 0))
	f := h.fake(0)
	now := time.Now()
	for i := range 250 {
		setConn(f, i%2 == 1, now.Add(time.Duration(i)*time.Second))
		h.srv.evaluateAlerts(now.Add(time.Duration(i) * time.Second))
	}
	a := h.alerts(tok)
	if len(a.History) != alertHistorySize || a.History[0].Seq != 250 || a.History[199].Seq != 51 {
		t.Fatalf("history %d entries, %d .. %d", len(a.History), a.History[0].Seq, a.History[len(a.History)-1].Seq)
	}
}

func TestAlertSettingsAPI(t *testing.T) {
	var auditPath string
	h := alertHarness(t, withAudit(t, &auditPath), withSipEvents(100))
	tok := h.login("admin", adminPW)
	expect(t, h.do("GET", "/api/alerts/settings", tok, ""), 200,
		`{"gatewayEvents":true,"rules":{"instanceDown":{"enabled":true,"forSeconds":60},"gatewayDown":{"enabled":true,"forSeconds":60},"idleCpuLow":{"enabled":true,"forSeconds":60,"below":10},"sessionsHigh":{"enabled":true,"forSeconds":60,"above":90}},"targets":[]}`)

	// Roles: a viewer reads the alerts, not the settings.
	vt := h.login("viewer", viewerPW)
	expect(t, h.do("GET", "/api/alerts", vt, ""), 200, `{"active":[],"history":[]}`)
	expect(t, h.do("GET", "/api/alerts/settings", vt, ""), 403, "")
	expect(t, h.do("PUT", "/api/alerts/settings", vt, `{"targets":[]}`), 403, "")
	expect(t, h.do("POST", "/api/alerts/test", vt, `{"id":"x"}`), 403, "")

	const secret = "https://hooks.example.com:8443/services/T0001/B0002/SECRETPATHtoken?access_token=SECRETQUERYtoken"
	target := func(id, name, format, url string) string {
		return fmt.Sprintf(`{"id":%q,"name":%q,"format":%q,"language":"en","enabled":true,"url":%q}`, id, name, format, url)
	}
	for _, c := range []struct{ body, msg string }{
		{`{"rules":{"instanceDown":{"enabled":true,"forSeconds":-1}}}`, "forSeconds of instanceDown must be 0 to 86400"},
		{`{"rules":{"gatewayDown":{"enabled":true,"forSeconds":86401}}}`, "forSeconds of gatewayDown must be 0 to 86400"},
		{`{"rules":{"idleCpuLow":{"enabled":true,"forSeconds":60,"below":0}}}`, "below of idleCpuLow must be 1 to 99"},
		{`{"rules":{"idleCpuLow":{"enabled":false,"forSeconds":60,"below":100}}}`, "below of idleCpuLow must be 1 to 99"},
		{`{"rules":{"sessionsHigh":{"enabled":true,"forSeconds":60,"above":101}}}`, "above of sessionsHigh must be 1 to 100"},
		{`{"rules":{"sessionsHigh":{"enabled":true,"forSeconds":60,"above":0}}}`, "above of sessionsHigh must be 1 to 100"},
		{`{"targets":[` + strings.Repeat(target("", "a", "json", secret)+",", 5) + target("", "b", "json", secret) + `]}`, "at most 5 targets"},
		{`{"targets":[` + target("", "", "json", secret) + `]}`, "the name of target 1 must be 1 to 40 characters"},
		{`{"targets":[` + target("", strings.Repeat("名", 41), "json", secret) + `]}`, "the name of target 1 must be 1 to 40 characters"},
		{`{"targets":[` + target("", "a", "json", secret) + "," + target("", "a", "slack", secret) + `]}`, `two targets are named "a"`},
		{`{"targets":[` + target("", "a", "teams", secret) + `]}`, `the format of target "a" must be json, slack, dingtalk, wecom or feishu`},
		{`{"targets":[{"name":"a","format":"json","language":"de","enabled":true,"url":"https://h/x"}]}`, `the language of target "a" must be en or zh-CN`},
		{`{"targets":[` + target("", "a", "json", "ftp://h/SECRETPATHtoken") + `]}`, `the url of target "a" must be an absolute http or https URL with a host, at most 2048 characters`},
		{`{"targets":[` + target("", "a", "json", "/hook/SECRETPATHtoken") + `]}`, `the url of target "a" must be`},
		{`{"targets":[` + target("", "a", "json", "https://h/"+strings.Repeat("x", 2048)) + `]}`, `the url of target "a" must be`},
		{`{"targets":[` + target("0000000000000000", "a", "json", "") + `]}`, `target "a" is new and needs a url`},
		{`{"rules":{"instanceDown":{"enabled":true,"forSeconds":1.5}}}`, "invalid JSON body"},
		{`{"gatewayEvents":true}`, "invalid JSON body"},
	} {
		r := h.do("PUT", "/api/alerts/settings", tok, c.body)
		var e struct{ Error string }
		r.json(t, &e)
		if r.code != 400 || !strings.Contains(e.Error, c.msg) || strings.Contains(r.body, "SECRET") {
			t.Errorf("%s: %d %s", c.body, r.code, r.body)
		}
	}
	if _, ok := h.st.Alerts(); ok {
		t.Fatal("an invalid PUT saved settings")
	}

	// Rules left out keep their values.
	r := h.do("PUT", "/api/alerts/settings", tok, `{"rules":{"idleCpuLow":{"enabled":false,"forSeconds":30,"below":20}},"targets":[`+target("", "ops chat", "slack", secret)+`]}`)
	expect(t, r, 200, "")
	var got struct {
		Rules   store.AlertRules
		Targets []alertTargetView
	}
	r.json(t, &got)
	want := defaultAlertSettings().Rules
	want.IdleCPULow = store.BelowRule{AlertRule: store.AlertRule{ForSeconds: 30}, Below: 20}
	if got.Rules != want || len(got.Targets) != 1 || got.Targets[0].URLHint != "hooks.example.com:8443" || len(got.Targets[0].ID) != 16 || strings.Contains(r.body, "SECRET") {
		t.Fatalf("saved %s", r.body)
	}
	id := got.Targets[0].ID
	if g := h.do("GET", "/api/alerts/settings", tok, ""); g.body != r.body || strings.Contains(g.body, "SECRET") {
		t.Fatalf("GET %s", g.body)
	}

	// The same id without url keeps the URL; an unknown id is a new target.
	expect(t, h.do("PUT", "/api/alerts/settings", tok, `{"targets":[`+target(id, "ops", "wecom", "")+`]}`), 200, "")
	if a, _ := h.st.Alerts(); a.Targets[0].URL != secret || a.Targets[0].ID != id || a.Targets[0].Format != "wecom" || a.Rules.IdleCPULow.Below != 20 {
		t.Fatalf("stored %+v", a)
	}
	expect(t, h.do("PUT", "/api/alerts/settings", tok, `{"targets":[`+target(id, "ops", "wecom", "")+","+target(id, "twin", "json", "")+`]}`), 400, `{"error":"invalid alert settings: target \"twin\" is new and needs a url"}`)
	expect(t, h.do("PUT", "/api/alerts/settings", tok, `{"targets":[]}`), 200, "")
	expect(t, h.do("POST", "/api/alerts/test", tok, `{"id":"`+id+`"}`), 404, `{"error":"target not found"}`)

	h.srv.cfg.Audit.Close()
	data := string(must(t, auditPath))
	if strings.Contains(data, "SECRET") || strings.Contains(data, "/services/") {
		t.Fatalf("the audit log holds a URL:\n%s", data)
	}
	var details []string
	for _, e := range readAudit(t, auditPath) {
		if e.Action == "alerts.update" {
			details = append(details, e.Result+" "+e.Detail)
		}
	}
	for _, w := range []string{
		`ok rules idleCpuLow; target "ops chat" added (slack, hooks.example.com:8443)`,
		`ok target "ops" changed: renamed from "ops chat", format wecom`,
		`ok target "ops" removed`,
	} {
		if !strings.Contains(strings.Join(details, "\n"), w) {
			t.Errorf("no audit detail %q in\n%s", w, strings.Join(details, "\n"))
		}
	}
	// A viewer's attempt is audited as denied.
	if !strings.Contains(data, `"action":"alerts.update","target":"","detail":"PUT /api/alerts/settings","result":"denied"`) {
		t.Errorf("no denied entry:\n%s", data)
	}
}

// A PUT without targets (or with null) keeps the saved ones; [] removes them.
func TestAlertSettingsKeepTargets(t *testing.T) {
	var auditPath string
	h := alertHarness(t, withAudit(t, &auditPath))
	tok := h.login("admin", adminPW)
	hs := newHookServer(t, func(http.ResponseWriter, *http.Request, int) {})
	r := h.do("PUT", "/api/alerts/settings", tok, `{"targets":[{"name":"a","format":"json","language":"en","enabled":true,"url":"`+hs.URL+`/a"},{"name":"b","format":"slack","language":"en","enabled":false,"url":"`+hs.URL+`/b"}]}`)
	expect(t, r, 200, "")
	saved := r.body

	for _, body := range []string{
		`{"rules":{"idleCpuLow":{"enabled":false,"forSeconds":30,"below":20}}}`,
		`{"rules":{"idleCpuLow":{"enabled":true,"forSeconds":30,"below":20}},"targets":null}`,
	} {
		r = h.do("PUT", "/api/alerts/settings", tok, body)
		expect(t, r, 200, "")
		var got struct {
			Rules   store.AlertRules
			Targets []alertTargetView
		}
		r.json(t, &got)
		if got.Rules.IdleCPULow.Below != 20 || r.body[strings.Index(r.body, `"targets"`):] != saved[strings.Index(saved, `"targets"`):] {
			t.Fatalf("%s: saved %s, was %s", body, r.body, saved)
		}
		for _, tg := range got.Targets {
			expect(t, h.do("POST", "/api/alerts/test", tok, `{"id":"`+tg.ID+`"}`), 200, `{"ok":true,"status":200,"detail":""}`)
		}
	}
	if g := hs.got(); len(g) != 4 || g[0].path != "/a" || g[1].path != "/b" || g[2].path != "/a" || g[3].path != "/b" {
		t.Fatalf("got %+v", g)
	}

	expect(t, h.do("PUT", "/api/alerts/settings", tok, `{"targets":[]}`), 200, "")
	if a, _ := h.st.Alerts(); len(a.Targets) != 0 || a.Rules.IdleCPULow.Below != 20 {
		t.Fatalf("stored %+v", a)
	}

	h.srv.cfg.Audit.Close()
	var details []string
	for _, e := range readAudit(t, auditPath) {
		if e.Action == "alerts.update" {
			details = append(details, e.Detail)
		}
	}
	// The PUTs without targets change only the rule.
	if len(details) != 4 || !strings.HasPrefix(details[0], `target "a" added (json, 127.0.0.1:`) || details[1] != "rules idleCpuLow" || details[2] != "rules idleCpuLow" || details[3] != `target "a" removed; target "b" removed` {
		t.Fatalf("audit details %q", details)
	}
}
