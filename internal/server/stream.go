package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fsdeck/internal/esl"
)

type logEvent struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	File  string `json:"file"`
	Func  string `json:"func"`
	Line  int    `json:"line"`
}

// trackLogLevel adds delta streams at level and keeps the shared ESL log
// level at the highest level any open stream asks for.
func (i *instance) trackLogLevel(level, delta int) {
	i.logMu.Lock()
	defer i.logMu.Unlock()
	if i.levels[level] += delta; i.levels[level] <= 0 {
		delete(i.levels, level)
	}
	want := -1
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

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, u user) {
	level := -1 // no log events
	if q := r.URL.Query().Get("loglevel"); q != "" && u.Role == roleAdmin {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 || n > 7 {
			writeError(w, http.StatusBadRequest, "loglevel must be 0 to 7")
			return
		}
		level = n
	}
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	msgs, cancel := inst.sw.Subscribe(256)
	defer cancel()
	if level >= 0 {
		inst.trackLogLevel(level, 1)
		defer inst.trackLogLevel(level, -1)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // a server WriteTimeout would cut the stream
	w.WriteHeader(http.StatusOK)
	send := func(event string, v any) error {
		data, err := json.Marshal(v) // single line: newlines in strings are escaped
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		return rc.Flush()
	}
	if send("state", inst.sw.State()) != nil {
		return
	}

	ping := time.NewTicker(s.pingInterval)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-inst.done:
			return
		case <-ping.C:
			if _, err = fmt.Fprint(w, ": ping\n\n"); err == nil {
				err = rc.Flush()
			}
		case m, ok := <-msgs:
			if !ok {
				return
			}
			switch m.Kind {
			case esl.KindState:
				err = send("state", inst.sw.State())
			case esl.KindEvent:
				if m.Headers["Event-Name"] == "HEARTBEAT" {
					err = send("heartbeat", m.Headers)
				}
			case esl.KindLog:
				l, perr := strconv.Atoi(m.Headers["Log-Level"])
				if level < 0 || perr != nil || l > level {
					continue
				}
				line, _ := strconv.Atoi(m.Headers["Log-Line"])
				err = send("log", logEvent{
					Level: l,
					Text:  strings.TrimRight(m.Body, "\r\n"),
					File:  m.Headers["Log-File"],
					Func:  m.Headers["Log-Func"],
					Line:  line,
				})
			}
		}
		if err != nil {
			return
		}
	}
}
