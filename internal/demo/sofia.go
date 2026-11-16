package demo

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const sofiaUsage = "-USAGE: sofia status [profile <name>|gateway <name>] | xmlstatus [gateway [<name>]] | " +
	"profile <name> start|stop|restart|rescan|flush_inbound_reg|siptrace on|off|killgw <gateway>|register <gateway>|unregister <gateway>\n"

const statusRule = "=================================================================================================\n"

func (s *Switch) cmdSofia(arg string) string {
	f := strings.Fields(arg)
	switch {
	case len(f) == 1 && f[0] == "status":
		return s.sofiaStatus()
	case len(f) == 3 && f[0] == "status" && f[1] == "profile":
		return s.profileStatus(f[2])
	case len(f) == 3 && f[0] == "status" && f[1] == "gateway":
		return s.gatewayStatus(f[2])
	case len(f) == 1 && f[0] == "xmlstatus":
		return s.sofiaXML()
	case len(f) == 2 && f[0] == "xmlstatus" && f[1] == "gateway":
		var b strings.Builder
		b.WriteString("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n<gateways>\n")
		for _, g := range s.listedGateways() {
			b.WriteString(s.gatewayXML(g, "  "))
		}
		b.WriteString("</gateways>\n")
		return b.String()
	case len(f) == 3 && f[0] == "xmlstatus" && f[1] == "gateway":
		for _, g := range s.listedGateways() {
			if g.name == f[2] {
				return "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n" + s.gatewayXML(g, "")
			}
		}
		return "-ERR Invalid Gateway!\n"
	case len(f) >= 3 && f[0] == "profile":
		return s.profileCommand(f[1], f[2], f[3:])
	}
	return sofiaUsage
}

// listedGateways are the gateways sofia shows: those of a running profile
// that are not killed.
func (s *Switch) listedGateways() []*gateway {
	if !s.profile("external").running {
		return nil
	}
	var out []*gateway
	for _, g := range s.gateways {
		if !g.killed {
			out = append(out, g)
		}
	}
	return out
}

// inUse counts the channels of profile p.
func (s *Switch) inUse(p *profile) int {
	n := 0
	for _, ch := range s.channels {
		if ch.profile == p.name {
			n++
		}
	}
	return n
}

// statusRow is one line of sofia status.
type statusRow struct{ name, typ, data, state, retry string }

func (s *Switch) statusRows() []statusRow {
	var rows []statusRow
	for _, p := range s.profiles {
		if !p.running {
			continue
		}
		n := s.inUse(p)
		rows = append(rows, statusRow{name: p.name, typ: "profile", data: fmt.Sprintf("sip:mod_sofia@%s:%d", s.sp.ip, p.port), state: fmt.Sprintf("RUNNING (%d)", n)})
		if p.wss > 0 {
			rows = append(rows, statusRow{name: p.name, typ: "profile", data: fmt.Sprintf("sip:mod_sofia@%s:%d;transport=wss", s.sp.ip, p.wss), state: fmt.Sprintf("RUNNING (%d) (WSS)", n)})
		}
		if p.name == "external" {
			for _, g := range s.listedGateways() {
				row := statusRow{name: g.name, typ: "gateway", data: "sip:" + g.proxy, state: g.state}
				if g.state == "FAIL_WAIT" {
					row.retry = s.retry()
				}
				rows = append(rows, row)
			}
		}
	}
	if s.profile("internal").running {
		rows = append(rows, statusRow{name: s.sp.ip, typ: "alias", data: "internal", state: "ALIASED"})
	}
	return rows
}

// retry is the time to the next registration attempt of a failed gateway,
// which tries every 60 seconds.
func (s *Switch) retry() string {
	return strconv.Itoa(60-int(s.t.Sub(s.boot)/time.Second)%60) + "s"
}

func (s *Switch) sofiaStatus() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%25s\t%s\t%40s\t%s\n", "Name", "   Type", "Data", "State")
	b.WriteString(statusRule)
	profiles, aliases := 0, 0
	for _, r := range s.statusRows() {
		name := r.name
		switch r.typ {
		case "gateway":
			name = "external::" + r.name
		case "profile":
			profiles++
		case "alias":
			aliases++
		}
		state := r.state
		if r.retry != "" {
			state += " (retry: " + r.retry + ")"
		}
		fmt.Fprintf(&b, "%25s\t%s\t%40s\t%s\n", name, r.typ, r.data, state)
	}
	b.WriteString(statusRule)
	fmt.Fprintf(&b, "%d profiles %d alias\n", profiles, aliases)
	return b.String()
}

func (s *Switch) sofiaXML() string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n<profiles>\n")
	for _, r := range s.statusRows() {
		fmt.Fprintf(&b, "<%s>\n<name>%s</name>\n<type>%s</type>\n<data>%s</data>\n<state>%s</state>\n</%s>\n",
			r.typ, xmlText(r.name), r.typ, xmlText(r.data), xmlText(r.state), r.typ)
		if r.retry != "" {
			b.WriteString(" (retry: " + r.retry + ")\n")
		}
	}
	b.WriteString("</profiles>\n")
	return b.String()
}

// gatewayFields are the details of g in the order sofia gives them.
func (s *Switch) gatewayFields(g *gateway) [][2]string {
	expires, pingTime, uptime := "3600", "0.00", "0"
	if !g.register {
		expires = "0"
	}
	if g.status == "UP" {
		pingTime = fmt.Sprintf("%.2f", 6+float64(len(g.proxy)%7)*1.37)
		uptime = strconv.FormatInt(s.t.Sub(g.upSince).Microseconds(), 10)
	}
	pingCount := "0"
	if g.status == "UP" {
		pingCount = "1"
	}
	next := s.t.Add(time.Duration(30-int(s.t.Sub(s.boot)/time.Second)%30) * time.Second)
	fields := [][2]string{
		{"name", g.name}, {"profile", "external"}, {"scheme", "Digest"}, {"realm", g.proxy}, {"username", g.username}, {"password", "yes"},
		{"from", "<sip:" + g.username + "@" + g.proxy + ">"},
		{"contact", fmt.Sprintf("<sip:gw+%s@%s:5080;transport=udp;gw=%s>", g.name, s.sp.ip, g.name)},
		{"exten", g.username}, {"to", "sip:" + g.username + "@" + g.proxy}, {"proxy", "sip:" + g.proxy}, {"context", "public"},
		{"expires", expires}, {"freq", expires}, {"ping", strconv.FormatInt(next.Unix(), 10)}, {"pingfreq", "30"},
		{"pingmin", "1"}, {"pingcount", pingCount}, {"pingmax", "1"}, {"pingtime", pingTime}, {"pinging", "0"},
		{"state", g.state}, {"status", g.status}, {"uptime-usec", uptime},
		{"calls-in", strconv.Itoa(g.callsIn)}, {"calls-out", strconv.Itoa(g.callsOut)},
		{"failed-calls-in", strconv.Itoa(g.failedIn)}, {"failed-calls-out", strconv.Itoa(g.failedOut)},
	}
	if g.state == "FAIL_WAIT" {
		fields = append(fields, [2]string{"retry", s.retry()})
	}
	return fields
}

func (s *Switch) gatewayXML(g *gateway, indent string) string {
	var b strings.Builder
	b.WriteString(indent + "<gateway>\n")
	for _, f := range s.gatewayFields(g) {
		fmt.Fprintf(&b, "%s  <%s>%s</%s>\n", indent, f[0], xmlText(f[1]), f[0])
	}
	b.WriteString(indent + "</gateway>\n")
	return b.String()
}

func (s *Switch) gatewayStatus(name string) string {
	for _, g := range s.listedGateways() {
		if g.name != name {
			continue
		}
		var b strings.Builder
		b.WriteString(statusRule)
		for _, f := range s.gatewayFields(g) {
			fmt.Fprintf(&b, "%-16s\t%s\n", f[0], f[1])
		}
		b.WriteString(statusRule)
		return b.String()
	}
	return "Invalid Gateway!\n"
}

func (s *Switch) profileStatus(name string) string {
	p := s.profile(name)
	if p == nil || !p.running {
		return "Invalid Profile!\n"
	}
	codecs := s.values["global_codec_prefs"]
	if p.name == "external" {
		codecs = s.values["outbound_codec_prefs"]
	}
	regs := 0
	if p.name == "internal" {
		for _, r := range s.regs {
			if s.visible(r) {
				regs++
			}
		}
	}
	url := fmt.Sprintf("sip:mod_sofia@%s:%d", s.sp.ip, p.port)
	rows := [][2]string{
		{"Name", p.name}, {"Domain Name", "N/A"}, {"DBName", "sofia_reg_" + p.name}, {"Pres Hosts", s.sp.ip},
		{"Dialplan", "XML"}, {"Context", p.context}, {"Challenge Realm", "auto_from"},
		{"RTP-IP", s.sp.ip}, {"SIP-IP", s.sp.ip}, {"URL", url}, {"BIND-URL", url + ";maddr=" + s.sp.ip},
	}
	if p.wss > 0 {
		rows = append(rows, [2]string{"WSS-BIND-URL", fmt.Sprintf("sips:mod_sofia@%s:%d;transport=wss", s.sp.ip, p.wss)})
	}
	rows = append(rows, [][2]string{
		{"HOLD-MUSIC", "local_stream://moh"}, {"CODECS IN", codecs}, {"CODECS OUT", codecs}, {"TEL-EVENT", "101"}, {"DTMF-MODE", "rfc2833"},
		{"CALLS-IN", strconv.Itoa(p.callsIn)}, {"FAILED-CALLS-IN", strconv.Itoa(p.failedIn)},
		{"CALLS-OUT", strconv.Itoa(p.callsOut)}, {"FAILED-CALLS-OUT", strconv.Itoa(p.failedOut)}, {"REGISTRATIONS", strconv.Itoa(regs)},
	}...)
	var b strings.Builder
	b.WriteString(statusRule)
	for _, r := range rows {
		fmt.Fprintf(&b, "%-17s\t%s\n", r[0], r[1])
	}
	b.WriteString(statusRule)
	return b.String()
}

// profileCommand runs sofia profile <name> <verb> [args]. A stopped
// profile is unknown to everything but start, as on FreeSWITCH.
func (s *Switch) profileCommand(name, verb string, args []string) string {
	p := s.profile(name)
	if verb == "start" {
		switch {
		case p == nil:
			return "-ERR Failure starting " + name + "\n"
		case p.running:
			return "-ERR Profile " + name + " is already running\n"
		}
		s.setRunning(p, true)
		return "+OK\n"
	}
	if p == nil || !p.running {
		return "-ERR Invalid Profile [" + name + "]\n"
	}
	switch verb {
	case "stop":
		s.setRunning(p, false)
		return "stopping: " + p.name + "\n"
	case "restart":
		s.logf(levelNotice, srcProfile, "", "Restarting profile %s", p.name)
		s.hangupProfile(p)
		s.logf(levelNotice, srcProfile, "", "Started Profile %s [sofia_reg_%s]", p.name, p.name)
		return "restarting: " + p.name + "\n"
	case "rescan":
		return "+OK scan complete\n"
	case "flush_inbound_reg":
		if p.name == "internal" {
			var flushed []*registration
			for _, r := range s.regs {
				if !r.flushed {
					flushed = append(flushed, r)
				}
			}
			s.changed("flush:"+p.name, func() {
				for _, r := range flushed {
					r.flushed = false
					if s.visible(r) {
						r.until = s.t.Add(time.Duration(r.expires) * time.Second)
						s.registered(r)
					}
				}
			})
			for _, r := range s.regs {
				r.flushed = true
			}
		}
		return "+OK\n"
	case "siptrace":
		if len(args) != 1 || args[0] != "on" && args[0] != "off" {
			return sofiaUsage
		}
		old := p.siptrace
		s.changed("siptrace:"+p.name, func() { s.setTrace(p, old) })
		s.setTrace(p, args[0] == "on")
		if p.siptrace {
			return "Enabled sip debugging on " + p.name + "\n"
		}
		return "Disabled sip debugging on " + p.name + "\n"
	case "killgw", "register", "unregister":
		if len(args) != 1 {
			return sofiaUsage
		}
		g := s.gateway(args[0])
		if g == nil || g.killed || !s.profile("external").running {
			return "-ERR Invalid gateway!\n"
		}
		oldState, oldKilled := g.state, g.killed
		s.changed("gateway:"+g.name, func() { g.state, g.killed = oldState, oldKilled })
		switch verb {
		case "killgw":
			g.killed = true
			return "+OK gateway marked for deletion.\n"
		case "register":
			g.state = "REGED"
			if !g.up {
				g.state = "FAIL_WAIT"
			}
		case "unregister":
			if g.register {
				g.state = "UNREGED"
			}
		}
		return "+OK\n"
	}
	return sofiaUsage
}

// setTrace turns the SIP trace of p on, with a keep-alive at once, or off.
func (s *Switch) setTrace(p *profile, on bool) {
	s.cancel(p.trace)
	p.trace, p.siptrace = nil, on
	if on {
		s.traceOptions(p)
	}
}

// setRunning starts or stops p for a while.
func (s *Switch) setRunning(p *profile, run bool) {
	old := p.running
	s.changed("profile:"+p.name, func() { s.runProfile(p, old) })
	s.runProfile(p, run)
}

// runProfile starts or stops p. Stopping hangs up its calls and hides its
// registrations; starting shows them again.
func (s *Switch) runProfile(p *profile, run bool) {
	if p.running == run {
		return
	}
	if !run {
		s.logf(levelNotice, srcProfile, "", "Stopping profile %s", p.name)
		s.hangupProfile(p)
		s.setTrace(p, false)
		p.running = false
		return
	}
	p.running = true
	s.logf(levelNotice, srcProfile, "", "Started Profile %s [sofia_reg_%s]", p.name, p.name)
	if p.name == "internal" {
		for _, r := range s.regs {
			if s.visible(r) {
				r.until = s.t.Add(time.Duration(r.expires) * time.Second)
				s.registered(r)
			}
		}
	}
}

// hangupProfile ends every call with a leg on p, MANAGER_REQUEST.
func (s *Switch) hangupProfile(p *profile) {
	var calls []*call
	for _, ch := range s.channels {
		if ch.profile == p.name && !ch.call.ended && !containsCall(calls, ch.call) {
			calls = append(calls, ch.call)
		}
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].legs[0].session < calls[j].legs[0].session })
	for _, c := range calls {
		s.end(c, killedBy(nil, "MANAGER_REQUEST"))
	}
}

func containsCall(calls []*call, c *call) bool {
	for _, x := range calls {
		if x == c {
			return true
		}
	}
	return false
}
