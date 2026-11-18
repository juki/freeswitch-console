package server

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/juki/freeswitch-console/internal/esl"
)

// historySize keeps 2 hours at FreeSWITCH's default 20s heartbeat.
const historySize = 360

// sample is the load reported by one HEARTBEAT event.
type sample struct {
	T        int64   `json:"t"` // unix seconds, server receive time
	Sessions int     `json:"sessions"`
	Rate     int     `json:"rate"`
	Idle     float64 `json:"idle"`
}

// pastLoad is a Switch that knows the load of its heartbeats before it was
// dialled: a simulated switch of demo mode (demo.Switch.History). add gets
// them oldest first.
type pastLoad interface {
	History(add func(at time.Time, sessions, rate int, idle float64))
}

// collect is the only subscriber of the switch. It records a sample per
// HEARTBEAT, a record per CHANNEL_HANGUP_COMPLETE and per CUSTOM event of a
// SIP subclass kept (SipSubclasses), keeps the gateway states and seeds them
// after every connect, keeps the log backlog
// and forwards state changes, heartbeats and log lines to the open streams,
// until the switch is closed. Log lines come on a channel of their own, so a
// flood of them cannot crowd out the events.
func (i *instance) collect(events, logs <-chan esl.Message, cancel func()) {
	defer close(i.collected)
	defer cancel()
	if i.sw.State().Connected {
		i.seedGateways()
	}
	for {
		select {
		case <-i.done:
			return
		case m, ok := <-events:
			if !ok {
				return
			}
			switch m.Kind {
			case esl.KindEvent:
				switch m.Headers["Event-Name"] {
				case "HEARTBEAT":
					now := time.Now()
					hb := heartbeatSample(m.Headers, now)
					i.addSample(hb)
					i.setBeat(hb, integer(m.Headers["Max-Sessions"]), now)
					i.forward(streamItem{kind: "heartbeat", headers: m.Headers})
				case "CHANNEL_HANGUP_COMPLETE":
					i.addHangup(m.Headers)
				case "CUSTOM":
					now := time.Now()
					i.addSipEvent(m.Headers, now)
					i.gatewayEvent(m.Headers, now)
				}
			case esl.KindState:
				if m.Headers["connected"] == "true" {
					i.seedGateways()
				}
				i.forward(streamItem{kind: "state"})
			}
		case m, ok := <-logs:
			if !ok {
				return
			}
			i.addLog(m)
		}
	}
}

func heartbeatSample(h map[string]string, now time.Time) sample {
	sessions, _ := strconv.Atoi(h["Session-Count"])
	rate, _ := strconv.Atoi(h["Session-Per-Sec-Last"])
	idle, err := strconv.ParseFloat(h["Idle-CPU"], 64)
	// NaN and Inf parse but cannot be encoded as JSON.
	if err != nil || math.IsNaN(idle) || math.IsInf(idle, 0) {
		idle = 0
	}
	return sample{T: now.Unix(), Sessions: sessions, Rate: rate, Idle: idle}
}

func (i *instance) addSample(s sample) {
	i.histMu.Lock()
	defer i.histMu.Unlock()
	i.history[i.histNext] = s
	i.histNext = (i.histNext + 1) % historySize
	i.histLen = min(i.histLen+1, historySize)
}

// setBeat keeps the last heartbeat for the metrics.
func (i *instance) setBeat(s sample, limit int, at time.Time) {
	i.histMu.Lock()
	defer i.histMu.Unlock()
	i.beat, i.beatLimit, i.beatAt = s, limit, at
}

// samples returns the history oldest first; never nil.
func (i *instance) samples() []sample {
	i.histMu.Lock()
	defer i.histMu.Unlock()
	out := make([]sample, 0, i.histLen)
	start := (i.histNext - i.histLen + historySize) % historySize
	for n := 0; n < i.histLen; n++ {
		out = append(out, i.history[(start+n)%historySize])
	}
	return out
}

// handleHistory answers while disconnected too: the history is local state.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request, _ user) {
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"samples": inst.samples()})
}
