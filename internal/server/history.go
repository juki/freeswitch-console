package server

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"fsdeck/internal/esl"
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

// collect records a sample per HEARTBEAT until the switch is closed. It has
// its own subscription and never touches the log-level bookkeeping.
func (i *instance) collect(msgs <-chan esl.Message, cancel func()) {
	defer close(i.collected)
	defer cancel()
	for {
		select {
		case <-i.done:
			return
		case m, ok := <-msgs:
			if !ok {
				return
			}
			if m.Kind == esl.KindEvent && m.Headers["Event-Name"] == "HEARTBEAT" {
				i.addSample(heartbeatSample(m.Headers, time.Now()))
			}
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
