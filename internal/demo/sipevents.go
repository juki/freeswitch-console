package demo

import (
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/juki/freeswitch-console/internal/esl"
)

// The SIP events of the simulation are the CUSTOM events mod_sofia fires,
// with the headers sofia_reg.c gives them: registrations renewing, now and
// then a phone that leaves (an expired registration, or an unregister from a
// softphone) and comes back, bursts of failed registrations from a few
// attackers, carrier-backup flapping between FAIL_WAIT and DOWN, carrier-a
// registering again, and a killed gateway deleted and added back. They do
// not change the registrations listed.

// attacker is a source that tries passwords on the internal profile.
type attacker struct {
	ip    string
	port  int
	agent string
}

// attackers are in the documentation ranges, like every address of the demo.
var attackers = []attacker{
	{"203.0.113.45", 5070, "friendly-scanner"},
	{"198.51.100.177", 5060, "sipcli/v1.8"},
	{"192.0.2.250", 5062, "VaxSIPUserAgent/3.1"},
}

// guesses are the users the attackers try: a few of the directory, most not.
var guesses = []string{"100", "101", "1000", "1001", "1002", "1003", "1010", "200", "2000", "9001", "admin", "test", "user", "1099", "500"}

// attempt is one failed registration of a burst, after the burst's start.
type attempt struct {
	attacker
	after time.Duration
	user  string
}

// sipPast is how far back SipEvents goes.
const sipPast = time.Hour

// sipEvent is the subclass and the own headers of a CUSTOM event, and where
// in mod_sofia it is fired.
type sipEvent struct {
	subclass string
	src      source
	headers  map[string]string
}

var (
	srcRegisterEvent   = source{"sofia_reg.c", "sofia_reg_handle_register_token", 2059}
	srcUnregisterEvent = source{"sofia_reg.c", "sofia_reg_handle_register_token", 2213}
	srcFailureEvent    = source{"sofia_reg.c", "sofia_reg_handle_register_token", 1792}
	srcExpireEvent     = source{"sofia_reg.c", "sofia_reg_del_callback", 726}
	srcGatewayState    = source{"sofia_reg.c", "sofia_reg_fire_custom_gateway_state_event", 156}
	srcGatewayAdd      = source{"sofia_reg.c", "sofia_reg_add_gateway", 3723}
	srcGatewayDelete   = source{"sofia_reg.c", "sofia_reg_check_gateway", 343}
)

// reregSteps are the states a registered gateway goes through when it
// registers again, and the response that moves it to REGISTER.
var reregSteps = []struct {
	after  time.Duration
	state  string
	status int
	phrase string
}{
	{0, "UNREGED", 0, ""},
	{time.Second, "TRYING", 0, ""},
	{time.Second + 80*time.Millisecond, "REGISTER", 200, "OK"},
	{2 * time.Second, "REGED", 0, ""},
}

// directoryUser reports whether id is a user of the sample directory.
func directoryUser(id string) bool {
	return slices.ContainsFunc(users, func(u struct{ id, name string }) bool { return u.id == id })
}

func between(rng *rand.Rand, lo, hi time.Duration) time.Duration {
	return lo + time.Duration(rng.Int63n(int64(hi-lo)+1))
}

// setupSip starts the generators of the live SIP events. They draw from a
// random source of their own, so the calls are those of a switch without
// them.
func (s *Switch) setupSip() {
	s.sipRng = rand.New(rand.NewSource(s.seed + 1))
	s.after(between(s.sipRng, 3*time.Minute, 12*time.Minute), s.leave)
	if g := s.gateway("carrier-backup"); g != nil {
		s.after(between(s.sipRng, 2*time.Minute, 5*time.Minute), func() { s.flap(g) })
	}
	if g := s.gateway("carrier-a"); g != nil {
		s.after(between(s.sipRng, 9*time.Minute, 25*time.Minute), func() { s.reregister(g) })
	}
}

// publishSip publishes e now.
func (s *Switch) publishSip(e sipEvent) {
	if len(s.subs) == 0 {
		return
	}
	h := s.eventHeaders("CUSTOM", e.src.file, e.src.fn, e.src.line)
	h["Event-Subclass"] = e.subclass
	maps.Copy(h, e.headers)
	s.publish(esl.Message{Kind: esl.KindEvent, Headers: h})
}

// roaming reports whether r is a phone that comes and goes: a mobile, web or
// soft phone, not a desk phone.
func roaming(r *registration) bool {
	return r.expires <= 1800
}

// leave has a roaming phone leave now and then: a softphone unregisters, the
// others let their registration expire. It registers again a few minutes
// later.
func (s *Switch) leave() {
	s.after(between(s.sipRng, 6*time.Minute, 15*time.Minute), s.leave)
	var away []*registration
	for _, r := range s.regs {
		if roaming(r) && s.visible(r) {
			away = append(away, r)
		}
	}
	if len(away) == 0 {
		return
	}
	r := away[s.sipRng.Intn(len(away))]
	back := between(s.sipRng, time.Minute, 8*time.Minute)
	if strings.Contains(r.agent, "SoftDial") {
		s.publishSip(s.unregisterEvent(r))
	} else {
		s.publishSip(s.expireEvent(r, s.t))
	}
	s.after(back, func() {
		if s.visible(r) {
			r.until = s.t.Add(time.Duration(r.expires) * time.Second)
			s.registered(r)
		}
	})
}

// burst is the n failed registrations of one attack by a, with rng.
func burst(rng *rand.Rand, a attacker, n int) []attempt {
	out := make([]attempt, n)
	at := time.Duration(0)
	for i := range out {
		out[i] = attempt{attacker: a, after: at, user: guesses[rng.Intn(len(guesses))]}
		at += between(rng, 300*time.Millisecond, 2*time.Second)
	}
	return out
}

// flap moves carrier-backup, which never answers, between FAIL_WAIT and DOWN.
func (s *Switch) flap(g *gateway) {
	s.after(between(s.sipRng, 2*time.Minute, 5*time.Minute), func() { s.flap(g) })
	if g.killed || !s.profile("external").running {
		return
	}
	switch g.state {
	case "FAIL_WAIT":
		g.state = "DOWN"
	case "DOWN":
		g.state = "FAIL_WAIT"
	default:
		return
	}
	s.publishSip(s.gatewayEvent(g, g.state, 0, ""))
}

// reregister has a registered gateway register again now and then; its
// state in sofia status stays REGED.
func (s *Switch) reregister(g *gateway) {
	s.after(between(s.sipRng, 9*time.Minute, 25*time.Minute), func() { s.reregister(g) })
	if !g.register || !g.usable() || g.state != "REGED" || !s.profile("external").running {
		return
	}
	for _, st := range reregSteps {
		s.after(st.after, func() {
			if !g.killed {
				s.publishSip(s.gatewayEvent(g, st.state, st.status, st.phrase))
			}
		})
	}
}

// contact is the Contact of r as mod_sofia stores it.
func contact(r *registration) string {
	if r.proto == "udp" {
		return fmt.Sprintf(`"%s" <sip:%s@%s:%d>`, r.user, r.user, r.ip, r.port)
	}
	return fmt.Sprintf("<sip:%s@%s:%d;transport=%s>", r.user, r.ip, r.port, r.proto)
}

// userHeaders are the headers of a registration of r; register and
// unregister events add the user's variables of the directory.
func (s *Switch) userHeaders(r *registration, vars bool) map[string]string {
	h := map[string]string{
		"profile-name": "internal",
		"contact":      contact(r),
		"call-id":      r.callID,
		"network-ip":   r.ip,
		"network-port": strconv.Itoa(r.port),
		"user-agent":   r.agent,
		"realm":        s.sp.ip,
		"username":     r.user,
	}
	if vars {
		for k, v := range map[string]string{
			"from-user": r.user, "from-host": s.sp.ip, "rpid": "unknown",
			"sip_number_alias": r.user, "sip_auth_username": r.user, "sip_auth_realm": s.sp.ip,
			"number_alias": r.user, "user_name": r.user, "domain_name": s.sp.ip,
			"effective_caller_id_name": userName(r.user), "effective_caller_id_number": r.user,
		} {
			h[k] = v
		}
	}
	return h
}

// registerEvent is the sofia::register of r.
func (s *Switch) registerEvent(r *registration, expires int) sipEvent {
	h := s.userHeaders(r, true)
	h["presence-hosts"] = s.sp.ip + "," + s.sp.ip
	h["status"] = "Registered(" + strings.ToUpper(r.proto) + ")"
	if r.proto == "wss" {
		h["status"] = "Registered(WSS-NAT)"
	}
	h["expires"] = strconv.Itoa(expires)
	h["to-user"], h["to-host"] = r.user, s.sp.ip
	return sipEvent{"sofia::register", srcRegisterEvent, h}
}

// unregisterEvent is the sofia::unregister of r: a REGISTER with expires 0.
func (s *Switch) unregisterEvent(r *registration) sipEvent {
	h := s.userHeaders(r, true)
	h["expires"] = "0"
	return sipEvent{"sofia::unregister", srcUnregisterEvent, h}
}

// expireEvent is the sofia::expire of r, purged at at: its expires is that
// unix time, as the sip_registrations table holds it.
func (s *Switch) expireEvent(r *registration, at time.Time) sipEvent {
	h := s.userHeaders(r, false)
	h["user"], h["host"] = r.user, s.sp.ip
	h["expires"] = strconv.FormatInt(at.Unix(), 10)
	return sipEvent{"sofia::expire", srcExpireEvent, h}
}

// failureEvent is the sofia::register_failure of a.
func (s *Switch) failureEvent(a attempt) sipEvent {
	return sipEvent{"sofia::register_failure", srcFailureEvent, map[string]string{
		"profile-name":      "internal",
		"to-user":           a.user,
		"to-host":           s.sp.ip,
		"network-ip":        a.ip,
		"network-port":      strconv.Itoa(a.port),
		"user-agent":        a.agent,
		"registration-type": "REGISTER",
	}}
}

// gatewayEvent is a sofia::gateway_state of g in state. A gateway that has
// had an answer carries the address it registers with.
func (s *Switch) gatewayEvent(g *gateway, state string, status int, phrase string) sipEvent {
	h := map[string]string{"Gateway": g.name, "State": state, "Ping-Status": g.status}
	if g.up {
		h["Register-Network-IP"], h["Register-Network-Port"] = g.ip, "5060"
	}
	if phrase != "" {
		h["Phrase"] = phrase
	}
	if status != 0 {
		h["Status"] = strconv.Itoa(status)
	}
	return sipEvent{"sofia::gateway_state", srcGatewayState, h}
}

// gatewayListEvent is the sofia::gateway_add or sofia::gateway_delete of g.
func gatewayListEvent(g *gateway, added bool) sipEvent {
	h := map[string]string{"Gateway": g.name, "profile-name": "external"}
	if added {
		return sipEvent{"sofia::gateway_add", srcGatewayAdd, h}
	}
	return sipEvent{"sofia::gateway_delete", srcGatewayDelete, h}
}

// SipEvents calls add for the SIP events of the hour before now, oldest
// first, with every header of the CUSTOM event: the renewals of the
// registrations up to the last one, two phones that left and came back, two
// or three bursts of failed registrations, carrier-backup flapping into the
// state it has now, and carrier-a registering again once or twice. The
// result depends only on the seed and the state now. A switch that does not
// connect has none.
func (s *Switch) SipEvents(add func(at time.Time, headers map[string]string)) {
	s.mu.Lock()
	if s.rng == nil || s.closed {
		s.mu.Unlock()
		return
	}
	now := s.t
	from := now.Add(-sipPast)
	rng := rand.New(rand.NewSource(s.seed))
	type past struct {
		at time.Time
		e  sipEvent
	}
	var events []past
	put := func(at time.Time, e sipEvent) {
		if at.After(from) && at.Before(now) {
			events = append(events, past{at, e})
		}
	}

	var away []*registration
	for _, r := range s.regs {
		interval := time.Duration(r.expires) * time.Second
		for at := r.until.Add(-interval); at.After(from); at = at.Add(-interval * time.Duration(85+rng.Intn(11)) / 100) {
			put(at, s.registerEvent(r, r.expires))
		}
		if roaming(r) {
			away = append(away, r)
		}
	}
	for range min(2, len(away)) {
		r := away[rng.Intn(len(away))]
		at := from.Add(between(rng, 5*time.Minute, 45*time.Minute))
		if strings.Contains(r.agent, "SoftDial") {
			put(at, s.unregisterEvent(r))
		} else {
			put(at, s.expireEvent(r, at))
		}
		put(at.Add(between(rng, time.Minute, 8*time.Minute)), s.registerEvent(r, r.expires))
	}
	for _, k := range rng.Perm(len(attackers))[:2+rng.Intn(2)] {
		at := from.Add(between(rng, 0, 55*time.Minute))
		for _, a := range burst(rng, attackers[k], 6+rng.Intn(25)) {
			put(at.Add(a.after), s.failureEvent(a))
		}
	}
	if g := s.gateway("carrier-backup"); g != nil && !g.killed && (g.state == "FAIL_WAIT" || g.state == "DOWN") {
		state := g.state
		for at := now.Add(-between(rng, 20*time.Second, 2*time.Minute)); at.After(from); at = at.Add(-between(rng, 2*time.Minute, 5*time.Minute)) {
			put(at, s.gatewayEvent(g, state, 0, ""))
			state = map[string]string{"FAIL_WAIT": "DOWN", "DOWN": "FAIL_WAIT"}[state]
		}
	}
	if g := s.gateway("carrier-a"); g != nil && g.register && g.usable() {
		for range 1 + rng.Intn(2) {
			at := from.Add(between(rng, 0, 55*time.Minute))
			for _, st := range reregSteps {
				put(at.Add(st.after), s.gatewayEvent(g, st.state, st.status, st.phrase))
			}
		}
	}

	slices.SortStableFunc(events, func(a, b past) int { return a.at.Compare(b.at) })
	out := make([]map[string]string, len(events))
	for i, p := range events {
		h := s.headersAt("CUSTOM", p.e.src.file, p.e.src.fn, p.e.src.line, p.at, uint64(i+1))
		h["Event-Subclass"] = p.e.subclass
		maps.Copy(h, p.e.headers)
		out[i] = h
	}
	s.mu.Unlock()
	for i, p := range events {
		add(p.at, out[i])
	}
}
