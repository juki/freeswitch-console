package server

import (
	"cmp"
	"crypto/subtle"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxCauses bounds the hang-up causes counted per instance; further causes
// are counted as "other".
const maxCauses = 100

// handleMetrics serves the metrics in the Prometheus text format to a
// request with the bearer token of Config.MetricsToken. It reads only state
// the console holds; nothing is asked of FreeSWITCH.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MetricsToken == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.MetricsToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "a valid bearer token is required")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.writeMetrics(w, time.Now())
}

// instanceMetrics is a copy of what the metrics of one instance read.
type instanceMetrics struct {
	name, id  string
	inst      string // the instance label value: the name, with the id when the name is shared
	up        bool
	beat      sample
	beatLimit int
	beatAt    time.Time // zero before the first heartbeat
	hangups   map[string]uint64
	sipOn     bool
	sip       map[string]uint64
}

func (i *instance) metrics(name, id string) instanceMetrics {
	m := instanceMetrics{name: name, id: id, up: i.sw.State().Connected, sipOn: i.sipSize > 0}
	i.histMu.Lock()
	m.beat, m.beatLimit, m.beatAt = i.beat, i.beatLimit, i.beatAt
	i.histMu.Unlock()
	i.callsMu.Lock()
	m.hangups = maps.Clone(i.hangupCounts)
	i.callsMu.Unlock()
	i.sipMu.Lock()
	m.sip = maps.Clone(i.sipCounts)
	i.sipMu.Unlock()
	return m
}

// writeMetrics writes every metric family, the instances sorted by name and
// the label values of each sorted, so the same state gives the same text.
func (s *Server) writeMetrics(w io.Writer, now time.Time) {
	names := map[string]string{}
	for _, in := range s.cfg.Store.List() {
		names[in.ID] = in.Name
	}
	s.mu.Lock()
	insts := maps.Clone(s.insts)
	s.mu.Unlock()
	var list []instanceMetrics
	shared := map[string]int{}
	for id, inst := range insts {
		if name, ok := names[id]; ok {
			list = append(list, inst.metrics(name, id))
			shared[name]++
		}
	}
	slices.SortFunc(list, func(a, b instanceMetrics) int { return cmp.Or(cmp.Compare(a.name, b.name), cmp.Compare(a.id, b.id)) })
	// Names are not unique; two series with the same labels fail the scrape.
	for i := range list {
		list[i].inst = list[i].name
		if shared[list[i].name] > 1 {
			list[i].inst = list[i].name + " (" + list[i].id + ")"
		}
	}

	var b strings.Builder
	family := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	family("fsconsole_build_info", "gauge", "The version of the console.")
	fmt.Fprintf(&b, "fsconsole_build_info{version=%s} 1\n", label(s.cfg.Version))
	family("fsconsole_instance_up", "gauge", "1 when the Event Socket of the instance is connected.")
	for _, m := range list {
		up := 0
		if m.up {
			up = 1
		}
		fmt.Fprintf(&b, "fsconsole_instance_up{instance=%s} %d\n", label(m.inst), up)
	}
	for _, g := range []struct {
		name, help string
		value      func(m instanceMetrics) string
	}{
		{"fsconsole_sessions", "Channels in progress at the last heartbeat.", func(m instanceMetrics) string { return strconv.Itoa(m.beat.Sessions) }},
		{"fsconsole_sessions_per_second", "Sessions created in the second before the last heartbeat.", func(m instanceMetrics) string { return strconv.Itoa(m.beat.Rate) }},
		{"fsconsole_idle_cpu_percent", "Idle CPU at the last heartbeat, percent.", func(m instanceMetrics) string { return strconv.FormatFloat(m.beat.Idle, 'g', -1, 64) }},
		{"fsconsole_session_limit", "Max-Sessions at the last heartbeat.", func(m instanceMetrics) string { return strconv.Itoa(m.beatLimit) }},
		{"fsconsole_heartbeat_age_seconds", "Seconds since the last heartbeat was received.", func(m instanceMetrics) string {
			return strconv.FormatFloat(now.Sub(m.beatAt).Seconds(), 'f', 3, 64)
		}},
	} {
		family(g.name, "gauge", g.help)
		for _, m := range list {
			if !m.beatAt.IsZero() {
				fmt.Fprintf(&b, "%s{instance=%s} %s\n", g.name, label(m.inst), g.value(m))
			}
		}
	}
	family("fsconsole_hangups_total", "counter", "Hang-ups received since the console started, by cause.")
	for _, m := range list {
		for _, cause := range slices.Sorted(maps.Keys(m.hangups)) {
			fmt.Fprintf(&b, "fsconsole_hangups_total{instance=%s,cause=%s} %d\n", label(m.inst), label(cause), m.hangups[cause])
		}
	}
	family("fsconsole_sip_events_total", "counter", "SIP events received since the console started, by kind.")
	for _, m := range list {
		if !m.sipOn {
			continue
		}
		var kinds []string
		for _, sub := range SipSubclasses {
			kinds = append(kinds, sipKind(sub))
		}
		slices.Sort(kinds)
		for _, kind := range kinds {
			fmt.Fprintf(&b, "fsconsole_sip_events_total{instance=%s,kind=%s} %d\n", label(m.inst), label(kind), m.sip[kind])
		}
	}
	io.WriteString(w, b.String())
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// label quotes a label value as the Prometheus text format requires.
func label(v string) string {
	return `"` + labelEscaper.Replace(v) + `"`
}
