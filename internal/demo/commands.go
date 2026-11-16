package demo

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// commandDef is one simulated API command.
type commandDef struct {
	name, module, description, syntax string
	run                               func(s *Switch, arg string) string
}

// commandTable lists every command the simulation answers; anything else is
// "Command not found", as on FreeSWITCH. It is filled in init, since the
// commands refer to it.
var commandTable []commandDef

func init() {
	commandTable = []commandDef{
		{"console_complete_xml", "mod_commands", "Complete a console line, as XML", "c=<cursor>;<line>", (*Switch).cmdComplete},
		{"fsctl", "mod_commands", "Control the core", "loglevel [<level>]|max_sessions <n>|sps <n>|pause|resume", (*Switch).cmdFsctl},
		{"global_getvar", "mod_commands", "Get a global variable", "[<var>]", (*Switch).cmdGetvar},
		{"global_setvar", "mod_commands", "Set a global variable", "<var>=<value>", (*Switch).cmdSetvar},
		{"hostname", "mod_commands", "Return the host name", "", (*Switch).cmdHostname},
		{"json", "mod_commands", "Run a command given as JSON", "{\"command\":\"status\",\"data\":\"\"}", (*Switch).cmdJSON},
		{"load", "mod_commands", "Load a module", "<mod_name>", (*Switch).cmdLoad},
		{"originate", "mod_commands", "Start a call", "<call url> <exten>|&<application_name>(<app_args>) [<dialplan>] [<context>] [<cid_name>] [<cid_num>] [<timeout_sec>]", (*Switch).cmdOriginate},
		{"reload", "mod_commands", "Reload a module", "[-f] <mod_name>", (*Switch).cmdReload},
		{"reloadacl", "mod_commands", "Reload the access lists", "", (*Switch).cmdReloadACL},
		{"reloadxml", "mod_commands", "Reload the XML configuration", "", (*Switch).cmdReloadXML},
		{"show", "mod_commands", "Show the state of the switch", "channels|calls|registrations|modules|api [count|as json]", (*Switch).cmdShow},
		{"sofia", "mod_sofia", "Control the SIP profiles", "<cmd> <args>", (*Switch).cmdSofia},
		{"status", "mod_commands", "Show the current status", "", (*Switch).cmdStatus},
		{"unload", "mod_commands", "Unload a module", "[-f] <mod_name>", (*Switch).cmdUnload},
		{"uptime", "mod_commands", "Show the uptime", "[us|ms|s|m|h|d]", (*Switch).cmdUptime},
		{"uuid_kill", "mod_commands", "Hang up a channel", "<uuid> [cause]", (*Switch).cmdKill},
		{"version", "mod_commands", "Show the version", "[short]", (*Switch).cmdVersion},
		{"xml_locate", "mod_commands", "Find a part of the XML configuration", "[root | <section> | <section> <tag> <tag_attr_name> <tag_attr_val>]", (*Switch).cmdLocate},
	}
}

// Bounds of the global variables a visitor adds.
const (
	maxOwnVars  = 64
	maxVarName  = 128
	maxVarValue = 1024
)

// command answers one API command line.
func (s *Switch) command(line string) string {
	name, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	arg = strings.TrimSpace(arg)
	for i := range commandTable {
		if c := &commandTable[i]; c.name == name && s.loaded(c.module) {
			return c.run(s, arg)
		}
	}
	return "-ERR " + name + " Command not found!\n"
}

func (s *Switch) usage(name string) string {
	for _, c := range commandTable {
		if c.name == name {
			return "-USAGE: " + c.syntax + "\n"
		}
	}
	return "-USAGE\n"
}

func (s *Switch) cmdStatus(string) string {
	y, d, h, m, sec, ms, us := s.uptime()
	ready := "ready"
	if s.paused {
		ready = "not ready"
	}
	return fmt.Sprintf("UP %d years, %d days, %d hours, %d minutes, %d seconds, %d milliseconds, %d microseconds\n"+
		"FreeSWITCH (Version %s-release  64bit) is %s\n"+
		"%d session(s) since startup\n"+
		"%d session(s) - peak %d, last 5min %d \n"+
		"%d session(s) per Sec out of max %d, peak %d, last 5min %d \n"+
		"%d session(s) max\n"+
		"min idle cpu 0.00/%.2f\n"+
		"Current Stack Size/Max 240K/8192K\n",
		y, d, h, m, sec, ms, us, s.sp.version, ready, s.sessionN, len(s.channels), s.peak, s.peak5(),
		s.ratePerSec(), s.sps, s.ratePeak, s.ratePeak5(), s.maxSessions, s.idle)
}

// statusJSON is the answer of json {"command":"status"}.
func (s *Switch) statusJSON() string {
	type count struct {
		Total    int `json:"total"`
		Active   int `json:"active"`
		Peak     int `json:"peak"`
		Peak5Min int `json:"peak5Min"`
		Limit    int `json:"limit"`
	}
	type rate struct {
		Current  int `json:"current"`
		Max      int `json:"max"`
		Peak     int `json:"peak"`
		Peak5Min int `json:"peak5Min"`
	}
	y, d, h, m, sec, ms, us := s.uptime()
	status := "ready"
	if s.paused {
		status = "not ready"
	}
	var resp struct {
		SystemStatus string         `json:"systemStatus"`
		Uptime       map[string]int `json:"uptime"`
		Version      string         `json:"version"`
		Sessions     struct {
			Count count `json:"count"`
			Rate  rate  `json:"rate"`
		} `json:"sessions"`
		IdleCPU struct {
			Used    float64 `json:"used"`
			Allowed float64 `json:"allowed"`
		} `json:"idleCPU"`
		StackSizeKB struct {
			Current int `json:"current"`
			Max     int `json:"max"`
		} `json:"stackSizeKB"`
	}
	resp.SystemStatus = status
	resp.Uptime = map[string]int{"years": y, "days": d, "hours": h, "minutes": m, "seconds": sec, "milliseconds": ms, "microseconds": us}
	resp.Version = s.sp.version + "-release 64bit"
	resp.Sessions.Count = count{s.sessionN, len(s.channels), s.peak, s.peak5(), s.maxSessions}
	resp.Sessions.Rate = rate{s.ratePerSec(), s.sps, s.ratePeak, s.ratePeak5()}
	resp.IdleCPU.Allowed = float64(int(s.idle*100)) / 100
	resp.StackSizeKB.Current, resp.StackSizeKB.Max = 240, 8192
	body, _ := json.Marshal(resp)
	return `{"command":"status","data":"","status":"success","response":` + string(body) + "}\n"
}

func (s *Switch) cmdJSON(arg string) string {
	var req struct {
		Command string          `json:"command"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(arg), &req); err != nil {
		return "-ERR JSON parse error\n"
	}
	if req.Command == "status" {
		return s.statusJSON()
	}
	cmd, _ := json.Marshal(req.Command)
	return `{"command":` + string(cmd) + `,"status":"error","response":{"message":"Command not found!"}}` + "\n"
}

func (s *Switch) cmdVersion(arg string) string {
	if arg == "short" {
		return s.sp.version + "\n"
	}
	return "FreeSWITCH Version " + s.sp.version + "-release~64bit (-release 64bit)\n"
}

func (s *Switch) cmdHostname(string) string { return s.sp.name }

func (s *Switch) cmdUptime(arg string) string {
	d := s.t.Sub(s.boot)
	unit := map[string]time.Duration{
		"us": time.Microsecond, "microseconds": time.Microsecond, "ms": time.Millisecond, "milliseconds": time.Millisecond,
		"": time.Second, "s": time.Second, "seconds": time.Second, "m": time.Minute, "minutes": time.Minute,
		"h": time.Hour, "hours": time.Hour, "d": 24 * time.Hour, "days": 24 * time.Hour,
	}[arg]
	if unit == 0 {
		unit = time.Second
	}
	return strconv.FormatInt(int64(d/unit), 10)
}

func (s *Switch) cmdReloadXML(string) string {
	s.logf(levelInfo, srcXML, "", "Reloading XML")
	return "+OK [Success]\n"
}

func (s *Switch) cmdReloadACL(string) string { return "+OK acl reloaded\n" }

func (s *Switch) cmdGetvar(arg string) string {
	if arg != "" {
		return s.values[arg]
	}
	var b strings.Builder
	for _, name := range s.vars {
		b.WriteString(name + "=" + s.values[name] + "\n")
	}
	return b.String()
}

func (s *Switch) cmdSetvar(arg string) string {
	name, value, ok := strings.Cut(arg, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return s.usage("global_setvar")
	}
	old, existed := s.values[name]
	switch {
	case len(name) > maxVarName || len(value) > maxVarValue:
		return fmt.Sprintf("-ERR the demo keeps names of at most %d and values of at most %d characters\n", maxVarName, maxVarValue)
	case !existed && len(s.vars) >= len(s.conf.vars)+maxOwnVars:
		return fmt.Sprintf("-ERR the demo keeps at most %d variables of its own; try again in a few minutes\n", maxOwnVars)
	}
	s.changed("var:"+name, func() {
		if existed {
			s.values[name] = old
			return
		}
		delete(s.values, name)
		s.vars = slices.DeleteFunc(s.vars, func(v string) bool { return v == name })
	})
	if _, ok := s.values[name]; !ok {
		s.vars = append(s.vars, name)
	}
	s.values[name] = value
	return "+OK\n"
}

// setInt changes *p to v for a while.
func (s *Switch) setInt(key string, p *int, v int) {
	old := *p
	s.changed(key, func() { *p = old })
	*p = v
}

func (s *Switch) cmdFsctl(arg string) string {
	f := strings.Fields(arg)
	if len(f) == 0 {
		return s.usage("fsctl")
	}
	number := func() (int, bool) {
		if len(f) < 2 {
			return 0, false
		}
		n, err := strconv.Atoi(f[1])
		return n, err == nil && n > 0
	}
	switch f[0] {
	case "loglevel":
		if len(f) > 1 {
			n, err := strconv.Atoi(f[1])
			if err != nil {
				n = slices.Index(levelNames[:], strings.ToUpper(f[1]))
			}
			if n < 0 || n > 7 {
				return "-ERR Invalid log level\n"
			}
			s.setInt("fsctl:loglevel", &s.coreLevel, n)
		}
		return fmt.Sprintf("+OK log level: %s [%d]\n", levelNames[s.coreLevel], s.coreLevel)
	case "max_sessions":
		n, ok := number()
		if !ok {
			return s.usage("fsctl")
		}
		s.setInt("fsctl:max_sessions", &s.maxSessions, n)
		return fmt.Sprintf("+OK max sessions: %d\n", n)
	case "sps":
		n, ok := number()
		if !ok {
			return s.usage("fsctl")
		}
		s.setInt("fsctl:sps", &s.sps, n)
		return fmt.Sprintf("+OK sessions per second: %d\n", n)
	case "pause", "resume":
		old := s.paused
		s.changed("fsctl:pause", func() { s.paused = old })
		s.paused = f[0] == "pause"
		return "+OK\n"
	}
	return "-ERR INVALID COMMAND\n" + s.usage("fsctl")
}

// moduleArg is the module name of load, unload and reload, after -f.
func moduleArg(arg string) string {
	f := strings.Fields(arg)
	if len(f) > 1 && f[0] == "-f" {
		f = f[1:]
	}
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func (s *Switch) setLoaded(m *module, v bool) {
	old := m.loaded
	s.changed("module:"+m.name, func() { m.loaded = old })
	m.loaded = v
}

func (s *Switch) cmdLoad(arg string) string {
	m := s.module(moduleArg(arg))
	switch {
	case m == nil:
		return "-ERR [module load file routine returned an error]\n"
	case m.loaded:
		return "-ERR [Module already loaded]\n"
	}
	s.setLoaded(m, true)
	s.logf(levelConsole, srcModule, "", "Successfully Loaded [%s]", m.name)
	return "+OK Reloading XML\n+OK\n"
}

func (s *Switch) cmdUnload(arg string) string {
	m := s.module(moduleArg(arg))
	switch {
	case m == nil || !m.loaded:
		return "-ERR [Module is not loaded]\n"
	case m.keep:
		return "-ERR [" + m.name + " cannot be unloaded in the demo]\n"
	}
	s.setLoaded(m, false)
	s.logf(levelConsole, srcModule, "", "Stopping: %s", m.name)
	return "+OK\n"
}

func (s *Switch) cmdReload(arg string) string {
	m := s.module(moduleArg(arg))
	if m == nil || !m.loaded {
		return "-ERR [Module is not loaded]\n"
	}
	s.logf(levelConsole, srcModule, "", "Successfully Loaded [%s]", m.name)
	return "+OK Reloading XML\n+OK module unloaded\n+OK module loaded\n"
}

func (s *Switch) cmdLocate(arg string) string {
	f := strings.Fields(arg)
	var doc string
	var ok bool
	switch {
	case len(f) == 0 || len(f) == 1 && f[0] == "root":
		doc, ok = s.conf.locate("", "", "", "")
	case len(f) == 1:
		doc, ok = s.conf.locate(f[0], "", "", "")
	case len(f) == 4:
		doc, ok = s.conf.locate(f[0], f[1], f[2], f[3])
	default:
		return s.usage("xml_locate")
	}
	if !ok {
		return "can't find anything\n"
	}
	return doc
}

func (s *Switch) cmdOriginate(arg string) string {
	f := strings.Fields(arg)
	if len(f) < 2 {
		return s.usage("originate")
	}
	app, data := "", ""
	if rest, ok := strings.CutPrefix(f[1], "&"); ok {
		app, data, _ = strings.Cut(rest, "(")
		data = strings.TrimSuffix(data, ")")
	}
	return s.originateCall(f[0], app, data)
}

func (s *Switch) cmdKill(arg string) string {
	f := strings.Fields(arg)
	if len(f) == 0 {
		return s.usage("uuid_kill")
	}
	ch := s.channels[f[0]]
	if ch == nil {
		return "-ERR No such channel!\n"
	}
	cause := "NORMAL_CLEARING"
	if len(f) > 1 {
		if _, ok := q850[f[1]]; ok {
			cause = f[1]
		}
	}
	s.end(ch.call, killedBy(ch, cause))
	return "+OK\n"
}

// The columns of show, as FreeSWITCH 1.10 prints them.
var (
	channelCols = []string{"uuid", "direction", "created", "created_epoch", "name", "state", "cid_name", "cid_num", "ip_addr", "dest",
		"application", "application_data", "dialplan", "context", "read_codec", "read_rate", "read_bit_rate", "write_codec", "write_rate",
		"write_bit_rate", "secure", "hostname", "presence_id", "presence_data", "accountcode", "callstate", "callee_name", "callee_num",
		"callee_direction", "call_uuid", "sent_callee_name", "sent_callee_num", "initial_cid_name", "initial_cid_num", "initial_ip_addr",
		"initial_dest", "initial_dialplan", "initial_context"}
	legCols = []string{"uuid", "direction", "created", "created_epoch", "name", "state", "cid_name", "cid_num", "ip_addr", "dest",
		"presence_id", "presence_data", "accountcode", "callstate", "callee_name", "callee_num", "callee_direction", "call_uuid",
		"hostname", "sent_callee_name", "sent_callee_num"}
	callCols = func() []string {
		cols := append([]string{}, legCols...)
		for _, c := range legCols {
			if c != "call_uuid" && c != "hostname" {
				cols = append(cols, "b_"+c)
			}
		}
		return append(cols, "call_created_epoch")
	}()
	regCols    = []string{"reg_user", "realm", "token", "url", "expires", "network_ip", "network_port", "network_proto", "hostname", "metadata"}
	moduleCols = []string{"type", "name", "ikey", "filename"}
	apiCols    = []string{"name", "description", "syntax", "ikey"}
)

func (s *Switch) cmdShow(arg string) string {
	f := strings.Fields(arg)
	if len(f) == 0 {
		return s.usage("show")
	}
	var cols []string
	var rows [][]string
	switch f[0] {
	case "channels":
		cols = channelCols
		for _, ch := range s.sortedChannels() {
			rows = append(rows, s.channelValues(ch))
		}
	case "calls":
		cols = callCols
		for _, ch := range s.sortedChannels() {
			if ch != ch.call.legs[0] {
				continue
			}
			row := s.legValues(ch)
			if len(ch.call.legs) > 1 {
				b := s.legValues(ch.call.legs[1])
				for i, c := range legCols {
					if c != "call_uuid" && c != "hostname" {
						row = append(row, b[i])
					}
				}
			} else {
				row = append(row, make([]string, len(legCols)-2)...)
			}
			rows = append(rows, append(row, strconv.FormatInt(ch.created.Unix(), 10)))
		}
	case "registrations":
		cols = regCols
		for _, r := range s.regs {
			if !s.visible(r) {
				continue
			}
			url := fmt.Sprintf("sofia/internal/sip:%s@%s:%d", r.user, r.ip, r.port)
			switch r.proto {
			case "tcp":
				url += ";transport=tcp"
			case "wss":
				url += ";transport=ws"
			}
			rows = append(rows, []string{r.user, s.sp.ip, r.callID, url, strconv.FormatInt(r.until.Unix(), 10), r.ip, strconv.Itoa(r.port), r.proto, s.sp.name, r.agent})
		}
	case "modules":
		cols = moduleCols
		for _, c := range coreRows {
			rows = append(rows, c[:])
		}
		for _, m := range s.modules {
			if m.loaded {
				for _, i := range m.ifaces {
					rows = append(rows, []string{i[0], i[1], m.name, "/usr/lib/freeswitch/mod/" + m.name + ".so"})
				}
			}
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i][0] != rows[j][0] {
				return rows[i][0] < rows[j][0]
			}
			return rows[i][1] < rows[j][1]
		})
	case "api":
		cols = apiCols
		for _, c := range commandTable {
			if s.loaded(c.module) {
				rows = append(rows, []string{c.name, c.description, c.syntax, c.module})
			}
		}
	default:
		return s.usage("show")
	}
	switch strings.Join(f[1:], " ") {
	case "":
		var b strings.Builder
		b.WriteString(strings.Join(cols, ",") + "\n")
		for _, r := range rows {
			b.WriteString(strings.Join(r, ",") + "\n")
		}
		fmt.Fprintf(&b, "\n%d total.\n", len(rows))
		return b.String()
	case "count":
		return fmt.Sprintf("\n%d total.\n\n", len(rows))
	case "as json":
		return jsonRows(cols, rows)
	}
	return s.usage("show")
}

// jsonRows is the JSON form of show: every value a string, the columns in
// FreeSWITCH's order, and no rows key when there are none.
func jsonRows(cols []string, rows [][]string) string {
	if len(rows) == 0 {
		return `{"row_count":0}` + "\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `{"row_count":%d,"rows":[`, len(rows))
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('{')
		for j, c := range cols {
			if j > 0 {
				b.WriteByte(',')
			}
			k, _ := json.Marshal(c)
			v, _ := json.Marshal(r[j])
			b.Write(k)
			b.WriteByte(':')
			b.Write(v)
		}
		b.WriteByte('}')
	}
	b.WriteString("]}\n")
	return b.String()
}

func (s *Switch) sortedChannels() []*channel {
	out := make([]*channel, 0, len(s.channels))
	for _, ch := range s.channels {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].created.Equal(out[j].created) {
			return out[i].created.Before(out[j].created)
		}
		return out[i].session < out[j].session
	})
	return out
}

func (s *Switch) presence(ch *channel) string {
	switch {
	case ch.profile != "internal":
		return ""
	case ch.direction == "outbound":
		return ch.dest + "@" + s.sp.ip
	}
	return ch.cidNum + "@" + s.sp.ip
}

// legValues are the legCols of ch.
func (s *Switch) legValues(ch *channel) []string {
	calleeDir := ""
	if ch.calleeNum != "" {
		calleeDir = "SEND"
	}
	return []string{ch.uuid, ch.direction, ch.created.UTC().Format(time.DateTime), strconv.FormatInt(ch.created.Unix(), 10), ch.name, ch.state,
		ch.cidName, ch.cidNum, ch.ip, ch.dest, s.presence(ch), "", "", ch.callstate, ch.calleeName, ch.calleeNum, calleeDir,
		ch.call.legs[0].uuid, s.sp.name, ch.calleeName, ch.calleeNum}
}

// channelValues are the channelCols of ch.
func (s *Switch) channelValues(ch *channel) []string {
	bitRate := "64000"
	if ch.codec == "OPUS" {
		bitRate = "0"
	}
	rate := strconv.Itoa(ch.rate)
	first := ch.call.legs[0]
	initialDest, initialContext := ch.dest, ch.context
	if ch == first && ch.call.kind == inbound && ch.call.outcome != noRoute {
		initialDest, initialContext = did(ch.dest), "public"
	}
	calleeDir := ""
	if ch.calleeNum != "" {
		calleeDir = "SEND"
	}
	return []string{ch.uuid, ch.direction, ch.created.UTC().Format(time.DateTime), strconv.FormatInt(ch.created.Unix(), 10), ch.name, ch.state,
		ch.cidName, ch.cidNum, ch.ip, ch.dest, ch.app, ch.appData, "XML", ch.context, ch.codec, rate, bitRate, ch.codec, rate, bitRate, "",
		s.sp.name, s.presence(ch), "", "", ch.callstate, ch.calleeName, ch.calleeNum, calleeDir, first.uuid, ch.calleeName, ch.calleeNum,
		ch.cidName, ch.cidNum, ch.ip, initialDest, "XML", initialContext}
}

// cmdComplete answers a Tab of the console: c=<cursor>;<line>, the line up
// to the cursor being completed.
func (s *Switch) cmdComplete(arg string) string {
	cursor, line, _ := strings.Cut(arg, ";")
	if n, err := strconv.Atoi(strings.TrimPrefix(cursor, "c=")); err == nil && n > 0 && n <= len(line) {
		line = line[:n]
	}
	words := strings.Fields(line)
	partial := ""
	if len(words) > 0 && !strings.HasSuffix(line, " ") {
		partial, words = words[len(words)-1], words[:len(words)-1]
	}
	var matches []string
	for _, c := range s.candidates(words) {
		if strings.HasPrefix(c, partial) && !slices.Contains(matches, c) {
			matches = append(matches, c)
		}
	}
	slices.Sort(matches)
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\"?>\n<complete>\n")
	for _, m := range matches {
		b.WriteString("  <match>" + xmlText(m) + "</match>\n")
	}
	if len(matches) > 0 {
		text := matches[0]
		if len(matches) == 1 {
			text += " "
		} else {
			for _, m := range matches[1:] {
				for !strings.HasPrefix(m, text) {
					_, size := utf8.DecodeLastRuneInString(text)
					text = text[:len(text)-size]
				}
			}
		}
		fmt.Fprintf(&b, "  <write length=\"%d\">%s</write>\n", len(partial), xmlText(text))
	}
	b.WriteString("</complete>\n")
	return b.String()
}

// candidates are the words that may follow words in a console line.
func (s *Switch) candidates(words []string) []string {
	var names []string
	at := func(n int, w ...string) bool {
		if len(words) != n {
			return false
		}
		for i, x := range w {
			if x != "*" && words[i] != x {
				return false
			}
		}
		return true
	}
	profiles := func() []string {
		var out []string
		for _, p := range s.profiles {
			out = append(out, p.name)
		}
		return out
	}
	gateways := func() []string {
		var out []string
		for _, g := range s.gateways {
			if !g.killed {
				out = append(out, g.name)
			}
		}
		return out
	}
	switch {
	case len(words) == 0:
		for _, c := range commandTable {
			if s.loaded(c.module) {
				names = append(names, c.name)
			}
		}
	case at(1, "show"):
		names = []string{"api", "calls", "channels", "modules", "registrations"}
	case at(1, "sofia"):
		names = []string{"profile", "status", "xmlstatus"}
	case at(2, "sofia", "profile"), at(3, "sofia", "status", "profile"):
		names = profiles()
	case at(2, "sofia", "status"):
		names = []string{"gateway", "profile"}
	case at(2, "sofia", "xmlstatus"):
		names = []string{"gateway"}
	case at(3, "sofia", "status", "gateway"), at(3, "sofia", "xmlstatus", "gateway"), at(4, "sofia", "profile", "*", "killgw"),
		at(4, "sofia", "profile", "*", "register"), at(4, "sofia", "profile", "*", "unregister"):
		names = gateways()
	case at(3, "sofia", "profile", "*"):
		names = []string{"flush_inbound_reg", "killgw", "register", "rescan", "restart", "siptrace", "start", "stop", "unregister"}
	case at(4, "sofia", "profile", "*", "siptrace"):
		names = []string{"off", "on"}
	case at(1, "uuid_kill"):
		for _, ch := range s.sortedChannels() {
			names = append(names, ch.uuid)
		}
	case at(1, "load"):
		for _, m := range s.modules {
			if !m.loaded {
				names = append(names, m.name)
			}
		}
	case at(1, "unload"), at(1, "reload"):
		for _, m := range s.modules {
			if m.loaded {
				names = append(names, m.name)
			}
		}
	case at(1, "fsctl"):
		names = []string{"loglevel", "max_sessions", "pause", "resume", "sps"}
	case at(1, "global_getvar"):
		names = append(names, s.vars...)
	case at(1, "xml_locate"):
		names = []string{"configuration", "dialplan", "directory", "root"}
	}
	return names
}

func xmlText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
