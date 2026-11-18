package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/juki/freeswitch-console/internal/audit"
	"github.com/juki/freeswitch-console/internal/fsapi"
	"github.com/juki/freeswitch-console/internal/store"
)

// Alerts. Four fixed rules (store.AlertRules) are evaluated for every
// instance every Config.AlertInterval, from state the console already holds:
// the connection state, the gateway states (below) and the last heartbeat.
// An alert fires or resolves once its condition has held the new value for
// the rule's forSeconds without interruption. Active alerts and the last
// alertHistorySize transitions live in memory only. Transitions are sent to
// the webhook targets (notify.go).
//
// One goroutine, runAlerts, evaluates; it owns the conditions, which
// alertMu guards so the handlers can read them. While an instance is
// disconnected, its gateway, CPU and session conditions are frozen: they
// neither fire nor resolve, and once it is connected again they count from
// the reconnect (gateways from the seed after it, the CPU and sessions from
// the first heartbeat after it).

const (
	alertHistorySize = 200
	alertEvery       = 5 * time.Second

	ruleInstanceDown = "instanceDown"
	ruleGatewayDown  = "gatewayDown"
	ruleIdleCPULow   = "idleCpuLow"
	ruleSessionsHigh = "sessionsHigh"
)

func defaultAlertSettings() store.AlertSettings {
	on := store.AlertRule{Enabled: true, ForSeconds: 60}
	return store.AlertSettings{
		Rules: store.AlertRules{
			InstanceDown: on,
			GatewayDown:  on,
			IdleCPULow:   store.BelowRule{AlertRule: on, Below: 10},
			SessionsHigh: store.AboveRule{AlertRule: on, Above: 90},
		},
		Targets: []store.AlertTarget{},
	}
}

// alertSettings returns the saved settings, or the defaults.
func (s *Server) alertSettings() store.AlertSettings {
	if a, ok := s.cfg.Store.Alerts(); ok {
		return a
	}
	return defaultAlertSettings()
}

// alertView is an alert as the API shows it. Fields that do not apply to
// its rule are zero.
type alertView struct {
	ID         string  `json:"id"`
	Rule       string  `json:"rule"`
	InstanceID string  `json:"instanceId"`
	Instance   string  `json:"instance"` // its name
	Subject    string  `json:"subject"`  // the gateway of gatewayDown
	Since      int64   `json:"since"`    // unix ms, when the condition began
	FiredAt    int64   `json:"firedAt"`  // unix ms
	State      string  `json:"state"`    // gateway state
	Ping       string  `json:"ping"`     // gateway ping status
	Reason     string  `json:"reason"`   // last connection error of the instance
	Value      float64 `json:"value"`    // idle CPU percent, or sessions percent of the limit
	Threshold  int     `json:"threshold"`
	Sessions   int     `json:"sessions"`
	Limit      int     `json:"limit"`
}

// alertEntry is one transition in the history. Only Deliveries changes
// once it is stored, under alertMu.
type alertEntry struct {
	alertView
	Seq        uint64          `json:"seq"`
	T          int64           `json:"t"`     // unix ms
	Event      string          `json:"event"` // firing, resolved; test only in a message
	DurationMs int64           `json:"durationMs"`
	Deliveries []alertDelivery `json:"deliveries"`
}

// alertDelivery is the outcome of sending an entry to one target.
type alertDelivery struct {
	Target string `json:"target"` // its name
	OK     bool   `json:"ok"`
	Status int    `json:"status"` // HTTP status; 0 without an answer
	Error  string `json:"error"`
}

// alertKey identifies an alert: a rule on an instance, and for gatewayDown
// the gateway.
type alertKey struct{ rule, instanceID, subject string }

// alertCond is what the evaluator keeps of a key whose alert is firing or
// whose condition differs from it.
type alertCond struct {
	view    alertView // ID, Since and FiredAt are set while firing
	firing  bool
	pending time.Time // when the condition began to differ from firing; zero while it does not
}

// alertScope is what one rule says about one instance at one evaluation.
type alertScope struct {
	frozen bool
	obs    map[string]alertObs // by subject
}

// alertObs is one condition observed.
type alertObs struct {
	cond bool
	at   time.Time // when the observed value began
	view alertView // the details of its alert
}

// runAlerts evaluates every interval until Close.
func (s *Server) runAlerts(every time.Duration) {
	defer close(s.alertDone)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.alertStop:
			return
		case <-t.C:
			s.evaluateAlerts(time.Now())
		}
	}
}

// evaluateAlerts applies the settings saved now to the cached state of every
// instance and sends the transitions.
func (s *Server) evaluateAlerts(now time.Time) {
	set := s.alertSettings()
	s.syncTargets(set.Targets)
	s.mu.Lock()
	list := s.cfg.Store.List()
	insts := maps.Clone(s.insts)
	s.mu.Unlock()
	type scopeKey struct{ rule, instanceID string }
	scopes := map[scopeKey]alertScope{}
	for _, in := range list {
		if inst := insts[in.ID]; inst != nil {
			inst.observeAlerts(in, set.Rules, func(rule string, sc alertScope) { scopes[scopeKey{rule, in.ID}] = sc })
		}
	}
	r := set.Rules
	forSeconds := map[string]int{
		ruleInstanceDown: r.InstanceDown.ForSeconds, ruleGatewayDown: r.GatewayDown.ForSeconds,
		ruleIdleCPULow: r.IdleCPULow.ForSeconds, ruleSessionsHigh: r.SessionsHigh.ForSeconds,
	}

	var out []notice
	apply := func(key alertKey, c *alertCond, o alertObs) {
		if e := s.applyAlert(c, o, forSeconds[key.rule], now); e != nil {
			out = append(out, notice{entry: e, data: *e})
		}
		if !c.firing && c.pending.IsZero() {
			delete(s.alertConds, key)
		}
	}
	s.alertMu.Lock()
	for key, c := range s.alertConds {
		sc, ok := scopes[scopeKey{key.rule, key.instanceID}]
		if !ok {
			// The rule is off or the instance deleted: dropped silently.
			delete(s.alertConds, key)
			continue
		}
		if sc.frozen {
			c.pending = time.Time{}
			if !c.firing {
				delete(s.alertConds, key)
			}
			continue
		}
		o, ok := sc.obs[key.subject]
		if !ok {
			// The gateway was deleted: dropped silently.
			delete(s.alertConds, key)
			continue
		}
		apply(key, c, o)
	}
	for sk, sc := range scopes {
		for subject, o := range sc.obs {
			key := alertKey{sk.rule, sk.instanceID, subject}
			if o.cond && s.alertConds[key] == nil {
				c := &alertCond{}
				s.alertConds[key] = c
				apply(key, c, o)
			}
		}
	}
	s.alertMu.Unlock()
	s.enqueue(out)
}

// applyAlert moves c on by the observation o and returns the history entry
// of a transition, or nil. alertMu must be held.
func (s *Server) applyAlert(c *alertCond, o alertObs, forSeconds int, now time.Time) *alertEntry {
	if c.firing && o.cond {
		v := o.view
		v.ID, v.Since, v.FiredAt = c.view.ID, c.view.Since, c.view.FiredAt
		c.view = v
	}
	if o.cond == c.firing {
		c.pending = time.Time{}
		return nil
	}
	if c.pending.IsZero() {
		c.pending = o.at
		if c.pending.IsZero() || c.pending.After(now) {
			c.pending = now
		}
	}
	if now.Sub(c.pending) < time.Duration(forSeconds)*time.Second {
		return nil
	}
	s.alertSeq++
	e := &alertEntry{Seq: s.alertSeq, T: now.UnixMilli(), Deliveries: []alertDelivery{}}
	if o.cond {
		c.view = o.view
		c.view.ID, c.view.Since, c.view.FiredAt = strconv.FormatUint(e.Seq, 10), c.pending.UnixMilli(), now.UnixMilli()
		e.alertView, e.Event = c.view, "firing"
	} else {
		v := o.view
		v.ID, v.Since, v.FiredAt = c.view.ID, c.view.Since, c.view.FiredAt
		// How long the condition held: until it ended, not until the
		// debounce let the alert resolve.
		e.alertView, e.Event, e.DurationMs = v, "resolved", max(0, c.pending.UnixMilli()-v.Since)
	}
	c.firing = o.cond
	c.pending = time.Time{}
	s.pushAlertEntry(e)
	return e
}

// pushAlertEntry stores e in the history ring. alertMu must be held.
func (s *Server) pushAlertEntry(e *alertEntry) {
	if len(s.alertHist) < alertHistorySize {
		s.alertHist = append(s.alertHist, e)
		return
	}
	s.alertHist[s.alertNext] = e
	s.alertNext = (s.alertNext + 1) % alertHistorySize
}

// recordDelivery adds the outcome d to e; e may have left the history.
func (s *Server) recordDelivery(e *alertEntry, d alertDelivery) {
	if e == nil {
		return
	}
	s.alertMu.Lock()
	defer s.alertMu.Unlock()
	e.Deliveries = append(e.Deliveries, d)
}

// later returns the later of a and b.
func later(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

// observeAlerts reports to add what each enabled rule says about the
// instance in, from its cached state.
func (i *instance) observeAlerts(in store.Instance, rules store.AlertRules, add func(rule string, sc alertScope)) {
	st := i.sw.State()
	base := alertView{InstanceID: in.ID, Instance: in.Name}
	one := func(o alertObs) alertScope { return alertScope{obs: map[string]alertObs{"": o}} }

	if rules.InstanceDown.Enabled {
		v := base
		v.Rule, v.Reason = ruleInstanceDown, st.LastError
		add(ruleInstanceDown, one(alertObs{cond: !st.Connected, at: st.Since, view: v}))
	}

	// Without the SIP events nothing keeps the gateway states: the rule is
	// inactive.
	if rules.GatewayDown.Enabled && i.sipSize > 0 {
		sc := alertScope{frozen: true}
		i.gwMu.Lock()
		if st.Connected && i.gwSeeded.Equal(st.Since) {
			sc = alertScope{obs: map[string]alertObs{}}
			for name, g := range i.gateways {
				if g.gone {
					continue
				}
				v := base
				v.Rule, v.Subject, v.State, v.Ping = ruleGatewayDown, name, g.state, g.ping
				sc.obs[name] = alertObs{cond: g.bad, at: later(g.since, st.Since), view: v}
			}
		}
		i.gwMu.Unlock()
		add(ruleGatewayDown, sc)
	}

	i.histMu.Lock()
	beat, limit, beatAt := i.beat, i.beatLimit, i.beatAt
	i.histMu.Unlock()
	fresh := st.Connected && !beatAt.Before(st.Since) // a heartbeat of this connection
	at := later(beatAt, st.Since)
	if r := rules.IdleCPULow; r.Enabled {
		sc := alertScope{frozen: true}
		if fresh {
			v := base
			v.Rule, v.Value, v.Threshold = ruleIdleCPULow, round1(beat.Idle), r.Below
			sc = one(alertObs{cond: beat.Idle < float64(r.Below), at: at, view: v})
		}
		add(ruleIdleCPULow, sc)
	}
	if r := rules.SessionsHigh; r.Enabled {
		sc := alertScope{frozen: true}
		if fresh {
			v := base
			v.Rule, v.Threshold, v.Sessions, v.Limit = ruleSessionsHigh, r.Above, beat.Sessions, limit
			if limit > 0 {
				v.Value = round1(float64(beat.Sessions) * 100 / float64(limit))
			}
			sc = one(alertObs{cond: limit > 0 && beat.Sessions*100 >= r.Above*limit, at: at, view: v})
		}
		add(ruleSessionsHigh, sc)
	}
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// gatewayState is what the console knows of one gateway of an instance.
type gatewayState struct {
	state, ping string
	bad         bool
	gone        bool      // deleted; kept so a seed in flight does not bring it back
	since       time.Time // when bad last changed, or the gateway was first seen
	gen         uint64    // gwGen of the last event about it
}

// Gateway states and ping statuses as mod_sofia names them
// (sofia_state_names in mod_sofia.c, sofia_gateway_status_name in sofia.c):
// UNREGED, TRYING, REGISTER, REGED, UNREGISTER, FAILED, FAIL_WAIT, EXPIRED,
// NOREG, DOWN, TIMEOUT; ping UP, DOWN (INVALID cannot occur).
var badGatewayStates = []string{"FAIL_WAIT", "FAILED", "EXPIRED", "TIMEOUT", "DOWN"}

// classifyGateway says whether a gateway is bad. known is false for the
// states in between (TRYING, REGISTER, UNREGED, UNREGISTER) with a ping that
// is not DOWN: the gateway keeps its classification.
func classifyGateway(state, ping string) (bad, known bool) {
	switch {
	case ping == "DOWN" || slices.Contains(badGatewayStates, state):
		return true, true
	case state == "REGED" || state == "NOREG":
		return false, true
	}
	return false, false
}

// setGateway records state and ping of the gateway name at now. gwMu must
// be held.
func (i *instance) setGateway(name, state, ping string, now time.Time) *gatewayState {
	g := i.gateways[name]
	if g == nil || g.gone {
		g = &gatewayState{since: now}
		i.gateways[name] = g
	}
	g.state, g.ping = state, ping
	if bad, known := classifyGateway(state, ping); known && bad != g.bad {
		g.bad, g.since = bad, now
	}
	return g
}

// gatewayEvent keeps the gateway states from a gateway_state, gateway_add
// or gateway_delete event received at now. A gateway_add carries no state:
// the gateway is good until an event or a seed says otherwise.
func (i *instance) gatewayEvent(h map[string]string, now time.Time) {
	name := h["Gateway"]
	if i.sipSize == 0 || name == "" {
		return
	}
	i.gwMu.Lock()
	defer i.gwMu.Unlock()
	switch h["Event-Subclass"] {
	case "sofia::gateway_state":
		i.gwGen++
		i.setGateway(name, h["State"], h["Ping-Status"], now).gen = i.gwGen
	case "sofia::gateway_add":
		if g := i.gateways[name]; g == nil || g.gone {
			i.gwGen++
			i.gateways[name] = &gatewayState{since: now, gen: i.gwGen}
		}
	case "sofia::gateway_delete":
		i.gwGen++
		i.gateways[name] = &gatewayState{gone: true, gen: i.gwGen}
	}
}

// seedGateways reads the state of every gateway once, in a goroutine of its
// own: the answer comes over the connection whose events the collector
// reads. A gateway an event has told about since the command was sent
// (deleted included) keeps what the event said; one the answer does not list
// and no event has told about since is dropped, with old deletions. Until the seed of the current connection is done, the
// gatewayDown conditions of the instance are frozen.
func (i *instance) seedGateways() {
	if i.sipSize == 0 {
		return
	}
	since := i.sw.State().Since
	i.gwMu.Lock()
	gen := i.gwGen
	i.gwMu.Unlock()
	i.seeding.Add(1)
	go func() {
		defer i.seeding.Done()
		ctx, cancel := context.WithTimeout(context.Background(), eslTimeout)
		defer cancel()
		rows, err := fsapi.SofiaGateways(ctx, i.sw)
		now := time.Now()
		i.gwMu.Lock()
		defer i.gwMu.Unlock()
		if err == nil {
			listed := map[string]bool{}
			for _, row := range rows {
				name := row["name"]
				listed[name] = true
				if g := i.gateways[name]; g == nil || g.gen <= gen {
					i.setGateway(name, row["state"], row["status"], now)
				}
			}
			for name, g := range i.gateways {
				if !listed[name] && g.gen <= gen {
					delete(i.gateways, name)
				}
			}
		}
		// On an error the states stay as they were; the events keep them.
		if since.After(i.gwSeeded) {
			i.gwSeeded = since
		}
	}()
}

// alertCounts returns the number of active alerts per instance ID.
func (s *Server) alertCounts() map[string]int {
	s.alertMu.Lock()
	defer s.alertMu.Unlock()
	n := map[string]int{}
	for key, c := range s.alertConds {
		if c.firing {
			n[key.instanceID]++
		}
	}
	return n
}

func (s *Server) handleAlerts(w http.ResponseWriter, _ *http.Request, _ user) {
	active := []alertView{}
	s.alertMu.Lock()
	for _, c := range s.alertConds {
		if c.firing {
			active = append(active, c.view)
		}
	}
	n := len(s.alertHist)
	history := make([]alertEntry, 0, n)
	for k := 1; k <= n; k++ {
		e := *s.alertHist[(s.alertNext-k+n)%n]
		e.Deliveries = append([]alertDelivery{}, e.Deliveries...)
		history = append(history, e)
	}
	s.alertMu.Unlock()
	slices.SortFunc(active, func(a, b alertView) int {
		return cmp.Or(cmp.Compare(a.Since, b.Since), cmp.Compare(a.FiredAt, b.FiredAt), cmp.Compare(a.ID, b.ID))
	})
	writeJSON(w, http.StatusOK, struct {
		Active  []alertView  `json:"active"`
		History []alertEntry `json:"history"`
	}{active, history})
}

// alertTargetView is a target as the API shows it: the host of its URL,
// never the URL.
type alertTargetView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Format   string `json:"format"`
	Language string `json:"language"`
	Enabled  bool   `json:"enabled"`
	URLHint  string `json:"urlHint"`
}

func (s *Server) alertSettingsView(a store.AlertSettings) any {
	targets := []alertTargetView{}
	for _, t := range a.Targets {
		targets = append(targets, alertTargetView{ID: t.ID, Name: t.Name, Format: t.Format, Language: t.Language, Enabled: t.Enabled, URLHint: store.URLHost(t.URL)})
	}
	return struct {
		GatewayEvents bool              `json:"gatewayEvents"`
		Rules         store.AlertRules  `json:"rules"`
		Targets       []alertTargetView `json:"targets"`
	}{s.cfg.SipEvents > 0, a.Rules, targets}
}

func (s *Server) handleAlertSettings(w http.ResponseWriter, _ *http.Request, _ user) {
	writeJSON(w, http.StatusOK, s.alertSettingsView(s.alertSettings()))
}

// handleUpdateAlertSettings replaces the settings. Rules left out of the
// body keep their values. Without targets (or with null) the saved targets
// stay as they are, and [] removes them all. Otherwise a target with the ID
// of a saved one updates it, and keeps its URL when url is empty; any other
// target is new and needs a url; saved targets left out are removed.
func (s *Server) handleUpdateAlertSettings(w http.ResponseWriter, r *http.Request, u user) {
	s.alertSetMu.Lock()
	defer s.alertSetMu.Unlock()
	old := s.alertSettings()
	var req struct {
		Rules   store.AlertRules `json:"rules"`
		Targets []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Format   string `json:"format"`
			Language string `json:"language"`
			Enabled  bool   `json:"enabled"`
			URL      string `json:"url"`
		} `json:"targets"`
	}
	req.Rules = old.Rules
	if !decodeJSON(w, r, &req) {
		return
	}
	next := store.AlertSettings{Rules: req.Rules, Targets: []store.AlertTarget{}}
	if req.Targets == nil { // absent or null; [] decodes to an empty, non-nil slice
		next.Targets = append(next.Targets, old.Targets...)
	}
	used := map[string]bool{}
	var err error
	for _, t := range req.Targets {
		nt := store.AlertTarget{Name: t.Name, Format: t.Format, Language: t.Language, Enabled: t.Enabled, URL: t.URL}
		k := slices.IndexFunc(old.Targets, func(o store.AlertTarget) bool { return o.ID == t.ID })
		switch {
		case t.ID != "" && k >= 0 && !used[t.ID]:
			used[t.ID] = true
			nt.ID = t.ID
			if nt.URL == "" {
				nt.URL = old.Targets[k].URL
			}
		case nt.URL == "" && err == nil:
			err = fmt.Errorf("%w: target %q is new and needs a url", store.ErrInvalidAlerts, cutText(t.Name, 40))
		}
		next.Targets = append(next.Targets, nt)
	}
	saved := next
	if err == nil {
		saved, err = s.cfg.Store.SetAlerts(next)
	}
	s.record(r, audit.Entry{User: u.Username, Action: "alerts.update", Detail: alertChanges(old, next)}, err)
	switch {
	case errors.Is(err, store.ErrInvalidAlerts):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "cannot save the alert settings: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, s.alertSettingsView(saved))
	}
}

// alertChanges describes the change from old to next for the audit log:
// the rules changed, and the targets added, removed or changed by name,
// format and host. It never shows more of a URL than its host.
func alertChanges(old, next store.AlertSettings) string {
	var parts, rules []string
	o, n := old.Rules, next.Rules
	for _, c := range []struct {
		key  string
		same bool
	}{
		{ruleInstanceDown, o.InstanceDown == n.InstanceDown}, {ruleGatewayDown, o.GatewayDown == n.GatewayDown},
		{ruleIdleCPULow, o.IdleCPULow == n.IdleCPULow}, {ruleSessionsHigh, o.SessionsHigh == n.SessionsHigh},
	} {
		if !c.same {
			rules = append(rules, c.key)
		}
	}
	if len(rules) > 0 {
		parts = append(parts, "rules "+strings.Join(rules, ","))
	}
	for _, t := range next.Targets {
		k := slices.IndexFunc(old.Targets, func(p store.AlertTarget) bool { return t.ID != "" && p.ID == t.ID })
		if k < 0 {
			parts = append(parts, fmt.Sprintf("target %q added (%s, %s)", cutText(t.Name, 40), cutText(t.Format, 16), store.URLHost(t.URL)))
			continue
		}
		p := old.Targets[k]
		var what []string
		if p.Name != t.Name {
			what = append(what, fmt.Sprintf("renamed from %q", cutText(p.Name, 40)))
		}
		if p.Format != t.Format {
			what = append(what, "format "+cutText(t.Format, 16))
		}
		if p.Language != t.Language {
			what = append(what, "language "+cutText(t.Language, 16))
		}
		if p.Enabled != t.Enabled {
			what = append(what, "enabled "+strconv.FormatBool(t.Enabled))
		}
		if p.URL != t.URL {
			what = append(what, "url host "+store.URLHost(t.URL))
		}
		if len(what) > 0 {
			parts = append(parts, fmt.Sprintf("target %q changed: %s", cutText(t.Name, 40), strings.Join(what, ", ")))
		}
	}
	for _, p := range old.Targets {
		if !slices.ContainsFunc(next.Targets, func(t store.AlertTarget) bool { return t.ID == p.ID }) {
			parts = append(parts, fmt.Sprintf("target %q removed", cutText(p.Name, 40)))
		}
	}
	if len(parts) == 0 {
		return "no change"
	}
	return strings.Join(parts, "; ")
}

// handleAlertTest sends the test message to a saved target now, once, and
// answers how it went.
func (s *Server) handleAlertTest(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	set, _ := s.cfg.Store.Alerts()
	k := slices.IndexFunc(set.Targets, func(t store.AlertTarget) bool { return t.ID == req.ID })
	if req.ID == "" || k < 0 {
		writeError(w, http.StatusNotFound, "target not found")
		return
	}
	t := set.Targets[k]
	now := time.Now()
	res := s.post(r.Context(), t, alertEntry{alertView: alertView{Since: now.UnixMilli()}, T: now.UnixMilli(), Event: "test"})
	var err error
	if !res.ok {
		err = errors.New(res.why)
	}
	s.record(r, audit.Entry{User: u.Username, Action: "alerts.test", Target: t.Name, Detail: "format=" + t.Format + " host=" + store.URLHost(t.URL)}, err)
	writeJSON(w, http.StatusOK, struct {
		OK     bool   `json:"ok"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}{res.ok, res.status, res.detail})
}
