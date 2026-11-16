package demo

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/juki/freeswitch-console/internal/esl"
)

// outcome is how a call ends.
type outcome int

const (
	normal        outcome = iota // answered, then one side hangs up
	cancel                       // the caller gives up while it rings
	noAnswer                     // nobody answers within call_timeout
	busy                         // 486
	rejected                     // 603
	noRoute                      // the dialplan has no extension for it, 404
	notRegistered                // the user has no device; voicemail answers
	timerExpired                 // the carrier never answers the INVITE, 408
	mediaTimeout                 // answered, then the RTP stops
	incompatible                 // no common codec, 488
)

// outcomeMix is the share of each outcome in percent.
var outcomeMix = [...]struct {
	o      outcome
	weight float64
}{
	{normal, 78}, {cancel, 6}, {noAnswer, 5}, {busy, 4}, {rejected, 2}, {noRoute, 1.5},
	{notRegistered, 1.5}, {timerExpired, 1}, {mediaTimeout, 0.5}, {incompatible, 0.5},
}

// kind is where a call comes from and goes to.
type kind int

const (
	inbound   kind = iota // from a carrier to an extension
	local                 // from an extension to another
	outbound              // from an extension to a carrier
	originate             // one leg started from the console
)

type channel struct {
	uuid      string
	call      *call
	session   int
	direction string // inbound or outbound, as FreeSWITCH sees it
	name      string
	profile   string
	gateway   string
	cidName   string
	cidNum    string
	dest      string
	context   string
	app       string
	appData   string
	created   time.Time
	answered  time.Time // zero while not answered
	state     string    // channel state
	callstate string
	ip        string // the peer's signalling address
	port      int
	proto     string
	agent     string
	fromHost  string
	callID    string
	codec     string
	rate      int
	mos       float64

	calleeName, calleeNum string
}

type call struct {
	kind        kind
	outcome     outcome
	legs        []*channel // the a-leg first
	callerHangs bool
	// from the start of the call; 0 for a step that does not happen
	ring, answer, end time.Duration
	ended             bool
	pending           []*event // the scheduled steps
}

// ending is how one leg ended.
type ending struct {
	cause  string
	status int    // SIP status; 0 for none
	disp   string // sip_hangup_disposition
	failed string // originate_failed_cause
}

// q850 is the cause code FreeSWITCH reports in hangup_cause_q850.
var q850 = map[string]int{
	"NORMAL_CLEARING": 16, "USER_BUSY": 17, "NO_ANSWER": 19, "CALL_REJECTED": 21, "NO_ROUTE_DESTINATION": 3,
	"INCOMPATIBLE_DESTINATION": 88, "RECOVERY_ON_TIMER_EXPIRE": 102, "ORIGINATOR_CANCEL": 487,
	"MEDIA_TIMEOUT": 604, "MANAGER_REQUEST": 503, "USER_NOT_REGISTERED": 606,
}

// The extensions of the sample dialplan, as the dialplan log lines show them.
var (
	extLocal    = [2]string{"local_extension", `^(10[01][0-9])$`}
	extOutbound = [2]string{"outbound_carrier", `^\+?(1\d{10})$`}
	extInbound  = [2]string{"inbound_numbers", `^\+?131255501(0[1-9]|10)$`}
	extOthers   = [][2]string{{"check_voicemail", `^\*98$`}, {"echo_test", `^9196$`}}
)

var areaCodes = []string{"212", "312", "415", "617", "713", "206"}

func userName(id string) string {
	for _, u := range users {
		if u.id == id {
			return u.name
		}
	}
	return id
}

// did is the number of the public dialplan that rings user (1001 to 1010).
func did(user string) string {
	return "+1312555" + "01" + user[2:]
}

func (s *Switch) number() string {
	return fmt.Sprintf("1%s55501%02d", areaCodes[s.rng.Intn(len(areaCodes))], s.rng.Intn(100))
}

func (s *Switch) pickOutcome() outcome {
	x := s.rng.Float64() * 100
	for _, m := range outcomeMix {
		if x < m.weight {
			return m.o
		}
		x -= m.weight
	}
	return normal
}

// newCall starts a call with a random outcome, if the switch can take it.
func (s *Switch) newCall() {
	c := s.plan(s.pickOutcome())
	if c == nil || len(s.channels)+len(c.legs) > s.limit() {
		return
	}
	s.begin(c)
}

// plan draws everything about a call with outcome o: its kind, parties,
// codec and times. It returns nil when no kind of call is possible now,
// such as while both profiles are stopped.
func (s *Switch) plan(o outcome) *call {
	internal, external := s.profile("internal").running, s.profile("external").running
	var devices []*registration
	for _, r := range s.regs {
		if s.visible(r) {
			devices = append(devices, r)
		}
	}
	var gws []*gateway
	for _, g := range s.gateways {
		if g.usable() {
			gws = append(gws, g)
		}
	}
	single := o == noRoute || o == notRegistered
	type choice struct {
		k      kind
		weight int
	}
	var choices []choice
	total := 0
	add := func(k kind, weight int, ok bool) {
		if ok {
			choices = append(choices, choice{k, weight})
			total += weight
		}
	}
	add(inbound, 40, o != timerExpired && external && len(gws) > 0 && (single || internal && len(devices) > 0))
	add(local, 35, o != timerExpired && internal && len(devices) > 0 && (single || s.otherDevice(devices, devices[0].user)))
	add(outbound, 25, !single && internal && external && len(gws) > 0 && len(devices) > 0)
	if total == 0 {
		return nil
	}
	pick := s.rng.Intn(total)
	var k kind
	for _, ch := range choices {
		if pick < ch.weight {
			k = ch.k
			break
		}
		pick -= ch.weight
	}

	c := &call{kind: k, outcome: o, callerHangs: s.rng.Intn(2) == 0}
	var a, b *channel
	var gw *gateway
	if len(gws) > 0 {
		gw = gws[s.rng.Intn(len(gws))]
	}
	switch k {
	case inbound:
		from := "+" + s.number()
		a = &channel{direction: "inbound", name: "sofia/external/" + from + "@" + gw.proxy, profile: "external", gateway: gw.name,
			cidName: from, cidNum: from, context: "public", ip: gw.ip, port: 5060, proto: "udp", agent: gw.agent, fromHost: gw.proxy}
	default:
		a = s.deviceLeg(devices[s.rng.Intn(len(devices))], "inbound")
	}
	a.uuid, a.callID = s.uuid(), s.token()

	var callee string
	switch {
	case k == outbound:
		num := s.number()
		a.dest, a.calleeName, a.calleeNum = num, num, num
		a.app, a.appData = "bridge", "sofia/gateway/carrier-a/"+num+"|sofia/gateway/carrier-b/"+num
		b = &channel{direction: "outbound", name: "sofia/external/" + num, profile: "external", gateway: gw.name, dest: num,
			cidName: a.cidName, cidNum: s.values["outbound_caller_id"], ip: gw.ip, port: 5060, proto: "udp", agent: gw.agent, fromHost: s.sp.ip,
			calleeName: num, calleeNum: num}
	case o == noRoute && k == inbound:
		a.dest = fmt.Sprintf("+131255501%02d", 50+s.rng.Intn(50))
	case o == noRoute:
		a.dest = "1099"
	case o == notRegistered:
		var absent []string
		for _, u := range users {
			if !s.hasDevice(devices, u.id) {
				absent = append(absent, u.id)
			}
		}
		callee = absent[s.rng.Intn(len(absent))]
	default:
		var targets []*registration
		for _, r := range devices {
			if r.user != a.cidNum {
				targets = append(targets, r)
			}
		}
		d := targets[s.rng.Intn(len(targets))]
		callee = d.user
		b = s.deviceLeg(d, "outbound")
	}
	if callee != "" {
		a.calleeName, a.calleeNum = userName(callee), callee
		a.app, a.appData = "bridge", "user/"+callee+"@"+s.sp.ip
		a.dest = callee
		if k == inbound {
			a.context = "default" // after the transfer of the public dialplan
		}
	}
	if b != nil {
		b.uuid, b.callID = s.uuid(), s.token()
		b.context = a.context
		if k != outbound {
			b.cidName, b.cidNum, b.dest = a.cidName, a.cidNum, callee
		}
		b.calleeName, b.calleeNum = a.calleeName, a.calleeNum
		c.legs = []*channel{a, b}
	} else {
		c.legs = []*channel{a}
	}

	codec := "PCMU"
	switch x := s.rng.Float64(); {
	case a.proto == "wss" || b != nil && b.proto == "wss":
		codec = "OPUS"
	case gw != nil && (k != local):
		if x < 0.5 {
			codec = "PCMA"
		}
	case x < 0.25:
		codec = "OPUS"
	case x < 0.4:
		codec = "PCMA"
	}
	for _, ch := range c.legs {
		ch.call, ch.codec, ch.rate = c, codec, 8000
		if codec == "OPUS" {
			ch.rate = 48000
		}
		ch.mos = 3.9 + 0.5*s.rng.Float64()
	}

	ring := s.between(300*time.Millisecond, time.Second)
	talk := s.between(20*time.Second, 180*time.Second)
	switch o {
	case normal, mediaTimeout:
		c.ring, c.answer = ring, ring+s.between(2*time.Second, 12*time.Second)
		c.end = c.answer + talk
		if o == mediaTimeout {
			a.mos = 2.0 + 0.2*s.rng.Float64()
		}
	case cancel:
		c.ring, c.end = ring, ring+s.between(3*time.Second, 25*time.Second)
	case noAnswer:
		c.ring, c.end = ring, 30*time.Second
	case busy:
		c.end = s.between(500*time.Millisecond, 2*time.Second)
	case rejected:
		c.ring, c.end = ring, ring+s.between(2*time.Second, 10*time.Second)
	case incompatible:
		c.end = s.between(200*time.Millisecond, 500*time.Millisecond)
	case timerExpired:
		c.end = 32 * time.Second
	case noRoute:
		c.end = s.between(50*time.Millisecond, 200*time.Millisecond)
	case notRegistered:
		c.answer = s.between(300*time.Millisecond, 600*time.Millisecond)
		c.end = c.answer + s.between(8*time.Second, 40*time.Second)
	}
	return c
}

// deviceLeg is the leg of the internal profile facing the registered device
// d: the caller's (inbound) or the callee's (outbound).
func (s *Switch) deviceLeg(d *registration, direction string) *channel {
	ch := &channel{direction: direction, profile: "internal", context: "default", ip: d.ip, port: d.port, proto: d.proto, agent: d.agent}
	if direction == "inbound" {
		ch.name, ch.cidName, ch.cidNum, ch.fromHost = "sofia/internal/"+d.user+"@"+s.sp.ip, userName(d.user), d.user, s.sp.ip
	} else {
		ch.name, ch.fromHost = fmt.Sprintf("sofia/internal/%s@%s:%d", d.user, d.ip, d.port), s.sp.ip
	}
	return ch
}

func (s *Switch) hasDevice(devices []*registration, user string) bool {
	for _, r := range devices {
		if r.user == user {
			return true
		}
	}
	return false
}

// otherDevice reports whether a device of a user other than user exists.
func (s *Switch) otherDevice(devices []*registration, user string) bool {
	for _, r := range devices {
		if r.user != user {
			return true
		}
	}
	return false
}

// open adds a new channel.
func (s *Switch) open(ch *channel) {
	ch.created = s.t
	s.channels[ch.uuid] = ch
	s.created()
	ch.session = s.sessionN
	ch.callstate = "DOWN"
	s.logf(levelNotice, srcNewChannel, ch.uuid, "New Channel %s [%s]", ch.name, ch.uuid)
}

// begin starts c now: its first log lines and SIP messages, and the
// schedule of the rest.
func (s *Switch) begin(c *call) {
	a := c.legs[0]
	a.state = "CS_EXECUTE"
	s.open(a)
	s.logf(levelDebug, srcState, a.uuid, "Channel %s entering state [received][100]", a.name)
	s.sip(a.profile, true, a.proto, a.ip, a.port, "INVITE sip:"+a.dest+"@"+s.sp.ip+" SIP/2.0", a.cidNum, a.fromHost, a.callID, "1 INVITE", a.agent)
	s.sip(a.profile, false, a.proto, a.ip, a.port, "SIP/2.0 100 Trying", a.cidNum, a.fromHost, a.callID, "1 INVITE", "")
	if !s.route(c) {
		a.state = "CS_ROUTING"
		s.logf(levelInfo, srcNoRoute, a.uuid, "No Route, Aborting")
	}
	if c.outcome == notRegistered {
		s.logf(levelDebug, srcExecute, a.uuid, "EXECUTE [depth=0] %s bridge(%s)", a.name, a.appData)
		s.logf(levelInfo, srcOriginate, a.uuid, "Cannot create outgoing channel of type [user] cause: [USER_NOT_REGISTERED]")
		s.logf(levelInfo, srcBridgeFail, a.uuid, "Originate Failed.  Cause: USER_NOT_REGISTERED")
		s.logf(levelDebug, srcExecute, a.uuid, "EXECUTE [depth=0] %s answer()", a.name)
		a.app, a.appData = "voicemail", "default "+s.sp.ip+" "+a.calleeNum
		s.logf(levelDebug, srcExecute, a.uuid, "EXECUTE [depth=0] %s voicemail(%s)", a.name, a.appData)
	}
	if len(c.legs) > 1 {
		b := c.legs[1]
		s.logf(levelDebug, srcExecute, a.uuid, "EXECUTE [depth=0] %s bridge(%s)", a.name, a.appData)
		b.state = "CS_CONSUME_MEDIA"
		s.open(b)
		s.sip(b.profile, false, b.proto, b.ip, b.port, "INVITE sip:"+b.dest+"@"+b.ip+" SIP/2.0", b.cidNum, b.fromHost, b.callID, "1 INVITE", "")
	}
	s.schedule(c)
}

// schedule plans the ringing, answer and end of c from now.
func (s *Switch) schedule(c *call) {
	if c.ring > 0 {
		c.pending = append(c.pending, s.after(c.ring, func() { s.ringing(c) }))
	}
	if c.answer > 0 {
		c.pending = append(c.pending, s.after(c.answer, func() { s.answered(c) }))
	}
	c.pending = append(c.pending, s.after(c.end, func() { s.end(c, nil) }))
}

// route logs the dialplan of the a-leg of c and reports whether an
// extension matched.
func (s *Switch) route(c *call) bool {
	a := c.legs[0]
	if c.kind == originate {
		return true
	}
	pass := func(ext [2]string, dest string, actions ...string) {
		s.logf(levelDebug, srcDialplan, a.uuid, "Dialplan: %s parsing [%s->%s] continue=false", a.name, a.context, ext[0])
		s.logf(levelDebug, srcDialplan, a.uuid, "Dialplan: %s Regex (PASS) [%s] destination_number(%s) =~ /%s/ break=on-false", a.name, ext[0], dest, ext[1])
		for _, act := range actions {
			s.logf(levelDebug, srcDialplan, a.uuid, "Dialplan: %s Action %s", a.name, act)
		}
	}
	fail := func(context, dest string, exts ...[2]string) {
		for _, ext := range exts {
			s.logf(levelDebug, srcDialplan, a.uuid, "Dialplan: %s parsing [%s->%s] continue=false", a.name, context, ext[0])
			s.logf(levelDebug, srcDialplan, a.uuid, "Dialplan: %s Regex (FAIL) [%s] destination_number(%s) =~ /%s/ break=on-false", a.name, ext[0], dest, ext[1])
		}
	}
	processing := func(dest, context string) {
		s.logf(levelInfo, srcProcessing, a.uuid, "Processing %s <%s>->%s in context %s", a.cidName, a.cidNum, dest, context)
	}
	if c.kind == inbound {
		number := did(a.dest)
		if c.outcome == noRoute {
			number = a.dest
		}
		processing(number, "public")
		if c.outcome == noRoute {
			fail("public", number, extInbound)
			return false
		}
		suffix := a.dest[2:]
		ctx := a.context
		a.context = "public"
		pass(extInbound, number, "set(domain_name="+s.sp.ip+")", "transfer(10"+suffix+" XML default)")
		a.context = ctx
		s.logf(levelDebug, srcExecute, a.uuid, "EXECUTE [depth=0] %s set(domain_name=%s)", a.name, s.sp.ip)
		s.logf(levelDebug, srcExecute, a.uuid, "EXECUTE [depth=0] %s transfer(%s XML default)", a.name, a.dest)
		s.logf(levelNotice, srcTransfer, a.uuid, "Transfer %s to XML[%s@default]", a.name, a.dest)
	}
	processing(a.dest, "default")
	switch {
	case c.kind == outbound:
		fail("default", a.dest, extLocal)
		pass(extOutbound, a.dest, "set(effective_caller_id_number="+s.values["outbound_caller_id"]+")", "bridge("+a.appData+")")
		s.logf(levelDebug, srcExecute, a.uuid, "EXECUTE [depth=0] %s set(effective_caller_id_number=%s)", a.name, s.values["outbound_caller_id"])
	case c.outcome == noRoute:
		fail("default", a.dest, append([][2]string{extLocal, extOutbound}, extOthers...)...)
		return false
	default:
		pass(extLocal, a.dest, "set(call_timeout=30)", "set(hangup_after_bridge=true)", "set(continue_on_fail=true)", "bridge("+a.appData+")",
			"answer()", "voicemail(default "+s.sp.ip+" "+a.dest+")")
		for _, v := range []string{"call_timeout=30", "hangup_after_bridge=true", "continue_on_fail=true"} {
			s.logf(levelDebug, srcExecute, a.uuid, "EXECUTE [depth=0] %s set(%s)", a.name, v)
		}
	}
	return true
}

func (s *Switch) ringing(c *call) {
	if c.ended {
		return
	}
	for _, ch := range c.legs {
		ch.callstate = "RINGING"
	}
	if len(c.legs) > 1 {
		a, b := c.legs[0], c.legs[1]
		s.logf(levelNotice, srcRingReady, b.uuid, "Ring-Ready %s!", b.name)
		s.sip(b.profile, true, b.proto, b.ip, b.port, "SIP/2.0 180 Ringing", b.dest, b.ip, b.callID, "1 INVITE", b.agent)
		s.sip(a.profile, false, a.proto, a.ip, a.port, "SIP/2.0 180 Ringing", a.cidNum, a.fromHost, a.callID, "1 INVITE", "")
	} else {
		s.logf(levelNotice, srcRingReady, c.legs[0].uuid, "Ring-Ready %s!", c.legs[0].name)
	}
}

func (s *Switch) answered(c *call) {
	if c.ended {
		return
	}
	state := "CS_EXECUTE"
	if len(c.legs) > 1 {
		state = "CS_EXCHANGE_MEDIA"
	}
	for i := len(c.legs) - 1; i >= 0; i-- {
		ch := c.legs[i]
		ch.answered, ch.callstate, ch.state = s.t, "ACTIVE", state
		s.logf(levelNotice, srcAnswered, ch.uuid, "Channel [%s] has been answered", ch.name)
	}
	a := c.legs[0]
	if len(c.legs) > 1 {
		b := c.legs[1]
		s.sip(b.profile, true, b.proto, b.ip, b.port, "SIP/2.0 200 OK", b.dest, b.ip, b.callID, "1 INVITE", b.agent)
	}
	s.sip(a.profile, false, a.proto, a.ip, a.port, "SIP/2.0 200 OK", a.cidNum, a.fromHost, a.callID, "1 INVITE", "")
	if c.kind == originate && a.app != "" {
		s.logf(levelDebug, srcExecute, a.uuid, "EXECUTE [depth=0] %s %s(%s)", a.name, a.app, a.appData)
	}
}

// ending is how leg i of c ends by its outcome.
func (s *Switch) ending(c *call, i int) ending {
	b := i == 1
	pick := func(a, bEnd ending) ending {
		if b {
			return bEnd
		}
		return a
	}
	switch c.outcome {
	case normal:
		if c.kind == originate {
			return ending{"NORMAL_CLEARING", 200, "recv_bye", ""}
		}
		if c.callerHangs {
			return pick(ending{"NORMAL_CLEARING", 200, "recv_bye", ""}, ending{"NORMAL_CLEARING", 200, "send_bye", ""})
		}
		return pick(ending{"NORMAL_CLEARING", 200, "send_bye", ""}, ending{"NORMAL_CLEARING", 200, "recv_bye", ""})
	case mediaTimeout:
		return pick(ending{"MEDIA_TIMEOUT", 200, "send_bye", ""}, ending{"NORMAL_CLEARING", 200, "send_bye", ""})
	case cancel:
		return pick(ending{"ORIGINATOR_CANCEL", 487, "recv_cancel", ""}, ending{"ORIGINATOR_CANCEL", 487, "send_cancel", ""})
	case noAnswer:
		return pick(ending{"NO_ANSWER", 480, "send_refuse", ""}, ending{"NO_ANSWER", 487, "send_cancel", ""})
	case busy:
		return pick(ending{"USER_BUSY", 486, "send_refuse", ""}, ending{"USER_BUSY", 486, "recv_refuse", ""})
	case rejected:
		return pick(ending{"CALL_REJECTED", 603, "send_refuse", ""}, ending{"CALL_REJECTED", 603, "recv_refuse", ""})
	case incompatible:
		return pick(ending{"INCOMPATIBLE_DESTINATION", 488, "send_refuse", ""}, ending{"INCOMPATIBLE_DESTINATION", 488, "recv_refuse", ""})
	case timerExpired:
		return pick(ending{"RECOVERY_ON_TIMER_EXPIRE", 408, "send_refuse", ""}, ending{"RECOVERY_ON_TIMER_EXPIRE", 408, "recv_refuse", ""})
	case noRoute:
		return ending{"NO_ROUTE_DESTINATION", 404, "send_refuse", ""}
	case notRegistered:
		return ending{"NORMAL_CLEARING", 200, "recv_bye", "USER_NOT_REGISTERED"}
	}
	return ending{"NORMAL_CLEARING", 200, "send_bye", ""}
}

// killedBy is how a call ends that the console or a profile stop hangs up:
// the leg killed with cause, the others as FreeSWITCH then ends them.
func killedBy(target *channel, cause string) func(*channel) ending {
	return func(ch *channel) ending {
		answered := !ch.answered.IsZero()
		switch {
		case ch == target || cause == "MANAGER_REQUEST":
			if answered {
				return ending{cause, 200, "send_bye", ""}
			}
			return ending{cause, 0, "send_refuse", ""}
		case answered:
			return ending{"NORMAL_CLEARING", 200, "send_bye", ""}
		case ch.direction == "outbound":
			return ending{"ORIGINATOR_CANCEL", 487, "send_cancel", ""}
		}
		return ending{"NORMAL_CLEARING", 0, "send_refuse", ""}
	}
}

// end hangs up every leg of c, by its outcome or by why: the log lines and
// a CHANNEL_HANGUP_COMPLETE event per leg.
func (s *Switch) end(c *call, why func(*channel) ending) {
	if c.ended {
		return
	}
	c.ended = true
	for _, e := range c.pending {
		s.cancel(e)
	}
	c.pending = nil
	ends := make([]ending, len(c.legs))
	for i, ch := range c.legs {
		if why != nil {
			ends[i] = why(ch)
		} else {
			ends[i] = s.ending(c, i)
		}
	}
	a := c.legs[0]
	if c.outcome == mediaTimeout && why == nil {
		s.logf(levelWarning, srcMedia, a.uuid, "%s MEDIA TIMEOUT: no RTP received for 60 seconds", a.name)
	}
	// The leg that received the BYE or the refusal hangs up first.
	order := []int{0}
	if len(c.legs) > 1 {
		order = []int{0, 1}
		if !strings.HasPrefix(ends[0].disp, "recv") && strings.HasPrefix(ends[1].disp, "recv") {
			order = []int{1, 0}
		}
	}
	for _, i := range order {
		ch, e := c.legs[i], ends[i]
		if e.disp == "recv_bye" {
			s.sip(ch.profile, true, ch.proto, ch.ip, ch.port, "BYE sip:"+s.sp.ip+" SIP/2.0", ch.cidNum, ch.fromHost, ch.callID, "2 BYE", ch.agent)
		}
		s.logf(levelNotice, srcHangup, ch.uuid, "Hangup %s [%s] [%s]", ch.name, ch.state, e.cause)
	}
	for i, ch := range c.legs {
		if len(s.subs) > 0 {
			s.publish(esl.Message{Kind: esl.KindEvent, Headers: s.hangupHeaders(c, ch, ends[i])})
		}
	}
	for _, ch := range c.legs {
		s.logf(levelNotice, srcEnded, ch.uuid, "Session %d (%s) Ended", ch.session, ch.name)
		s.logf(levelNotice, srcClose, ch.uuid, "Close Channel %s [CS_DESTROY]", ch.name)
		delete(s.channels, ch.uuid)
		ok := !ch.answered.IsZero()
		if p := s.profile(ch.profile); p != nil {
			if ch.direction == "inbound" {
				p.callsIn++
				if !ok {
					p.failedIn++
				}
			} else {
				p.callsOut++
				if !ok {
					p.failedOut++
				}
			}
		}
		if g := s.gateway(ch.gateway); g != nil {
			if ch.direction == "inbound" {
				g.callsIn++
				if !ok {
					g.failedIn++
				}
			} else {
				g.callsOut++
				if !ok {
					g.failedOut++
				}
			}
		}
	}
}

// hangupHeaders is the CHANNEL_HANGUP_COMPLETE event of leg ch of c.
func (s *Switch) hangupHeaders(c *call, ch *channel, e ending) map[string]string {
	h := s.eventHeaders("CHANNEL_HANGUP_COMPLETE", "switch_core_state_machine.c", "switch_core_session_reporting_state", 946)
	us := func(t time.Time) string {
		if t.IsZero() {
			return "0"
		}
		return strconv.FormatInt(t.UnixMicro(), 10)
	}
	answered := !ch.answered.IsZero()
	for k, v := range map[string]string{
		"Hangup-Cause":                       e.cause,
		"Channel-State":                      "CS_REPORTING",
		"Channel-Call-State":                 "HANGUP",
		"Channel-State-Number":               "11",
		"Channel-Name":                       ch.name,
		"Unique-ID":                          ch.uuid,
		"Call-Direction":                     ch.direction,
		"Presence-Call-Direction":            ch.direction,
		"Channel-HIT-Dialplan":               strconv.FormatBool(ch.direction == "inbound"),
		"Channel-Presence-ID":                s.presence(ch),
		"Channel-Call-UUID":                  c.legs[0].uuid,
		"Answer-State":                       "hangup",
		"Caller-Direction":                   ch.direction,
		"Caller-Logical-Direction":           ch.direction,
		"Caller-Username":                    ch.cidNum,
		"Caller-Dialplan":                    "XML",
		"Caller-Caller-ID-Name":              ch.cidName,
		"Caller-Caller-ID-Number":            ch.cidNum,
		"Caller-Orig-Caller-ID-Name":         ch.cidName,
		"Caller-Orig-Caller-ID-Number":       ch.cidNum,
		"Caller-Callee-ID-Name":              ch.calleeName,
		"Caller-Callee-ID-Number":            ch.calleeNum,
		"Caller-Network-Addr":                ch.ip,
		"Caller-ANI":                         ch.cidNum,
		"Caller-Destination-Number":          ch.dest,
		"Caller-Unique-ID":                   ch.uuid,
		"Caller-Source":                      "mod_sofia",
		"Caller-Context":                     ch.context,
		"Caller-Channel-Name":                ch.name,
		"Caller-Profile-Index":               "1",
		"Caller-Profile-Created-Time":        us(ch.created),
		"Caller-Channel-Created-Time":        us(ch.created),
		"Caller-Channel-Answered-Time":       us(ch.answered),
		"Caller-Channel-Progress-Time":       "0",
		"Caller-Channel-Progress-Media-Time": us(ch.answered),
		"Caller-Channel-Hangup-Time":         us(s.t),
		"Caller-Channel-Transfer-Time":       "0",
		"Caller-Channel-Bridged-Time":        "0",
		"Caller-Screen-Bit":                  "true",
		"Caller-Privacy-Hide-Name":           "false",
		"Caller-Privacy-Hide-Number":         "false",
	} {
		h[k] = v
	}
	v := func(name, value string) { h["variable_"+name] = value }
	v("direction", ch.direction)
	v("uuid", ch.uuid)
	v("call_uuid", c.legs[0].uuid)
	v("session_id", strconv.Itoa(ch.session))
	v("channel_name", ch.name)
	v("sip_call_id", ch.callID)
	v("sip_from_user", strings.TrimPrefix(ch.cidNum, "+"))
	v("sip_from_host", ch.fromHost)
	v("sip_to_user", ch.dest)
	v("sip_local_network_addr", s.sp.ip)
	v("sip_network_ip", ch.ip)
	v("sip_network_port", strconv.Itoa(ch.port))
	v("sip_via_protocol", ch.proto)
	if ch.agent != "" {
		v("sip_user_agent", ch.agent)
	}
	if p := s.profile(ch.profile); p != nil {
		v("sofia_profile_name", p.name)
		v("sofia_profile_url", fmt.Sprintf("sip:mod_sofia@%s:%d", s.sp.ip, p.port))
		v("recovery_profile_name", p.name)
	}
	if ch.gateway != "" {
		v("sip_gateway_name", ch.gateway)
	}
	if ch.profile == "internal" && ch.direction == "inbound" {
		v("user_name", ch.cidNum)
		v("domain_name", s.sp.ip)
	}
	if ch.app != "" {
		v("current_application", ch.app)
		v("current_application_data", ch.appData)
	}
	v("hangup_cause", e.cause)
	v("hangup_cause_q850", strconv.Itoa(q850[e.cause]))
	v("sip_hangup_disposition", e.disp)
	if e.status > 0 {
		v("sip_term_status", strconv.Itoa(e.status))
		v("proto_specific_hangup_cause", "sip:"+strconv.Itoa(e.status))
	}
	if e.disp == "recv_refuse" {
		v("sip_invite_failure_status", strconv.Itoa(e.status))
	}
	if e.failed != "" {
		v("originate_failed_cause", e.failed)
	}
	if len(c.legs) > 1 {
		other := c.legs[1]
		h["Other-Type"] = "originatee"
		if ch == other {
			other = c.legs[0]
			h["Other-Type"] = "originator"
			v("signal_bond", other.uuid)
		} else {
			v("last_bridge_to", other.uuid)
			v("bridge_channel", other.name)
			if answered {
				v("bridge_uuid", other.uuid)
			} else {
				v("originate_disposition", e.cause)
			}
		}
		h["Other-Leg-Direction"] = other.direction
		h["Other-Leg-Unique-ID"] = other.uuid
		h["Other-Leg-Channel-Name"] = other.name
		h["Other-Leg-Caller-ID-Name"] = other.cidName
		h["Other-Leg-Caller-ID-Number"] = other.cidNum
		h["Other-Leg-Destination-Number"] = other.dest
	}

	duration := s.t.Sub(ch.created)
	billed := time.Duration(0)
	v("endpoint_disposition", "NO ANSWER")
	if answered {
		billed = s.t.Sub(ch.answered)
		v("endpoint_disposition", "ANSWER")
		h["Channel-Read-Codec-Name"], h["Channel-Read-Codec-Rate"] = ch.codec, strconv.Itoa(ch.rate)
		h["Channel-Write-Codec-Name"], h["Channel-Write-Codec-Rate"] = ch.codec, strconv.Itoa(ch.rate)
		v("read_codec", ch.codec)
		v("read_rate", strconv.Itoa(ch.rate))
		v("write_codec", ch.codec)
		v("write_rate", strconv.Itoa(ch.rate))
		v("rtp_use_codec_name", ch.codec)
		v("local_media_ip", s.sp.ip)
		v("remote_media_ip", ch.ip)
		packets := int(billed / (20 * time.Millisecond))
		quality := min(100, max(0, (ch.mos-1)/3.5*100))
		v("rtp_audio_in_mos", fmt.Sprintf("%.2f", ch.mos))
		v("rtp_audio_in_quality_percentage", fmt.Sprintf("%.2f", quality))
		v("rtp_audio_in_packet_count", strconv.Itoa(packets))
		v("rtp_audio_out_packet_count", strconv.Itoa(packets))
		v("rtp_audio_in_skip_packet_count", strconv.Itoa(int(float64(packets)*(100-quality)/400)))
		v("answer_stamp", ch.answered.UTC().Format(time.DateTime))
	}
	epoch := func(t time.Time) string {
		if t.IsZero() {
			return "0"
		}
		return strconv.FormatInt(t.Unix(), 10)
	}
	v("start_epoch", epoch(ch.created))
	v("start_stamp", ch.created.UTC().Format(time.DateTime))
	v("answer_epoch", epoch(ch.answered))
	v("end_epoch", epoch(s.t))
	v("end_stamp", s.t.UTC().Format(time.DateTime))
	v("duration", strconv.Itoa(int(duration/time.Second)))
	v("mduration", strconv.FormatInt(duration.Milliseconds(), 10))
	v("billsec", strconv.Itoa(int(billed/time.Second)))
	v("billmsec", strconv.FormatInt(billed.Milliseconds(), 10))
	if answered {
		v("answersec", strconv.Itoa(int(ch.answered.Sub(ch.created)/time.Second)))
		v("waitsec", strconv.Itoa(int(ch.answered.Sub(ch.created)/time.Second)))
	}
	return h
}

// prefill starts the simulation with about n channels of answered calls in
// progress, begun 15 to 150 seconds ago, without their first log lines.
func (s *Switch) prefill(n int) {
	for tries := 0; len(s.channels) < n && tries < n; tries++ {
		c := s.plan(normal)
		if c == nil {
			return
		}
		// At least 15 s ago, so the answer (at most 13 s in) is past.
		start := s.t.Add(-s.between(15*time.Second, 150*time.Second))
		state := "CS_EXECUTE"
		if len(c.legs) > 1 {
			state = "CS_EXCHANGE_MEDIA"
		}
		for _, ch := range c.legs {
			s.sessionN++
			ch.session, ch.created, ch.answered, ch.state, ch.callstate = s.sessionN, start, start.Add(c.answer), state, "ACTIVE"
			s.channels[ch.uuid] = ch
		}
		c.ring, c.answer, c.end = 0, 0, s.between(5*time.Second, 175*time.Second)
		s.schedule(c)
	}
	s.peak = len(s.channels)
}

// originateCall starts a call of one leg from the console to the device or
// number of dial and answers +OK with its uuid, or -ERR with the cause.
func (s *Switch) originateCall(dial, app, appData string) string {
	// Channel variables in front of the endpoint: {a=b}[c=d]user/1001
	for len(dial) > 0 && (dial[0] == '{' || dial[0] == '[' || dial[0] == '<') {
		closer := map[byte]string{'{': "}", '[': "]", '<': ">"}[dial[0]]
		i := strings.Index(dial, closer)
		if i < 0 {
			return "-ERR INVALID_URL\n"
		}
		dial = dial[i+1:]
	}
	if s.paused {
		return "-ERR SYSTEM_SHUTDOWN\n"
	}
	if len(s.channels)+1 > s.limit() {
		return "-ERR SWITCH_CONGESTION\n"
	}
	cidName, cidNum := s.values["outbound_caller_name"], s.values["outbound_caller_id"]
	ch := &channel{direction: "outbound", cidName: cidName, cidNum: cidNum, fromHost: s.sp.ip, context: "default", app: app, appData: appData}
	switch rest, ok := strings.CutPrefix(dial, "user/"); {
	case ok:
		user, _, _ := strings.Cut(rest, "@")
		var dev *registration
		for _, r := range s.regs {
			if r.user == user && s.visible(r) {
				dev = r
				break
			}
		}
		known := false
		for _, u := range users {
			known = known || u.id == user
		}
		switch {
		case !known:
			return "-ERR SUBSCRIBER_ABSENT\n"
		case dev == nil:
			return "-ERR USER_NOT_REGISTERED\n"
		}
		leg := s.deviceLeg(dev, "outbound")
		ch.name, ch.profile, ch.ip, ch.port, ch.proto, ch.agent = leg.name, leg.profile, leg.ip, leg.port, leg.proto, leg.agent
		ch.dest, ch.calleeName, ch.calleeNum = user, userName(user), user
	case strings.HasPrefix(dial, "sofia/gateway/"):
		name, num, _ := strings.Cut(strings.TrimPrefix(dial, "sofia/gateway/"), "/")
		g := s.gateway(name)
		switch {
		case g == nil || g.killed:
			return "-ERR INVALID_GATEWAY\n"
		case !g.usable() || !s.profile("external").running:
			return "-ERR GATEWAY_DOWN\n"
		case num == "":
			return "-ERR INVALID_URL\n"
		}
		ch.name, ch.profile, ch.gateway, ch.ip, ch.port, ch.proto, ch.agent = "sofia/external/"+num, "external", g.name, g.ip, 5060, "udp", g.agent
		ch.dest, ch.calleeName, ch.calleeNum = num, num, num
	case strings.HasPrefix(dial, "loopback/"):
		ch.name, ch.dest = dial+"-a", strings.TrimPrefix(dial, "loopback/")
		ch.calleeName, ch.calleeNum = ch.dest, ch.dest
	default:
		return "-ERR CHAN_NOT_IMPLEMENTED\n"
	}
	c := &call{kind: originate, outcome: normal, legs: []*channel{ch}}
	ch.call, ch.codec, ch.rate, ch.mos = c, "PCMU", 8000, 3.9+0.5*s.rng.Float64()
	if ch.proto == "wss" {
		ch.codec, ch.rate = "OPUS", 48000
	}
	ch.uuid, ch.callID = s.uuid(), s.token()
	c.ring = s.between(300*time.Millisecond, time.Second)
	c.answer = c.ring + s.between(time.Second, 4*time.Second)
	c.end = c.answer + s.between(20*time.Second, 180*time.Second)
	ch.state = "CS_EXECUTE"
	s.open(ch)
	s.sip(ch.profile, false, ch.proto, ch.ip, ch.port, "INVITE sip:"+ch.dest+"@"+ch.ip+" SIP/2.0", strings.TrimPrefix(cidNum, "+"), s.sp.ip, ch.callID, "1 INVITE", "")
	s.schedule(c)
	return "+OK " + ch.uuid + "\n"
}
