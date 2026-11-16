package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/juki/freeswitch-console/internal/esl"
)

type logEvent struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	File  string `json:"file"`
	Func  string `json:"func"`
	Line  int    `json:"line"`
	Seq   uint64 `json:"seq"`            // 0 for a line that is not kept in the backlog
	UUID  string `json:"uuid,omitempty"` // the channel the line is about, from User-Data
}

// streamItem is one message for an open stream.
type streamItem struct {
	kind    string            // "state", "heartbeat" or "log"
	headers map[string]string // heartbeat
	log     logEvent
}

// streamBuffer is how far a stream may fall behind before it loses messages.
const streamBuffer = 1024

// stream is one open event stream; level is -1 when it gets no log lines.
type stream struct {
	level int
	ch    chan streamItem
}

// addLog keeps a line in the backlog when its level is at most the backlog
// level, and forwards it to every stream that asked for its level.
func (i *instance) addLog(m esl.Message) {
	level, err := strconv.Atoi(m.Headers["Log-Level"])
	if err != nil {
		return
	}
	line, _ := strconv.Atoi(m.Headers["Log-Line"])
	ev := logEvent{
		Level: level,
		Text:  strings.TrimRight(m.Body, "\r\n"),
		File:  m.Headers["Log-File"],
		Func:  m.Headers["Log-Func"],
		Line:  line,
		UUID:  m.Headers["User-Data"],
	}
	i.logMu.Lock()
	defer i.logMu.Unlock()
	if i.backlogSize > 0 && level <= i.floor {
		i.seq++
		ev.Seq = i.seq
		if len(i.backlog) < i.backlogSize {
			i.backlog = append(i.backlog, ev)
		} else {
			i.backlog[i.backlogNext] = ev
			i.backlogNext = (i.backlogNext + 1) % i.backlogSize
		}
		i.attachLate(ev)
	}
	for st := range i.streams {
		if level <= st.level {
			st.send(streamItem{kind: "log", log: ev})
		}
	}
}

// forward passes a state change or heartbeat to every stream.
func (i *instance) forward(item streamItem) {
	i.logMu.Lock()
	defer i.logMu.Unlock()
	for st := range i.streams {
		st.send(item)
	}
}

// send never blocks the collector: a stream that does not keep up loses
// messages, as an ESL subscriber does.
func (st *stream) send(item streamItem) {
	select {
	case st.ch <- item:
	default:
	}
}

// openStream registers a stream and, when replay is set, returns the backlog
// lines with a level of at most level, oldest first. Copying the backlog and
// registering happen under one lock, so no line is both replayed and
// forwarded, and none falls between the two.
func (i *instance) openStream(level int, replay bool) ([]logEvent, *stream) {
	i.logMu.Lock()
	defer i.logMu.Unlock()
	var lines []logEvent
	if replay && level >= 0 {
		n := len(i.backlog)
		for k := 0; k < n; k++ {
			if ev := i.backlog[(i.backlogNext+k)%n]; ev.Level <= level {
				lines = append(lines, ev)
			}
		}
	}
	st := &stream{level: level, ch: make(chan streamItem, streamBuffer)}
	i.streams[st] = struct{}{}
	if level >= 0 {
		i.levels[level]++
		i.applyLogLevel()
	}
	return lines, st
}

func (i *instance) closeStream(st *stream) {
	i.logMu.Lock()
	defer i.logMu.Unlock()
	delete(i.streams, st)
	if st.level >= 0 {
		if i.levels[st.level]--; i.levels[st.level] <= 0 {
			delete(i.levels, st.level)
		}
		i.applyLogLevel()
	}
}

// applyLogLevel keeps the shared ESL log level at the highest level any open
// stream asks for, and never below the backlog level. logMu must be held.
func (i *instance) applyLogLevel() {
	want := i.floor
	for l := range i.levels {
		want = max(want, l)
	}
	select {
	case <-i.done:
		return // the switch is closed
	default:
	}
	if want != i.applied {
		i.applied = want
		i.sw.SetLogLevel(want)
	}
}

// acquireStream counts a stream of the session token; it answers 429 and
// returns false when the session or the server has too many open.
func (s *Server) acquireStream(w http.ResponseWriter, token string) bool {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	msg := ""
	switch {
	case s.sessionStreams[token] >= s.maxSessionStreams:
		msg = "too many event streams open in this session; close another console tab"
	case s.openStreams >= s.maxStreams:
		msg = "too many event streams open on the server; try again later"
	}
	if msg != "" {
		writeError(w, http.StatusTooManyRequests, msg)
		return false
	}
	s.sessionStreams[token]++
	s.openStreams++
	return true
}

func (s *Server) releaseStream(token string) {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.sessionStreams[token]--; s.sessionStreams[token] <= 0 {
		delete(s.sessionStreams, token)
	}
	s.openStreams--
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, u user) {
	level := -1 // no log events
	q := r.URL.Query()
	if v := q.Get("loglevel"); v != "" && allowed(u.Role, capLogs) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 7 {
			writeError(w, http.StatusBadRequest, "loglevel must be 0 to 7")
			return
		}
		level = n
	}
	replay := true
	switch q.Get("backlog") {
	case "", "1":
	case "0":
		replay = false
	default:
		writeError(w, http.StatusBadRequest, "backlog must be 0 or 1")
		return
	}
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	token := sessionToken(r)
	if !s.acquireStream(w, token) {
		return
	}
	defer s.releaseStream(token)
	lines, st := inst.openStream(level, replay)
	defer inst.closeStream(st)

	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	// Neither a server WriteTimeout nor a ReadTimeout applies to the stream.
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.SetReadDeadline(time.Time{})
	w.WriteHeader(http.StatusOK)
	write := func(event string, v any) error {
		data, err := json.Marshal(v) // single line: newlines in strings are escaped
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		return err
	}
	send := func(event string, v any) error {
		if err := write(event, v); err != nil {
			return err
		}
		return rc.Flush()
	}
	if write("state", inst.sw.State()) != nil {
		return
	}
	var last uint64 // sequence number of the last replayed line
	if level >= 0 {
		for _, ev := range lines {
			if write("log", ev) != nil {
				return
			}
			last = ev.Seq
		}
		if write("backlog", map[string]int{"lines": len(lines)}) != nil {
			return
		}
	}
	if rc.Flush() != nil {
		return
	}

	ping := time.NewTicker(s.pingInterval)
	defer ping.Stop()
	// Signing out, or a change to the account, ends the session; the stream
	// ends with it, and a stream with log lines when the role loses capLogs.
	check := time.NewTicker(s.streamCheck)
	defer check.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-check.C:
			cur, ok := s.sessionUser(r)
			if !ok || (level >= 0 && !allowed(cur.Role, capLogs)) {
				return
			}
		case <-inst.done:
			return
		case <-inst.collected: // the switch was closed
			return
		case <-ping.C:
			if _, err = fmt.Fprint(w, ": ping\n\n"); err == nil {
				err = rc.Flush()
			}
		case item := <-st.ch:
			switch item.kind {
			case "state":
				err = send("state", inst.sw.State())
			case "heartbeat":
				err = send("heartbeat", item.headers)
			case "log":
				// Registration under the collector's lock already rules
				// out a repeat; the check keeps the guarantee local.
				if item.log.Seq != 0 && item.log.Seq <= last {
					continue
				}
				err = send("log", item.log)
			}
		}
		if err != nil {
			return
		}
	}
}
