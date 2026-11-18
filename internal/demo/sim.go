package demo

import (
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/juki/freeswitch-console/internal/esl"
)

type profile struct {
	name     string
	port     int
	wss      int // the secure WebSocket port; 0 for none
	context  string
	running  bool
	siptrace bool
	trace    *event // the next OPTIONS of the trace

	callsIn, failedIn, callsOut, failedOut int
}

type gateway struct {
	gatewaySpec
	state, status                          string
	killed                                 bool
	upSince                                time.Time
	callsIn, failedIn, callsOut, failedOut int
}

// usable reports whether calls can go through g.
func (g *gateway) usable() bool {
	return !g.killed && g.status == "UP" && g.state != "UNREGED"
}

type registration struct {
	regSpec
	callID  string
	until   time.Time // when it expires
	flushed bool      // by flush_inbound_reg, until the change is undone
}

type module struct {
	name   string
	ifaces [][2]string // type, name; the api ones are the simulated commands, added at setup
	loaded bool
	keep   bool // the simulation needs it, so it cannot be unloaded
}

// catalog lists the modules of the sample configuration (modules.conf.xml),
// loaded, then some that are installed but not loaded.
func catalog() []*module {
	m := func(name string, loaded, keep bool, ifaces ...string) *module {
		mod := &module{name: name, loaded: loaded, keep: keep}
		for i := 0; i+1 < len(ifaces); i += 2 {
			mod.ifaces = append(mod.ifaces, [2]string{ifaces[i], ifaces[i+1]})
		}
		return mod
	}
	return []*module{
		m("mod_console", true, true),
		m("mod_logfile", true, false),
		m("mod_event_socket", true, true, "application", "socket"),
		m("mod_sofia", true, true, "endpoint", "sofia", "endpoint", "rtp", "chat", "sip"),
		m("mod_loopback", true, false, "endpoint", "loopback", "endpoint", "null", "application", "unloop"),
		m("mod_commands", true, true),
		m("mod_dptools", true, true, "application", "answer", "application", "bridge", "application", "echo", "application", "hangup",
			"application", "park", "application", "playback", "application", "set", "application", "transfer", "dialplan", "inline", "endpoint", "user", "endpoint", "error"),
		m("mod_db", true, false, "application", "db", "limit", "db"),
		m("mod_hash", true, false, "application", "hash", "limit", "hash"),
		m("mod_conference", true, false, "application", "conference"),
		m("mod_voicemail", true, false, "application", "voicemail"),
		m("mod_fifo", true, false, "application", "fifo"),
		m("mod_valet_parking", true, false, "application", "valet_park"),
		m("mod_expr", true, false, "application", "expr"),
		m("mod_dialplan_xml", true, true, "dialplan", "XML"),
		m("mod_opus", true, false, "codec", "OPUS"),
		m("mod_sndfile", true, false, "file", "wav", "file", "flac", "file", "ogg"),
		m("mod_native_file", true, false, "file", "PCMU", "file", "PCMA"),
		m("mod_tone_stream", true, false, "file", "tone_stream", "file", "silence_stream"),
		m("mod_local_stream", true, false, "file", "local_stream"),
		m("mod_lua", true, false, "application", "lua", "dialplan", "LUA"),
		m("mod_say_en", true, false, "say", "en"),
		m("mod_xml_cdr", false, false, "application", "xml_cdr_log"),
		m("mod_callcenter", false, false, "application", "callcenter"),
		m("mod_spandsp", false, false, "application", "rxfax", "application", "txfax", "codec", "G722"),
	}
}

// coreRows are the interfaces of the core, which no module owns.
var coreRows = [][4]string{
	{"codec", "PCMU", "CORE_PCM_MODULE", ""},
	{"codec", "PCMA", "CORE_PCM_MODULE", ""},
	{"timer", "soft", "CORE_SOFTTIMER_MODULE", ""},
}

// setup builds the state at the start: the configuration, the devices
// registered some time ago, calls in progress, and the generators.
func (s *Switch) setup() {
	sp := s.sp
	s.boot = s.t.Add(-sp.uptime)
	s.changes = map[string]*change{}
	s.channels = map[string]*channel{}
	s.coreUUID = s.uuid()
	s.sessionN = sp.since
	s.coreLevel, s.maxSessions, s.sps = 7, 1000, 30
	s.idle = 99.2

	conf, err := loadConf([][2]string{
		{"hostname", sp.name}, {"local_ip_v4", sp.ip}, {"local_mask_v4", "255.255.255.0"}, {"local_ip_v6", "::1"},
		{"switch_serial", fmt.Sprintf("%010d", s.rng.Int63n(1e10))}, {"core_uuid", s.coreUUID},
		{"base_dir", "/usr"}, {"conf_dir", "/etc/freeswitch"}, {"log_dir", "/var/log/freeswitch"}, {"run_dir", "/var/run/freeswitch"},
		{"db_dir", "/var/lib/freeswitch/db"}, {"mod_dir", "/usr/lib/freeswitch/mod"}, {"sounds_dir", "/usr/share/freeswitch/sounds"},
		{"recordings_dir", "/var/lib/freeswitch/recordings"}, {"storage_dir", "/var/lib/freeswitch/storage"}, {"nat_type", "none"},
	})
	if err != nil {
		panic("demo: the embedded configuration: " + err.Error())
	}
	s.conf = conf
	s.values = map[string]string{}
	for _, kv := range conf.vars {
		if _, ok := s.values[kv[0]]; !ok {
			s.vars = append(s.vars, kv[0])
		}
		s.values[kv[0]] = kv[1]
	}

	s.profiles = []*profile{
		{name: "internal", port: 5060, wss: 7443, context: "default", running: true},
		{name: "external", port: 5080, context: "public", running: true},
	}
	for _, name := range sp.gateways {
		for _, gs := range gatewaySpecs {
			if gs.name != name {
				continue
			}
			g := &gateway{gatewaySpec: gs}
			s.resetGateway(g)
			g.upSince = s.boot.Add(time.Duration(30+s.rng.Intn(600)) * time.Second)
			g.callsIn = int(float64(sp.since) * (0.2 + 0.1*s.rng.Float64()))
			g.callsOut = int(float64(sp.since) * (0.1 + 0.05*s.rng.Float64()))
			g.failedIn, g.failedOut = g.callsIn/12, g.callsOut/9
			if !gs.up {
				g.callsIn, g.callsOut, g.failedIn, g.failedOut = 0, 0, 0, 0
			}
			s.gateways = append(s.gateways, g)
		}
	}

	s.modules = catalog()
	for i := range commandTable {
		for _, mod := range s.modules {
			if mod.name == commandTable[i].module {
				mod.ifaces = append(mod.ifaces, [2]string{"api", commandTable[i].name})
			}
		}
	}

	for _, rs := range sp.regs {
		r := &registration{regSpec: rs, callID: s.token()}
		interval := time.Duration(rs.expires) * time.Second
		ago := time.Duration(s.rng.Int63n(int64(interval * 8 / 10)))
		r.until = s.t.Add(interval - ago)
		s.regs = append(s.regs, r)
		s.after(max(time.Second, interval*9/10-ago), func() { s.refresh(r) })
	}

	since := s.t.Sub(s.boot) % heartbeatEvery
	s.after(heartbeatEvery-since, s.heartbeat)
	if g := s.gateway("carrier-backup"); g != nil {
		s.after(time.Duration(1+s.rng.Intn(29))*time.Second, func() { s.ping(g) })
	}
	s.after(s.errorGap(), s.scanner)
	s.after(s.expGap(), s.arrival)
	s.prefill(sp.busy)
	s.setupSip()
}

// resetGateway sets the state the configuration gives g.
func (s *Switch) resetGateway(g *gateway) {
	g.killed = false
	switch {
	case !g.register:
		g.state = "NOREG"
	case g.up:
		g.state = "REGED"
	default:
		g.state = "FAIL_WAIT"
	}
	g.status = "DOWN"
	if g.up {
		g.status = "UP"
	}
}

func (s *Switch) gateway(name string) *gateway {
	for _, g := range s.gateways {
		if g.name == name {
			return g
		}
	}
	return nil
}

func (s *Switch) profile(name string) *profile {
	for _, p := range s.profiles {
		if p.name == name {
			return p
		}
	}
	return nil
}

func (s *Switch) module(name string) *module {
	for _, m := range s.modules {
		if m.name == name {
			return m
		}
	}
	return nil
}

func (s *Switch) loaded(name string) bool {
	m := s.module(name)
	return m != nil && m.loaded
}

// visible reports whether r is listed: registered on a running profile and
// not flushed.
func (s *Switch) visible(r *registration) bool {
	return !r.flushed && s.profile("internal").running
}

// limit is the most channels the switch accepts.
func (s *Switch) limit() int {
	return min(maxChannels, s.maxSessions)
}

// uuid returns a version 4 uuid from the simulation's random source.
func (s *Switch) uuid() string {
	var b [16]byte
	for i := range b {
		b[i] = byte(s.rng.Intn(256))
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// token is a SIP Call-ID.
func (s *Switch) token() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 22)
	for i := range b {
		b[i] = chars[s.rng.Intn(len(chars))]
	}
	return string(b)
}

// between returns a duration from lo to hi.
func (s *Switch) between(lo, hi time.Duration) time.Duration {
	return lo + time.Duration(s.rng.Int63n(int64(hi-lo)+1))
}

// expGap is the time to the next call: exponential around the spec's mean.
func (s *Switch) expGap() time.Duration {
	return max(100*time.Millisecond, time.Duration(s.rng.ExpFloat64()*float64(s.sp.gap)))
}

// errorGap is the time to the next failed registration attempt from outside.
func (s *Switch) errorGap() time.Duration {
	return s.between(6*time.Minute, 14*time.Minute)
}

// arrival starts a call now and then, more often while the switch is below
// its range of channels and none when the next would take it above.
func (s *Switch) arrival() {
	gap := s.expGap()
	n := len(s.channels)
	if n < s.sp.sessions[0] {
		gap /= 4
	}
	s.after(gap, s.arrival)
	if !s.paused && n+2 <= s.sp.sessions[1] {
		s.newCall()
	}
}

// refresh renews the registration r, as its device does before it expires.
func (s *Switch) refresh(r *registration) {
	interval := time.Duration(r.expires) * time.Second
	s.after(interval*85/100+s.between(0, interval/10), func() { s.refresh(r) })
	if !s.visible(r) {
		return
	}
	r.until = s.t.Add(interval)
	s.registered(r)
}

// registered logs that r has registered, with its SIP exchange when the
// profile is traced, and publishes its sofia::register event.
func (s *Switch) registered(r *registration) {
	s.logf(levelInfo, srcRegister, "", "Registered %s@%s from %s/%s:%d, expires in %ds (%s)", r.user, s.sp.ip, r.proto, r.ip, r.port, r.expires, r.agent)
	s.sip("internal", true, r.proto, r.ip, r.port, "REGISTER sip:"+s.sp.ip+" SIP/2.0", r.user, s.sp.ip, r.callID, "2 REGISTER", r.agent)
	s.sip("internal", false, r.proto, r.ip, r.port, "SIP/2.0 200 OK", r.user, s.sp.ip, r.callID, "2 REGISTER", "")
	s.publishSip(s.registerEvent(r, r.expires))
}

// ping fails for carrier-backup, which is unreachable, every 30 seconds.
func (s *Switch) ping(g *gateway) {
	s.after(30*time.Second, func() { s.ping(g) })
	if g.killed || g.status != "DOWN" || !s.profile("external").running {
		return
	}
	s.logf(levelWarning, srcPing, "", "Ping failed %s with code 408 - count 0/1/0, state DOWN", g.name)
}

// scanner is one of the attackers trying passwords now and then: a burst of
// failed registrations, one user after another.
func (s *Switch) scanner() {
	s.after(s.errorGap(), s.scanner)
	who, n := attackers[s.rng.Intn(len(attackers))], 6+s.rng.Intn(25)
	if !s.profile("internal").running {
		return
	}
	for _, a := range burst(s.sipRng, who, n) {
		s.after(a.after, func() { s.authFailure(a) })
	}
}

// authFailure logs and publishes one failed registration of a burst.
func (s *Switch) authFailure(a attempt) {
	if !s.profile("internal").running {
		return
	}
	s.logf(levelWarning, srcAuth, "", "SIP auth failure (REGISTER) on sofia profile 'internal' for [%s@%s] from ip %s", a.user, s.sp.ip, a.ip)
	if !directoryUser(a.user) {
		s.logf(levelErr, srcFindUser, "", "Can't find user [%s@%s] from %s", a.user, s.sp.ip, a.ip)
	}
	s.publishSip(s.failureEvent(a))
}

// heartbeat publishes a HEARTBEAT event every 20 seconds of uptime.
func (s *Switch) heartbeat() {
	s.after(heartbeatEvery, s.heartbeat)
	n := len(s.channels)
	s.idle = idleCPU(n, s.rng)
	s.peaks[s.beats%len(s.peaks)] = s.peakSince
	s.ratePeaks[s.beats%len(s.ratePeaks)] = s.rateSince
	s.beats++
	s.peakSince, s.rateSince = n, s.ratePerSec()
	if len(s.subs) == 0 {
		return
	}
	h := s.eventHeaders("HEARTBEAT", "switch_core.c", "send_heartbeat", 77)
	y, d, hr, mi, se, ms, us := s.uptime()
	h["Event-Info"] = "System Ready"
	h["Up-Time"] = fmt.Sprintf("%d years, %d days, %d hours, %d minutes, %d seconds, %d milliseconds, %d microseconds", y, d, hr, mi, se, ms, us)
	h["FreeSWITCH-Version"] = s.sp.version + "-release~64bit"
	h["Uptime-msec"] = strconv.FormatInt(s.t.Sub(s.boot).Milliseconds(), 10)
	h["Heartbeat-Interval"] = "20"
	h["Session-Count"] = strconv.Itoa(n)
	h["Max-Sessions"] = strconv.Itoa(s.maxSessions)
	h["Session-Per-Sec"] = strconv.Itoa(s.sps)
	h["Session-Per-Sec-Last"] = strconv.Itoa(s.ratePerSec())
	h["Session-Per-Sec-Max"] = strconv.Itoa(s.ratePeak)
	h["Session-Per-Sec-FiveMin"] = strconv.Itoa(s.ratePeak5())
	h["Session-Since-Startup"] = strconv.Itoa(s.sessionN)
	h["Session-Peak-Max"] = strconv.Itoa(s.peak)
	h["Session-Peak-FiveMin"] = strconv.Itoa(s.peak5())
	h["Idle-CPU"] = fmt.Sprintf("%.6f", s.idle)
	s.publish(esl.Message{Kind: esl.KindEvent, Headers: h})
}

// idleCPU is the idle CPU, percent, with n channels in progress.
func idleCPU(n int, rng *rand.Rand) float64 {
	return 99.4 - 0.045*float64(n) - 0.4*rng.Float64()
}

// historyBeats is the number of heartbeats History covers: two hours.
const historyBeats = 360

// History calls add for each heartbeat of the last two hours, oldest first,
// with the load the switch would have reported: Session-Count,
// Session-Per-Sec-Last and Idle-CPU. The channels follow a working day
// squeezed into the two hours, a morning and a smaller afternoon peak within
// the switch's range, with some noise, and end at the channels in progress
// now, so the next live heartbeat carries on from the last one. The result
// depends only on the seed and the state now. A switch that does not
// connect has no history.
func (s *Switch) History(add func(at time.Time, sessions, rate int, idle float64)) {
	s.mu.Lock()
	if s.rng == nil || s.closed {
		s.mu.Unlock()
		return
	}
	sp, seed, now := s.sp, s.seed, len(s.channels)
	last := s.t.Add(-(s.t.Sub(s.boot) % heartbeatEvery)) // the heartbeat before the next one
	s.mu.Unlock()

	rng := rand.New(rand.NewSource(seed))
	lo, hi := float64(sp.sessions[0]), float64(sp.sessions[1])
	bump := func(x, at, width float64) float64 { return math.Exp(-(x - at) * (x - at) / (width * width)) }
	day := func(x float64) float64 { return lo + (hi-lo)*(0.1+0.8*bump(x, 0.3, 0.13)+0.6*bump(x, 0.7, 0.12)) }
	// The noise drifts as calls come and go: each value keeps most of the
	// one before.
	noise := make([]float64, historyBeats)
	sd := 0.07 * (hi - lo)
	for i := range noise {
		if i > 0 {
			noise[i] = 0.8 * noise[i-1]
		}
		noise[i] += 0.6 * sd * rng.NormFloat64()
	}
	// Bent over the last part of the day onto the channels now.
	gap := float64(now) - day(1) - noise[historyBeats-1]
	for i := range historyBeats {
		x := float64(i) / (historyBeats - 1)
		v := day(x) + noise[i] + gap*math.Pow(x, 6)
		n := min(max(int(math.Round(v)), min(sp.sessions[0], now)), max(sp.sessions[1], now))
		// Sessions created in the last second: about as often as n
		// channels of calls of a minute or two need.
		rate := 0
		if rng.Float64() < float64(n)/160 {
			rate = 1 + rng.Intn(2)
		}
		add(last.Add(-time.Duration(historyBeats-1-i)*heartbeatEvery), n, rate, idleCPU(n, rng))
	}
}

// created counts a new channel for the session totals and rates.
func (s *Switch) created() {
	s.sessionN++
	if sec := s.t.Unix(); sec != s.rateSec {
		s.rateLast = 0
		if sec == s.rateSec+1 {
			s.rateLast = s.rateNow
		}
		s.rateSec, s.rateNow = sec, 0
	}
	s.rateNow++
	s.ratePeak = max(s.ratePeak, s.rateNow)
	s.rateSince = max(s.rateSince, s.rateNow)
	n := len(s.channels)
	s.peak = max(s.peak, n)
	s.peakSince = max(s.peakSince, n)
}

// ratePerSec is the number of sessions created in the last whole second.
func (s *Switch) ratePerSec() int {
	switch s.t.Unix() {
	case s.rateSec:
		return s.rateLast
	case s.rateSec + 1:
		return s.rateNow
	}
	return 0
}

func (s *Switch) peak5() int {
	p := max(s.peakSince, len(s.channels))
	for _, v := range s.peaks {
		p = max(p, v)
	}
	return p
}

func (s *Switch) ratePeak5() int {
	p := s.rateSince
	for _, v := range s.ratePeaks {
		p = max(p, v)
	}
	return p
}

// uptime splits the time since boot as FreeSWITCH reports it.
func (s *Switch) uptime() (years, days, hours, minutes, seconds, ms, us int) {
	d := s.t.Sub(s.boot)
	us = int(d.Microseconds() % 1000)
	ms = int(d.Milliseconds() % 1000)
	secs := int(d / time.Second)
	years, secs = secs/(365*86400), secs%(365*86400)
	days, secs = secs/86400, secs%86400
	hours, secs = secs/3600, secs%3600
	return years, days, hours, secs / 60, secs % 60, ms, us
}

// eventHeaders are the headers every event carries.
func (s *Switch) eventHeaders(name, file, fn string, line int) map[string]string {
	s.eventSeq++
	return s.headersAt(name, file, fn, line, s.t, s.eventSeq)
}

// headersAt are the headers of an event at t with the sequence number seq.
func (s *Switch) headersAt(name, file, fn string, line int, at time.Time, seq uint64) map[string]string {
	t := at.UTC()
	return map[string]string{
		"Event-Name":                name,
		"Core-UUID":                 s.coreUUID,
		"FreeSWITCH-Hostname":       s.sp.name,
		"FreeSWITCH-Switchname":     s.sp.name,
		"FreeSWITCH-IPv4":           s.sp.ip,
		"FreeSWITCH-IPv6":           "::1",
		"Event-Date-Local":          t.Format(time.DateTime),
		"Event-Date-GMT":            t.Format(http.TimeFormat),
		"Event-Date-Timestamp":      strconv.FormatInt(t.UnixMicro(), 10),
		"Event-Calling-File":        file,
		"Event-Calling-Function":    fn,
		"Event-Calling-Line-Number": strconv.Itoa(line),
		"Event-Sequence":            strconv.FormatUint(seq, 10),
	}
}

// Log levels.
const (
	levelConsole = iota
	levelAlert
	levelCrit
	levelErr
	levelWarning
	levelNotice
	levelInfo
	levelDebug
)

var levelNames = [...]string{"CONSOLE", "ALERT", "CRIT", "ERR", "WARNING", "NOTICE", "INFO", "DEBUG"}

// source is where in FreeSWITCH a log line comes from.
type source struct {
	file, fn string
	line     int
}

var (
	srcNewChannel = source{"switch_channel.c", "switch_channel_set_name", 1142}
	srcState      = source{"sofia.c", "sofia_handle_sip_i_state", 7672}
	srcProcessing = source{"mod_dialplan_xml.c", "dialplan_hunt", 639}
	srcDialplan   = source{"mod_dialplan_xml.c", "parse_exten", 408}
	srcExecute    = source{"switch_core_session.c", "switch_core_session_exec", 2970}
	srcRingReady  = source{"switch_channel.c", "switch_channel_perform_mark_ring_ready_value", 3986}
	srcAnswered   = source{"switch_channel.c", "switch_channel_perform_mark_answered", 3758}
	srcHangup     = source{"switch_channel.c", "switch_channel_perform_hangup", 3535}
	srcEnded      = source{"switch_core_session.c", "switch_core_session_perform_destroy", 1773}
	srcClose      = source{"switch_core_session.c", "switch_core_session_perform_destroy", 1745}
	srcOriginate  = source{"switch_ivr_originate.c", "switch_ivr_originate", 4026}
	srcBridgeFail = source{"mod_dptools.c", "audio_bridge_function", 3638}
	srcTransfer   = source{"switch_ivr.c", "switch_ivr_session_transfer", 2266}
	srcNoRoute    = source{"switch_core_state_machine.c", "switch_core_standard_on_routing", 318}
	srcMedia      = source{"switch_core_media.c", "switch_core_media_read_frame", 3021}
	srcRegister   = source{"sofia_reg.c", "sofia_reg_handle_register_token", 1871}
	srcPing       = source{"sofia_reg.c", "sofia_reg_handle_sip_r_options", 3104}
	srcAuth       = source{"sofia_reg.c", "sofia_reg_parse_auth", 2985}
	srcFindUser   = source{"sofia_reg.c", "sofia_reg_parse_auth", 2924}
	srcProfile    = source{"sofia.c", "sofia_profile_thread_run", 3437}
	srcModule     = source{"switch_loadable_module.c", "switch_loadable_module_load_file", 1772}
	srcXML        = source{"switch_xml.c", "switch_xml_open_root", 2445}
	srcTrace      = source{"sofia.c", "logger", 4567}
)

// logf publishes a log line as FreeSWITCH formats it, when the core level
// and the level SetLogLevel asked for let it through; uuid is the channel it
// is about, or "".
func (s *Switch) logf(level int, src source, uuid, format string, args ...any) {
	if level > s.eslLevel || level > s.coreLevel || len(s.subs) == 0 {
		return
	}
	var b strings.Builder
	if uuid != "" {
		b.WriteString(uuid)
		b.WriteByte(' ')
	}
	b.WriteString(s.t.UTC().Format("2006-01-02 15:04:05.000000"))
	fmt.Fprintf(&b, " %.2f%% [%s] %s:%d ", s.idle, levelNames[level], src.file, src.line)
	fmt.Fprintf(&b, format, args...)
	b.WriteByte('\n')
	s.publishLog(level, src, uuid, b.String())
}

func (s *Switch) publishLog(level int, src source, uuid, text string) {
	s.publish(esl.Message{Kind: esl.KindLog, Body: text, Headers: map[string]string{
		"Content-Type":   "log/data",
		"Content-Length": strconv.Itoa(len(text)),
		"Log-Level":      strconv.Itoa(level),
		"Text-Channel":   "3",
		"Log-File":       src.file,
		"Log-Func":       src.fn,
		"Log-Line":       strconv.Itoa(src.line),
		"User-Data":      uuid,
	}})
}

// sip logs one SIP message of a traced profile at CONSOLE level, as sofia's
// siptrace prints it: recv for a message from the peer, send for one to it.
func (s *Switch) sip(profileName string, recv bool, proto, ip string, port int, first, user, host, callID, cseq, agent string) {
	p := s.profile(profileName)
	if p == nil || !p.siptrace || levelConsole > s.eslLevel || len(s.subs) == 0 {
		return
	}
	s.eventSeq++
	via := strings.ToUpper(proto)
	lines := []string{
		first,
		fmt.Sprintf("Via: SIP/2.0/%s %s:%d;branch=z9hG4bK%08x;rport", via, ip, port, s.eventSeq),
		fmt.Sprintf("From: <sip:%s@%s>;tag=%06x", user, host, s.eventSeq%0xffffff),
		fmt.Sprintf("To: <sip:%s@%s>", user, host),
		"Call-ID: " + callID,
		"CSeq: " + cseq,
	}
	if agent != "" {
		lines = append(lines, "User-Agent: "+agent)
	} else {
		lines = append(lines, "Server: FreeSWITCH-mod_sofia/"+s.sp.version+"-release~64bit")
	}
	lines = append(lines, "Content-Length: 0")
	msg := strings.Join(lines, "\r\n") + "\r\n\r\n"
	verb, dir := "send", "to"
	if recv {
		verb, dir = "recv", "from"
	}
	const rule = "   ------------------------------------------------------------------------\n"
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d bytes %s %s/%s:%d at %s:\n", verb, len(msg), dir, proto, ip, port, s.t.UTC().Format("15:04:05.000000"))
	b.WriteString(rule)
	for _, l := range lines {
		b.WriteString("   " + l + "\n")
	}
	b.WriteString(rule)
	s.publishLog(levelConsole, srcTrace, "", b.String())
}

// traceOptions is a device's keep-alive while the profile is traced, so a
// trace shows messages even on a quiet switch.
func (s *Switch) traceOptions(p *profile) {
	if !p.siptrace || !p.running {
		return
	}
	p.trace = s.after(15*time.Second, func() { s.traceOptions(p) })
	var r *registration
	for _, reg := range s.regs {
		if s.visible(reg) && (r == nil || s.rng.Intn(3) == 0) {
			r = reg
		}
	}
	if p.name != "internal" || r == nil {
		g := s.usableGateway()
		if g == nil {
			return
		}
		s.sip(p.name, false, "udp", g.ip, 5060, "OPTIONS sip:"+g.proxy+" SIP/2.0", g.username, g.proxy, s.token(), "1 OPTIONS", "")
		s.sip(p.name, true, "udp", g.ip, 5060, "SIP/2.0 200 OK", g.username, g.proxy, "ka-"+g.name, "1 OPTIONS", g.agent)
		return
	}
	s.sip(p.name, true, r.proto, r.ip, r.port, "OPTIONS sip:"+s.sp.ip+" SIP/2.0", r.user, s.sp.ip, r.callID, "1 OPTIONS", r.agent)
	s.sip(p.name, false, r.proto, r.ip, r.port, "SIP/2.0 200 OK", r.user, s.sp.ip, r.callID, "1 OPTIONS", "")
}

// usableGateway returns the first gateway calls can go through, or nil.
func (s *Switch) usableGateway() *gateway {
	for _, g := range s.gateways {
		if g.usable() {
			return g
		}
	}
	return nil
}
